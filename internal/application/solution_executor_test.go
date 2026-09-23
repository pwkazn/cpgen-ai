package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
	"cpgen/internal/workflow"
)

func newSolutionExecutorFixture(t *testing.T, repair bool, response ...http.HandlerFunc) (*similarityExecutorFixture, *application.SolutionExecutor) {
	t.Helper()
	return newSolutionExecutorFixtureWithOutputs(t, repair, llmBuiltinOutputs(t), response...)
}

func newSolutionExecutorFixtureWithOutputs(t *testing.T, repair bool, outputs map[string][]byte, response ...http.HandlerFunc) (*similarityExecutorFixture, *application.SolutionExecutor) {
	t.Helper()
	return newSolutionExecutorFixtureForWorkflow(t, repair, outputs, workflow.LegacySolutionCheckpointRevision, response...)
}

func newSolutionExecutorFixtureForWorkflow(t *testing.T, repair bool, outputs map[string][]byte, revision string, response ...http.HandlerFunc) (*similarityExecutorFixture, *application.SolutionExecutor) {
	t.Helper()
	var requests atomic.Int32
	handler := func(w http.ResponseWriter, _ *http.Request) {
		step := "solution.draft"
		ordinal := requests.Add(1)
		if ordinal == 1 {
			step = "idea.draft"
		} else if ordinal == 2 {
			step = "statement.draft"
		} else if ordinal >= 4 && (revision == workflow.GenerationRevision || revision == workflow.ExecutedSamplesRevision) {
			step = "data.draft"
		}
		content := string(outputs[step])
		if repair && ordinal == 3 {
			content = `{"verified":true}`
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "solution-fixture", "choices": []any{map[string]any{"message": map[string]string{"content": content}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	}
	f := similarityExecutorFixtureFromGeneration(t, newGenerationExecutorFixtureWithWorkflow(t, 6, repair, revision, 2, handler), response...)
	f.config.WorkflowRevision = revision
	var err error
	f.service, err = application.NewSimilarityExecutor(f.config)
	if err != nil {
		t.Fatal(err)
	}
	solution, err := application.NewSolutionExecutor(f.service, f.config.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return f, solution
}

func beginCommittedSolutionVerification(t *testing.T, f *similarityExecutorFixture, service *application.SolutionExecutor) (domain.SolutionDraftInputV1, domain.SolutionContent, domain.RunView) {
	t.Helper()
	ctx := context.Background()
	input, view := f.beginSimilarity(t)
	searched, err := f.service.RunSimilarity(ctx, view, input)
	if err != nil || searched.Outcome.Value == nil {
		t.Fatalf("search=%+v %v", searched, err)
	}
	evidence := searched.Outcome.Value.EvidenceDigest
	f.finish(t, view, evidence, "similarity_decision", evidence, searched.Occurrences)
	prepared, err := service.Reader().ReadInput(ctx, f.runID)
	if err != nil || prepared.Value == nil {
		t.Fatalf("accepted input=%+v %v", prepared, err)
	}
	digest, err := prepared.Value.Digest()
	if err != nil {
		t.Fatal(err)
	}
	decision := f.begin(t, "similarity_decision", evidence, 4)
	f.finish(t, decision, digest, "solution", digest, nil)
	draftView := f.begin(t, "solution", digest, 5)
	draft, err := service.CollectDraft(ctx, draftView, *prepared.Value)
	if err != nil || draft.Outcome.Value == nil {
		t.Fatalf("draft=%+v %v", draft, err)
	}
	content := *draft.Outcome.Value
	f.finish(t, draftView, content.ContentDigest, "solution_verify", content.ContentDigest, draft.Occurrences)
	return *prepared.Value, content, f.begin(t, "solution_verify", content.ContentDigest, 6)
}

func TestSolutionExecutorVerificationRequiresCommittedDraftAndCurrentAttempt(t *testing.T) {
	for _, mode := range []string{"pass", "compile_error", "wrong_answer"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f, service := newSolutionExecutorFixture(t, false)
			calls := 0
			var sandbox *solutionSandboxFixture
			factory := func(_ context.Context, identity port.SandboxAuthorizationIdentity) (application.SolutionSandbox, toolchain.Lock, error) {
				calls++
				if identity.StageName != "solution_verify" || identity.RunID != f.runID || identity.ScopeDigest.Validate() != nil {
					t.Fatal("sandbox scope was not derived from the verification attempt")
				}
				publisher, err := sandboxexec.NewArtifactSink(f.store, f.executorConfig.Blobs, f.clock, identity)
				if err != nil {
					return nil, toolchain.Lock{}, err
				}
				sandbox = &solutionSandboxFixture{publisher: publisher, mode: mode, expected: "0", t: t}
				return sandbox, solutionTestLock(t), nil
			}
			if _, err := service.VerifyDraft(ctx, domain.RunView{}, factory); err == nil || calls != 0 {
				t.Fatal("verification constructed sandbox without committed input")
			}
			input, content, view := beginCommittedSolutionVerification(t, f, service)
			if _, err := f.store.ReadCommittedSandboxStage(ctx, f.runID, "solution_verify"); err == nil {
				t.Fatal("running verification appeared committed")
			}
			result, err := service.VerifyDraft(ctx, view, factory)
			if err != nil || result.Report.Passed != (mode == "pass") || result.Report.ValidateFor(input, content) != nil {
				t.Fatalf("verification=%+v %v", result.Report, err)
			}
			f.finish(t, view, result.ReportArtifact.Blob.Digest, "solution_checkpoint", result.ReportArtifact.Blob.Digest, result.Occurrences)
			stage, err := f.store.ReadCommittedSandboxStage(ctx, f.runID, "solution_verify")
			if err != nil || len(stage.Artifacts) != len(result.Occurrences) || stage.Attempt.OutputDigest == nil || *stage.Attempt.OutputDigest != result.ReportArtifact.Blob.Digest {
				t.Fatalf("committed sandbox stage lost evidence: %v", err)
			}
			if _, err := service.VerifyDraft(ctx, view, factory); err == nil || calls != 1 {
				t.Fatal("completed attempt was allowed to execute verification again")
			}
			stored, err := service.Reader().ReadDraft(ctx, f.runID)
			if err != nil || stored.ContentDigest != content.ContentDigest || f.httpCalls.Load() != 3 || f.sends.Load() != 1 {
				t.Fatalf("verification changed draft provenance or dispatched models: %v", err)
			}
		})
	}
}

func TestSolutionExecutorGeneratesOnlyFromCommittedAcceptanceAndReplays(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "format-repair"}[repair], func(t *testing.T) {
			ctx := context.Background()
			f, service := newSolutionExecutorFixture(t, repair)
			input, view := f.beginSimilarity(t)
			if _, err := service.Reader().ReadInput(ctx, f.runID); err == nil {
				t.Fatal("uncommitted acceptance admitted")
			}
			searched, err := f.service.RunSimilarity(ctx, view, input)
			if err != nil || searched.Outcome.Value == nil {
				t.Fatalf("search=%+v %v", searched, err)
			}
			f.finish(t, view, searched.Outcome.Value.EvidenceDigest, "similarity_decision", searched.Outcome.Value.EvidenceDigest, searched.Occurrences)
			prepared, err := service.Reader().ReadInput(ctx, f.runID)
			if err != nil || prepared.Value == nil {
				t.Fatalf("input=%+v %v", prepared, err)
			}
			digest, err := prepared.Value.Digest()
			if err != nil {
				t.Fatal(err)
			}
			decisionView := f.begin(t, "similarity_decision", searched.Outcome.Value.EvidenceDigest, 4)
			f.finish(t, decisionView, digest, "solution", digest, nil)
			solutionView := f.begin(t, "solution", digest, 5)
			wrong := *prepared.Value
			wrong.SimilarityDecisionDigest = domain.SumBytes([]byte("substitution"))
			if _, err := service.CollectDraft(ctx, solutionView, wrong); err == nil || f.httpCalls.Load() != 2 {
				t.Fatalf("substituted input dispatched: %v HTTP=%d", err, f.httpCalls.Load())
			}
			result, err := service.CollectDraft(ctx, solutionView, *prepared.Value)
			if err != nil || result.Outcome.Value == nil {
				t.Fatalf("solution=%+v %v", result, err)
			}
			wantHTTP := int32(3)
			if repair {
				wantHTTP++
			}
			f.heartbeat(t)
			restarted, err := application.NewSolutionExecutor(f.service, f.config.Generation)
			if err != nil {
				t.Fatal(err)
			}
			replay, err := restarted.CollectDraft(ctx, solutionView, *prepared.Value)
			if err != nil || replay.Outcome.Value == nil || replay.Outcome.Value.ContentDigest != result.Outcome.Value.ContentDigest || f.httpCalls.Load() != wantHTTP || f.sends.Load() != 1 {
				t.Fatalf("replay=%+v %v LLM=%d similarity=%d", replay, err, f.httpCalls.Load(), f.sends.Load())
			}
			f.finish(t, solutionView, result.Outcome.Value.ContentDigest, "solution_verify", result.Outcome.Value.ContentDigest, result.Occurrences)
			stored, err := restarted.Reader().ReadDraft(ctx, f.runID)
			if err != nil || stored.ContentDigest != result.Outcome.Value.ContentDigest || f.httpCalls.Load() != wantHTTP {
				t.Fatalf("committed solution=%+v %v", stored, err)
			}
			quota, err := f.store.ReadMutationBudget(ctx, f.runID, "idea")
			if err != nil || quota.Claimed != 0 {
				t.Fatalf("solution consumed mutation: %+v %v", quota, err)
			}
		})
	}
}

