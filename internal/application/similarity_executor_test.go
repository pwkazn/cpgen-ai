package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

func TestSimilarityExecutorCommitsTypedEvidenceAndReconstructsWithoutHTTP(t *testing.T) {
	f := newSimilarityExecutorFixture(t, 3)
	ctx := context.Background()
	input, view := f.beginSimilarity(t)
	if _, err := f.service.ReadCommitted(ctx, f.runID); err == nil {
		t.Fatal("uncommitted evidence was readable")
	}
	first, err := f.service.RunSimilarity(ctx, view, input)
	if err != nil || first.Outcome.Value == nil || first.Decision == nil || first.Decision.Kind != similarity.DecisionAccept || len(first.Occurrences) != 1 || f.sends.Load() != 1 {
		t.Fatalf("first=%+v err=%v HTTP=%d", first, err, f.sends.Load())
	}
	f.heartbeat(t)
	restarted, err := application.NewSimilarityExecutor(f.config)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := restarted.RunSimilarity(ctx, view, input)
	if err != nil || !reflect.DeepEqual(first, replayed) || f.sends.Load() != 1 {
		t.Fatalf("replay changed receipt: %v HTTP=%d", err, f.sends.Load())
	}
	f.finish(t, view, first.Outcome.Value.EvidenceDigest, "slice2_checkpoint", first.Outcome.Value.EvidenceDigest, first.Occurrences)
	content, err := restarted.ReadCommitted(ctx, f.runID)
	if err != nil || !reflect.DeepEqual(content.Evidence, *first.Outcome.Value) || content.Input != input || content.Decision != *first.Decision || f.sends.Load() != 1 || f.httpCalls.Load() != 2 {
		t.Fatalf("committed=%+v err=%v similarity HTTP=%d generation HTTP=%d", content, err, f.sends.Load(), f.httpCalls.Load())
	}
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil || current.State != domain.RunRunning || current.CurrentStage != "slice2_checkpoint" {
		t.Fatalf("evidence collection bypassed the unfinished boundary: %+v %v", current, err)
	}
	// The legacy reader still requires the wire request as the stage input;
	// callers must deliberately choose the typed semantic-input API.
	reader, err := application.NewSimilarityReader(f.store, f.executorConfig.Blobs, f.config.Provider)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(ctx, f.runID, "similarity", content.Request); err == nil {
		t.Fatal("legacy input contract silently accepted a semantic digest")
	}
	bad := content.Request
	bad.Limit++
	if _, err := reader.ReadTyped(ctx, f.runID, "similarity", input, bad); err == nil || f.sends.Load() != 1 {
		t.Fatal("substituted typed wire request was accepted or sent")
	}
}

