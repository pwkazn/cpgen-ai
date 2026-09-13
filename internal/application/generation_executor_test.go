package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/agent"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

func TestGenerationExecutorCommitsRepairedTypedChainAndReplaysWithoutHTTP(t *testing.T) {
	f := newGenerationExecutorFixture(t, 4, true)
	ctx := context.Background()
	view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	idea, err := f.executor.RunIdea(ctx, view, f.snapshot)
	if err != nil || idea.Outcome.Value == nil || len(idea.Occurrences) != 2 || len(idea.CallTraces) != 2 || idea.Usage != (port.Usage{InputTokens: 6, OutputTokens: 8}) {
		t.Fatalf("idea=%+v err=%v", idea, err)
	}
	if err := idea.PublishCache(ctx); err == nil {
		t.Fatal("uncommitted response entered cache")
	}
	// Reconstruct the executor and advance the heartbeat version. The exact
	// opening time and logical call identities must remain recoverable.
	f.heartbeat(t)
	second, err := application.NewGenerationExecutor(f.executorConfig)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := second.RunIdea(ctx, view, f.snapshot)
	if err != nil || replayed.Outcome.Value == nil || replayed.Outcome.Value.BatchDigest != idea.Outcome.Value.BatchDigest || f.httpCalls.Load() != 2 {
		t.Fatalf("replayed=%+v err=%v HTTP=%d", replayed, err, f.httpCalls.Load())
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
	input := domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: f.snapshot.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}
	digest, err := input.Digest()
	if err != nil {
		t.Fatal(err)
	}
	f.finish(t, view, batch.BatchDigest, "statement", digest, idea.Occurrences)
	if err := idea.PublishCache(ctx); err != nil {
		t.Fatal(err)
	}
	statementView := f.begin(t, "statement", digest, 2)
	statement, err := f.executor.RunStatement(ctx, statementView, input)
	if err != nil || statement.Outcome.Value == nil || len(statement.Occurrences) != 2 || statement.Outcome.Value.ValidateChain(f.snapshot, batch, selection) != nil || f.httpCalls.Load() != 4 {
		t.Fatalf("statement=%+v err=%v HTTP=%d", statement, err, f.httpCalls.Load())
	}
	f.finish(t, statementView, statement.Outcome.Value.SpecDigest, "similarity", domain.SumBytes([]byte("next similarity input")), statement.Occurrences)
	if err := statement.PublishCache(ctx); err != nil {
		t.Fatal(err)
	}
	content, err := f.executor.Reader().ReadStatement(ctx, f.runID)
	if err != nil || content.Problem.SpecDigest != statement.Outcome.Value.SpecDigest || f.httpCalls.Load() != 4 {
		t.Fatalf("committed=%+v err=%v HTTP=%d", content, err, f.httpCalls.Load())
	}
}