func TestSolutionExecutorNonAcceptedBusinessEvidenceRequiresReview(t *testing.T) {
	for _, score := range []string{"0.6", "0.95", "empty"} {
		t.Run(score, func(t *testing.T) {
			ctx := context.Background()
			f, service := newSolutionExecutorFixture(t, false, func(w http.ResponseWriter, _ *http.Request) {
				hits := `[{"source":"fixture","external_id":"one","score":` + score + `}]`
				if score == "empty" {
					hits = "[]"
				}
				_, _ = w.Write([]byte(`{"provider_identity":"fixture","hits":` + hits + `,"usage":{"input_tokens":2,"output_tokens":3,"cost_micro_usd":11}}`))
			})
			input, view := f.beginSimilarity(t)
			searched, err := f.service.RunSimilarity(ctx, view, input)
			if err != nil || searched.Outcome.Value == nil {
				t.Fatalf("search=%+v %v", searched, err)
			}
			f.finish(t, view, searched.Outcome.Value.EvidenceDigest, "similarity_decision", searched.Outcome.Value.EvidenceDigest, searched.Occurrences)
			for replay := 0; replay < 2; replay++ {
				result, err := service.Reader().ReadInput(ctx, f.runID)
				if err != nil || result.Review == nil || result.Value != nil || f.httpCalls.Load() != 2 || f.sends.Load() != 1 {
					t.Fatalf("review=%+v %v", result, err)
				}
			}
			quota, err := f.store.ReadMutationBudget(ctx, f.runID, "idea")
			if err != nil || quota.Claimed != 0 {
				t.Fatalf("review consumed mutation: %+v %v", quota, err)
			}
		})
	}
}