func TestSimilarityExecutorRejectsSubstitutedInputAndPolicyBeforeDispatch(t *testing.T) {
	f := newSimilarityExecutorFixture(t, 3)
	input, view := f.beginSimilarity(t)
	ctx := context.Background()
	for _, change := range []func(*domain.SimilarityInputV1){
		func(i *domain.SimilarityInputV1) { i.ProblemSpecDigest = domain.SumBytes([]byte("different problem")) },
		func(i *domain.SimilarityInputV1) {
			i.CandidateProjectionDigest = domain.SumBytes([]byte("different query"))
		},
		func(i *domain.SimilarityInputV1) {
			i.ExecutionPolicyDigest = domain.SumBytes([]byte("different execution"))
		},
	} {
		bad := input
		change(&bad)
		if _, err := f.service.RunSimilarity(ctx, view, bad); err == nil {
			t.Fatal("substituted input admitted")
		}
	}
	for _, change := range []func(*application.SimilarityExecutorConfig){
		func(c *application.SimilarityExecutorConfig) {
			c.Provider = changedSimilarityExecutorPolicy{c.Provider}
		},
		func(c *application.SimilarityExecutorConfig) { c.Limit++ },
		func(c *application.SimilarityExecutorConfig) { c.CostUpperBoundMicroUSD++ },
		func(c *application.SimilarityExecutorConfig) { c.RetryPolicy.MaxAttempts++ },
		func(c *application.SimilarityExecutorConfig) {
			c.Policy, _ = similarity.NewPolicy("changed/v1", .4, .9, .4, .9, 1, nil)
		},
	} {
		config := f.config
		change(&config)
		changed, err := application.NewSimilarityExecutor(config)
		if err != nil {
			t.Fatal(err)
		}
		statement, err := f.executor.Reader().ReadStatement(ctx, f.runID)
		if err != nil {
			t.Fatal(err)
		}
		altered, err := changed.InputForProblem(statement.Problem)
		if err != nil || altered.ExecutionPolicyDigest == input.ExecutionPolicyDigest {
			t.Fatalf("unbound execution setting: %v", err)
		}
		if _, err := changed.ReadInput(ctx, f.runID); err == nil {
			t.Fatal("changed policy reinterpreted committed input")
		}
		if _, err := changed.RunSimilarity(ctx, view, input); err == nil {
			t.Fatal("changed policy dispatched")
		}
	}
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.active.Stop(ctx, f.runID, current.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.RunSimilarity(ctx, view, input); err == nil {
		t.Fatal("unmetered execution admitted")
	}
	if f.sends.Load() != 0 || f.httpCalls.Load() != 2 {
		t.Fatal("read/planning or rejected input dispatched HTTP")
	}
}

// This provider changes only the physical policy identity. It exercises input
// admission without requiring a second HTTP service or issuing any request.
type changedSimilarityExecutorPolicy struct{ similarity.PhysicalProvider }

func (p changedSimilarityExecutorPolicy) PlanSearch(request similarity.Request) (similarity.PhysicalSearchPlan, error) {
	plan, err := p.PhysicalProvider.PlanSearch(request)
	plan.PolicyDigest = domain.SumBytes([]byte("changed physical provider policy"))
	return plan, err
}

func TestSimilarityExecutorRetainsNonAcceptDecisionsWithCommittedEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		decision similarity.DecisionKind
	}{
		{"review", `{"provider_identity":"fixture","hits":[{"source":"fixture","external_id":"one","score":0.6}]}`, similarity.DecisionNeedsReview},
		{"reject", `{"provider_identity":"fixture","hits":[{"source":"fixture","external_id":"one","score":0.9}]}`, similarity.DecisionReject},
		{"insufficient", `{"provider_identity":"fixture","hits":[]}`, similarity.DecisionBlocked},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSimilarityExecutorFixture(t, 1, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(test.body)) })
			input, view := f.beginSimilarity(t)
			result, err := f.service.RunSimilarity(context.Background(), view, input)
			if err != nil || result.Outcome.Value == nil || result.Decision == nil || result.Decision.Kind != test.decision || len(result.Occurrences) != 1 {
				t.Fatalf("collection lost policy evidence: %+v %v", result, err)
			}
			f.finish(t, view, result.Outcome.Value.EvidenceDigest, "slice2_checkpoint", result.Outcome.Value.EvidenceDigest, result.Occurrences)
			committed, err := f.service.ReadCommitted(context.Background(), f.runID)
			if err != nil || committed.Decision != *result.Decision || committed.Decision.EvidenceDigest != committed.Evidence.EvidenceDigest || f.sends.Load() != 1 {
				t.Fatalf("decision reinterpreted after commit: %+v %v", committed, err)
			}
		})
	}
}