func TestGenerationExecutorRejectsUnmeteredStaleAndSubstitutedInputBeforeDispatch(t *testing.T) {
	f := newGenerationExecutorFixture(t, 4, false)
	ctx := context.Background()
	view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	bad, err := domain.NewGenerationRequestSnapshotV1(f.snapshot.Request, f.snapshot.EffectiveSeed+1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.executor.RunIdea(ctx, view, bad); err == nil || f.httpCalls.Load() != 0 {
		t.Fatalf("substituted input dispatched: %v", err)
	}
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.active.Stop(ctx, f.runID, current.Version); err != nil {
		t.Fatal(err)
	}
	if _, err := f.executor.RunIdea(ctx, view, f.snapshot); err == nil || f.httpCalls.Load() != 0 {
		t.Fatalf("unmetered work dispatched: %v", err)
	}
	current, err = f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.InterruptStage(ctx, domain.InterruptStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "idea", AttemptID: view.AttemptID(), Cause: domain.CauseStepDeadline, IdempotencyKey: coordinatorID("interrupt", "executor"), At: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = f.begin(t, "idea", f.snapshot.SnapshotDigest, 2)
	if _, err := f.executor.RunIdea(ctx, view, f.snapshot); err == nil || f.httpCalls.Load() != 0 {
		t.Fatalf("obsolete view dispatched: %v", err)
	}
}

func TestGenerationExecutorBudgetFailureIsReviewWithoutContentRepair(t *testing.T) {
	f := newGenerationExecutorFixture(t, 0, true)
	view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	result, err := f.executor.RunIdea(context.Background(), view, f.snapshot)
	if err != nil || result.Outcome.Review == nil || result.Outcome.Review.Reason != "llm_budget_exhausted" || len(result.Occurrences) != 0 || f.httpCalls.Load() != 0 {
		t.Fatalf("budget result=%+v err=%v HTTP=%d", result, err, f.httpCalls.Load())
	}
}

func TestGenerationExecutorMapsProviderFailuresWithoutFormatRepairOrReplaySend(t *testing.T) {
	for _, test := range []struct {
		name    string
		respond http.HandlerFunc
		blocked bool
		reason  string
	}{
		{"unavailable", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }, true, ""},
		{"rejected", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadRequest) }, false, "llm_content_rejected"},
		{"unknown", func(w http.ResponseWriter, _ *http.Request) {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
		}, false, "llm_boundary_unknown"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newGenerationExecutorFixture(t, 4, true, test.respond)
			view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
			for replay := 0; replay < 2; replay++ {
				result, err := f.executor.RunIdea(context.Background(), view, f.snapshot)
				if err != nil || result.Outcome.Validate() != nil || len(result.Occurrences) != 0 || f.httpCalls.Load() != 1 {
					t.Fatalf("result=%+v err=%v HTTP=%d", result, err, f.httpCalls.Load())
				}
				if test.blocked {
					if result.Outcome.Blocked == nil || result.Outcome.Blocked.StageInputDigest != f.snapshot.SnapshotDigest || result.Outcome.Blocked.PolicyDigest != f.options.ProviderPolicyDigest {
						t.Fatalf("blocked=%+v", result.Outcome)
					}
				} else if result.Outcome.Review == nil || result.Outcome.Review.Reason != test.reason {
					t.Fatalf("review=%+v", result.Outcome)
				}
			}
		})
	}
}

func TestGenerationExecutorReusesCommittedPriorAttemptAtZeroRemainingCallBudget(t *testing.T) {
	testGenerationExecutorPriorAttemptCache(t, false)
}

func TestGenerationExecutorBlockedPredecessorRequiresFreshDependencyEvenWithCache(t *testing.T) {
	testGenerationExecutorPriorAttemptCache(t, true)
}