func TestSimilarityExecutorBlockedRetryUsesNewAttemptEvidenceIdentity(t *testing.T) {
	var response atomic.Int32
	f := newSimilarityExecutorFixture(t, 3, func(w http.ResponseWriter, r *http.Request) {
		if response.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		similarityExecutorSuccess(w, r)
	})
	ctx := context.Background()
	input, firstView := f.beginSimilarity(t)
	var first application.SimilarityStageResult
	for replay := 0; replay < 2; replay++ {
		var err error
		first, err = f.service.RunSimilarity(ctx, firstView, input)
		if err != nil || first.Outcome.Blocked == nil || first.Outcome.Validate() != nil || len(first.Occurrences) != 0 || f.sends.Load() != 1 {
			t.Fatalf("blocked=%+v err=%v HTTP=%d", first, err, f.sends.Load())
		}
	}
	binding := first.Outcome.Blocked
	digest, _ := input.Digest()
	if binding.StageInputDigest != digest || binding.PolicyDigest != input.ExecutionPolicyDigest {
		t.Fatal("blocked retry lost semantic bindings")
	}
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.active.Stop(ctx, f.runID, current.Version); err != nil {
		t.Fatal(err)
	}
	current, err = f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "similarity", AttemptID: firstView.AttemptID(), AttemptState: domain.StageAttemptBlocked, RunState: domain.RunBlocked, BlockedBinding: binding, IdempotencyKey: coordinatorID("finish", "similarity-blocked"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	secondView := f.begin(t, "similarity", digest, 4)
	if _, err := f.service.RunSimilarity(ctx, firstView, input); err == nil {
		t.Fatal("old attempt remained authorized")
	}
	second, err := f.service.RunSimilarity(ctx, secondView, input)
	if err != nil || second.Outcome.Value == nil || f.sends.Load() != 2 {
		t.Fatalf("new attempt=%+v err=%v", second, err)
	}
	f.keysMu.Lock()
	keys := append([]string(nil), f.keys...)
	f.keysMu.Unlock()
	if len(keys) != 2 || keys[0] == "" || keys[0] == keys[1] || first.CallTrace.LogicalOperationID == second.CallTrace.LogicalOperationID {
		t.Fatalf("new attempt reused old wire identity: %v", keys)
	}
	f.finish(t, secondView, second.Outcome.Value.EvidenceDigest, "slice2_checkpoint", second.Outcome.Value.EvidenceDigest, second.Occurrences)
	if _, err := f.service.ReadCommitted(ctx, f.runID); err != nil || f.sends.Load() != 2 {
		t.Fatalf("new attempt committed read: %v", err)
	}
}

func TestSimilarityExecutorFailureMappingAndBudgetDoNotRepairOrResend(t *testing.T) {
	for _, test := range []struct {
		name    string
		budget  int64
		respond http.HandlerFunc
		reason  string
		calls   int32
	}{
		{"budget", 0, similarityExecutorSuccess, "similarity_budget_exhausted", 0},
		{"rejected", 3, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }, "similarity_content_rejected", 1},
		{"unknown", 3, func(w http.ResponseWriter, _ *http.Request) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}, "similarity_boundary_unknown", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSimilarityExecutorFixture(t, test.budget, test.respond)
			input, view := f.beginSimilarity(t)
			for replay := 0; replay < 2; replay++ {
				result, err := f.service.RunSimilarity(context.Background(), view, input)
				if err != nil || result.Outcome.Review == nil || result.Outcome.Review.Reason != test.reason || result.Outcome.Review.PolicyDigest != input.ExecutionPolicyDigest || len(result.Occurrences) != 0 || result.Decision != nil || f.sends.Load() != test.calls || f.httpCalls.Load() != 2 {
					t.Fatalf("result=%+v err=%v similarity HTTP=%d generation HTTP=%d", result, err, f.sends.Load(), f.httpCalls.Load())
				}
			}
		})
	}
}

type similarityExecutorFixture struct {
	generationExecutorFixture
	service  *application.SimilarityExecutor
	config   application.SimilarityExecutorConfig
	sends    *atomic.Int32
	keysMu   *sync.Mutex
	keys     []string
	endpoint string
}