func testGenerationExecutorPriorAttemptCache(t *testing.T, recheck bool) {
	maxCalls := int64(1)
	if recheck {
		maxCalls = 2
	}
	content := string(llmBuiltinOutputs(t)["idea.draft"])
	f := newGenerationExecutorFixture(t, maxCalls, false, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "cached-dependency-fixture", "choices": []any{map[string]any{"message": map[string]string{"content": content}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	})
	ctx := context.Background()
	view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	first, err := f.executor.RunIdea(ctx, view, f.snapshot)
	if err != nil || first.Outcome.Value == nil {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	batch := *first.Outcome.Value
	ids, err := batch.OrderedFeasibleCandidateIDs(f.options.SelectionPolicy)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := domain.NewIdeaSelection(f.snapshot.RequestDigest, batch, ids[0], f.options.SelectionPolicy, []string{"deterministic_selection"}, []domain.Digest{batch.BatchDigest})
	if err != nil {
		t.Fatal(err)
	}
	statementInput := domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: f.snapshot.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}
	input, err := statementInput.Digest()
	if err != nil {
		t.Fatal(err)
	}
	f.finish(t, view, batch.BatchDigest, "statement", input, first.Occurrences)
	if err := first.PublishCache(ctx); err != nil {
		t.Fatal(err)
	}
	statement := f.begin(t, "statement", input, 2)
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
	evidence, policy := domain.SumBytes([]byte("recheck current draft")), f.options.ProviderPolicyDigest
	review, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "statement", AttemptID: statement.AttemptID(), AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview, ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &policy, IdempotencyKey: coordinatorID("finish", "executor-review"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	edits := domain.SumBytes([]byte("recheck the same frozen input"))
	decision, err := f.store.CreateReview(ctx, domain.CreateReviewRequest{ID: "review_00000000000000000000000000001601", RunID: f.runID, ExpectedRunVersion: review.Version, Kind: domain.ReviewRevise, WorkflowRevision: workflow.LegacySimilarityRevision, StageName: "statement", StageInputDigest: input, EvidenceDigest: evidence, PolicyDigest: policy, RequestedEditsDigest: &edits, Reviewer: "fixture", Reason: "recheck existing input", IdempotencyKey: coordinatorID("review", "executor"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	_, configJSON, err := f.store.RunViewDocuments(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.ApplyReview(ctx, domain.ApplyReviewCommand{RunID: f.runID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID, StageName: "statement", StageInputDigest: input, EvidenceDigest: evidence, PolicyDigest: policy, NewInputDigest: &f.snapshot.SnapshotDigest, NewConfigJSON: configJSON, NewConfigDigest: &policy, InvalidatedStages: []domain.StageName{"idea", "statement", "similarity"}, IdempotencyKey: coordinatorID("apply", "executor-review"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	view = f.begin(t, "idea", f.snapshot.SnapshotDigest, 3)
	wantHTTP := int32(1)
	wantOccurrence := domain.PendingOccurrenceCacheReuse
	if recheck {
		// The cache source survives review invalidation. A subsequent BLOCKED
		// predecessor must nevertheless authorize fresh dependency work, even
		// when recovery occurs after BeginStage and before opening the call.
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
		at := f.clock.Now()
		binding := domain.BlockedCheckpoint{RunID: f.runID, StageName: "idea", StageInputDigest: f.snapshot.SnapshotDigest, DependencyID: "llm:fixture", DependencyDigest: domain.SumBytes([]byte("dependency")), PolicyDigest: f.options.ProviderPolicyDigest, ErrorDigest: domain.SumBytes([]byte("dependency unavailable")), RetryAfter: at, CreatedAt: at}
		if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "idea", AttemptID: view.AttemptID(), AttemptState: domain.StageAttemptBlocked, RunState: domain.RunBlocked, BlockedBinding: &binding, IdempotencyKey: coordinatorID("finish", "cache-blocked"), At: at}); err != nil {
			t.Fatal(err)
		}
		obsolete := view
		view = f.begin(t, "idea", f.snapshot.SnapshotDigest, 4)
		if _, err := f.store.ReadAttemptDependencyCheckpoint(ctx, f.runID, "idea", obsolete.AttemptID()); err == nil {
			t.Fatal("obsolete attempt can read current dependency admission")
		}
		persisted, err := f.store.ReadAttemptDependencyCheckpoint(ctx, f.runID, "idea", view.AttemptID())
		if err != nil || persisted == nil || *persisted != binding {
			t.Fatalf("checkpoint=%+v %v", persisted, err)
		}
		f.executor, err = application.NewGenerationExecutor(f.executorConfig)
		if err != nil {
			t.Fatal(err)
		}
		wantHTTP, wantOccurrence = 2, domain.PendingOccurrenceNewWrite
	}
	for replay := 0; replay < 2; replay++ {
		hit, err := f.executor.RunIdea(ctx, view, f.snapshot)
		if err != nil || hit.Outcome.Value == nil || hit.Outcome.Value.BatchDigest != batch.BatchDigest || len(hit.Occurrences) != 1 || hit.Occurrences[0].Kind != wantOccurrence || (!recheck && hit.Usage != (port.Usage{})) || f.httpCalls.Load() != wantHTTP {
			t.Fatalf("hit=%+v err=%v HTTP=%d", hit, err, f.httpCalls.Load())
		}
		if replay == 1 {
			f.finish(t, view, batch.BatchDigest, "statement", input, hit.Occurrences)
		}
	}
	idea, err := f.executor.Reader().ReadIdea(ctx, f.runID)
	if err != nil || idea.Batch.BatchDigest != batch.BatchDigest || f.httpCalls.Load() != wantHTTP {
		t.Fatalf("read=%+v err=%v", idea, err)
	}
}

type generationExecutorFixture struct {
	coordinatorFixture
	executor                     *application.GenerationExecutor
	executorConfig               application.GenerationExecutorConfig
	options                      application.GenerationReaderOptions
	snapshot                     domain.GenerationRequestSnapshotV1
	httpCalls                    *atomic.Int32
	active                       *application.ActiveTime
	runGuard                     *runlock.Guard
	endpoint, blobRoot, lockRoot string
}

func newGenerationExecutorFixture(t *testing.T, maxCalls int64, repair bool, respond ...http.HandlerFunc) generationExecutorFixture {
	t.Helper()
	return newGenerationExecutorFixtureWithWorkflow(t, maxCalls, repair, workflow.LegacySimilarityRevision, 0, respond...)
}

func newGenerationExecutorFixtureWithWorkflow(t *testing.T, maxCalls int64, repair bool, revision string, similarityCalls int64, respond ...http.HandlerFunc) generationExecutorFixture {
	t.Helper()
	cfg := llmApplicationConfig(t)
	if repair {
		cfg.LLM.MaxFormatRepairs = 1
	} else {
		cfg.LLM.MaxFormatRepairs = 0
	}
	t.Setenv(cfg.LLM.APIKeyEnv, "fixture-key")
	outputs := llmBuiltinOutputs(t)
	httpCalls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ordinal := httpCalls.Add(1)
		if len(respond) != 0 {
			respond[0](w, r)
			return
		}
		content := string(outputs["idea.draft"])
		if repair {
			if ordinal%2 == 1 {
				content = `{"private":"invalid output"}`
			} else if ordinal == 4 {
				content = string(outputs["statement.draft"])
			}
		} else if ordinal == 2 {
			content = string(outputs["statement.draft"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "generation-executor", "choices": []any{map[string]any{"message": map[string]string{"content": content}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	}))
	t.Cleanup(server.Close)
	mapped, _, err := application.BuildLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mapped.Endpoint, mapped.AllowInsecureHTTP = server.URL, true
	model, err := agent.NewLangChain(mapped)
	if err != nil {
		t.Fatal(err)
	}
	limits := domain.BudgetLimits{MaxLLMCalls: maxCalls, MaxLLMInputTokens: 500000, MaxLLMOutputTokens: 10000, MaxLLMCostMicroUSD: 1000, MaxArtifactBytes: 1000000, MaxActiveTimeMilliseconds: 60000}
	limits.MaxSimilarityCalls, limits.MaxSimilarityCostMicroUSD = similarityCalls, similarityCalls*100
	if workflow.HasSolutionStages(revision) {
		limits.MaxArtifactBytes, limits.MaxSandboxCreates, limits.MaxActiveTimeMilliseconds = 64<<20, 100, 180000
	}
	if revision == workflow.GenerationRevision {
		limits.MaxActiveTimeMilliseconds = 600000
		limits.MaxPackageBytes = 16 << 20
	}
	snapshot, err := domain.NewGenerationRequestSnapshotV1(domain.GenerationRequestV1{SchemaVersion: domain.RequestSchemaV1, Mode: domain.RequestModeManual, Brief: "Graphs", Tags: []string{"graphs"}, NormalizedTags: []string{"graphs"}, Language: "en", Difficulty: "hard", TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: limits}, 9007199254740993)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "generation.db")
	clock := newRecordingClock(time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC))
	if workflow.HasSolutionStages(revision) {
		clock = newRecordingClock(time.Now().UTC())
	}
	store, err := openFreshApplicationSQLite(t, sqlite.Config{Path: path, BusyTimeout: time.Second, MaxReaders: 4}, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	blobRoot := filepath.Join(t.TempDir(), "private")
	blobs, err := blob.NewStore(blobRoot)
	if err != nil {
		t.Fatal(err)
	}
	lockRoot := t.TempDir()
	locks, err := runlock.NewManager(lockRoot, runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locks.Close() })
	f := coordinatorFixture{store: store, path: path, clock: clock, runID: "run_00000000000000000000000000001601"}
	guard, err := locks.AcquireRun(context.Background(), f.runID, runlock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = guard.Close() })
	artifactGuard, err := locks.AcquireArtifacts(context.Background(), runlock.Shared)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = artifactGuard.Close() })
	raw, err := snapshot.Request.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	configJSON, err := cfg.Effective()
	if err != nil {
		t.Fatal(err)
	}
	stages := []domain.StageName{"idea", "statement", "similarity"}
	if revision == workflow.LegacySimilarityCheckpointRevision {
		stages = append(stages, "slice2_checkpoint")
	}
	if revision == workflow.LegacySolutionCheckpointRevision {
		stages = append(stages, "similarity_decision", "solution", "solution_verify", "solution_checkpoint")
	}
	if revision == workflow.GenerationRevision {
		stages = append(stages, "similarity_decision", "solution", "solution_verify", "solution_decision", "data", "data_verify", "judge", "quality", "package")
	}
	_, err = store.CreateRun(context.Background(), domain.CreateRunRequest{RunID: f.runID, SubmittedRequestJSON: raw, SubmittedRequestDigest: snapshot.RequestDigest, EffectiveSeed: snapshot.EffectiveSeed, RedactedEffectiveConfigJSON: configJSON, RedactedEffectiveConfigDigest: cfg.EffectiveDigest(), WorkflowRevision: revision, SchemaVersion: domain.RequestSchemaV1, WorkflowDigest: domain.SumBytes([]byte(revision)), BudgetLimits: limits, StageSequence: stages, CreatedAt: clock.Now(), IdempotencyKey: coordinatorID("create", "generation-executor")})
	if err != nil {
		t.Fatal(err)
	}
	options := application.GenerationReaderOptions{IdeaCount: 2, SelectionPolicy: domain.SelectionOrdinalPolicyV1, StatementRevision: 1, ProviderPolicyDigest: cfg.EffectiveDigest(), Sampling: port.SamplingPolicy{TopP: 1}, MaxOutput: port.OutputLimit{Tokens: 512, Bytes: 16384}}
	ideaRepair, err := application.BuildFormatRepairPolicy(cfg, "idea.draft")
	if err != nil {
		t.Fatal(err)
	}
	statementRepair, err := application.BuildFormatRepairPolicy(cfg, "statement.draft")
	if err != nil {
		t.Fatal(err)
	}
	executorConfig := application.GenerationExecutorConfig{Store: store, Blobs: blobs, LLM: model, Clock: clock, Locks: locks, Content: options, IdeaRepair: ideaRepair, StatementRepair: statementRepair, RetryPolicy: domain.RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, JitterSeedDigest: domain.SumBytes([]byte("generation retry"))}, CostUpperBoundMicroUSD: 100}
	executorConfig.SolutionRepair, err = application.BuildFormatRepairPolicy(cfg, "solution.draft")
	if err != nil {
		t.Fatal(err)
	}
	executorConfig.DataRepair, err = application.BuildFormatRepairPolicy(cfg, "data.draft")
	if err != nil {
		t.Fatal(err)
	}
	executor, err := application.NewGenerationExecutor(executorConfig)
	if err != nil {
		t.Fatal(err)
	}
	active, err := application.NewActiveTime(store, clock, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	return generationExecutorFixture{coordinatorFixture: f, executor: executor, executorConfig: executorConfig, options: options, snapshot: snapshot, httpCalls: httpCalls, active: active, runGuard: guard, endpoint: server.URL, blobRoot: blobRoot, lockRoot: lockRoot}
}

func (f generationExecutorFixture) begin(t *testing.T, stage domain.StageName, input domain.Digest, ordinal int) domain.RunView {
	t.Helper()
	ctx := context.Background()
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf("%s-%d", stage, ordinal)
	attemptID := domain.AttemptID(coordinatorID("attempt", identity))
	_, err = f.store.BeginStage(ctx, domain.BeginStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: stage, AttemptID: attemptID, InputDigest: input, IdempotencyKey: coordinatorID("begin", identity), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	current, err = f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.active.Start(ctx, f.runID, current.Version); err != nil {
		t.Fatal(err)
	}
	current, err = f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	budget, err := f.store.BudgetSnapshot(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := domain.NewRunView(domain.RunViewData{RunID: f.runID, AttemptID: attemptID, WorkflowRevision: current.WorkflowRevision, SchemaVersion: current.SchemaVersion, RequestDigest: current.RequestDigest, ConfigDigest: current.ConfigDigest, WorkflowDigest: current.WorkflowDigest, State: current.State, CurrentStage: stage, Version: current.Version, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	return view
}

func (f generationExecutorFixture) heartbeat(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.active.Heartbeat(ctx, f.runID, current.Version); err != nil {
		t.Fatal(err)
	}
}

func (f generationExecutorFixture) finish(t *testing.T, view domain.RunView, output domain.Digest, next domain.StageName, nextInput domain.Digest, occurrences []domain.PendingOccurrence) {
	t.Helper()
	ctx := context.Background()
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
	_, err = f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: view.CurrentStage(), AttemptID: view.AttemptID(), AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: next, NextInputDigest: &nextInput, Occurrences: occurrences, IdempotencyKey: coordinatorID("finish", string(view.AttemptID())), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
}