func newSimilarityExecutorFixture(t *testing.T, maxCalls int64, respond ...http.HandlerFunc) *similarityExecutorFixture {
	t.Helper()
	return similarityExecutorFixtureFromGeneration(t, newGenerationExecutorFixtureWithWorkflow(t, 4, false, workflow.LegacySimilarityCheckpointRevision, maxCalls), respond...)
}

func similarityExecutorFixtureFromGeneration(t *testing.T, generation generationExecutorFixture, respond ...http.HandlerFunc) *similarityExecutorFixture {
	t.Helper()
	f := &similarityExecutorFixture{generationExecutorFixture: generation, sends: new(atomic.Int32), keysMu: new(sync.Mutex)}
	t.Setenv("CPGEN_DURABLE_SIMILARITY_KEY", "fixture-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.sends.Add(1)
		f.keysMu.Lock()
		f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
		f.keysMu.Unlock()
		if len(respond) != 0 {
			respond[0](w, r)
			return
		}
		similarityExecutorSuccess(w, r)
	}))
	t.Cleanup(server.Close)
	f.endpoint = server.URL
	provider, _ := durableSimilarityProvider(t, server.URL)
	policy, err := similarity.NewPolicy("durable/v1", .5, .8, .5, .8, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.config = application.SimilarityExecutorConfig{Generation: f.executor, Provider: provider, Policy: policy, Limit: 10, RetryPolicy: f.executorConfig.RetryPolicy, CostUpperBoundMicroUSD: 100}
	f.service, err = application.NewSimilarityExecutor(f.config)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func similarityExecutorSuccess(w http.ResponseWriter, _ *http.Request) {
	_, _ = w.Write([]byte(`{"provider_identity":"fixture","hits":[{"source":"fixture","external_id":"one","score":0.2}],"usage":{"input_tokens":2,"output_tokens":3,"cost_micro_usd":11}}`))
}

func (f *similarityExecutorFixture) beginSimilarity(t *testing.T) (domain.SimilarityInputV1, domain.RunView) {
	t.Helper()
	ctx := context.Background()
	ideaView := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	idea, err := f.executor.RunIdea(ctx, ideaView, f.snapshot)
	if err != nil || idea.Outcome.Value == nil {
		t.Fatalf("idea=%+v err=%v", idea, err)
	}
	batch := *idea.Outcome.Value
	ids, err := batch.OrderedFeasibleCandidateIDs(f.options.SelectionPolicy)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := domain.NewIdeaSelection(f.snapshot.RequestDigest, batch, ids[0], f.options.SelectionPolicy, []string{"deterministic_selection"}, []domain.Digest{batch.BatchDigest})
	if err != nil {
		t.Fatal(err)
	}
	statementInput := domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: f.snapshot.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}
	digest, err := statementInput.Digest()
	if err != nil {
		t.Fatal(err)
	}
	f.finish(t, ideaView, batch.BatchDigest, "statement", digest, idea.Occurrences)
	statementView := f.begin(t, "statement", digest, 2)
	statement, err := f.executor.RunStatement(ctx, statementView, statementInput)
	if err != nil || statement.Outcome.Value == nil {
		t.Fatalf("statement=%+v err=%v", statement, err)
	}
	input, err := f.service.InputForProblem(*statement.Outcome.Value)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := input.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip domain.SimilarityInputV1
	if err := json.Unmarshal(raw, &roundTrip); err != nil || roundTrip != input {
		t.Fatalf("input round trip: %v", err)
	}
	digest, err = input.Digest()
	if err != nil {
		t.Fatal(err)
	}
	f.finish(t, statementView, statement.Outcome.Value.SpecDigest, "similarity", digest, statement.Occurrences)
	loaded, err := f.service.ReadInput(ctx, f.runID)
	if err != nil || loaded != input || f.sends.Load() != 0 || f.httpCalls.Load() != 2 {
		t.Fatalf("input reconstruction=%+v err=%v", loaded, err)
	}
	return input, f.begin(t, "similarity", digest, 3)
}
