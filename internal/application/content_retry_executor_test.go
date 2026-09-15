package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestContentRetryMakesFreshMeteredDraftAndReplaysWithoutSending(t *testing.T) {
	outputs := llmBuiltinOutputs(t)
	var draft domain.IdeaDraftV1
	if err := json.Unmarshal(outputs["idea.draft"], &draft); err != nil {
		t.Fatal(err)
	}
	// Valid JSON/domain shape, but three candidates violate the requested two.
	draft.Candidates = append(draft.Candidates, draft.Candidates[0])
	bad, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"binding", "format", "http_rejection", "budget"} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			limit := int64(3)
			if mode == "budget" {
				limit = 0
			}
			f := newGenerationExecutorFixtureWithWorkflow(t, limit, false, workflow.RetryingGenerationRevision, 1, func(w http.ResponseWriter, _ *http.Request) {
				calls++
				if mode == "http_rejection" {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				content := outputs["idea.draft"]
				if calls == 1 {
					content = bad
					if mode == "format" {
						content = []byte(`{"private":"invalid"}`)
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(content)}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
			})
			ctx := context.Background()
			view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
			first, err := f.executor.RunIdea(ctx, view, f.snapshot)
			if err != nil || first.Outcome.Review == nil {
				t.Fatalf("first=%+v err=%v", first, err)
			}
			review := first.Outcome.Review
			if mode == "http_rejection" || mode == "budget" {
				if workflow.ContentRetryTarget(view.WorkflowRevision(), "idea", review.Reason) != "" {
					t.Fatal("non-content failure got a retry")
				}
				return
			}
			if workflow.ContentRetryTarget(view.WorkflowRevision(), "idea", review.Reason) != "idea" {
				t.Fatalf("missing retry for %s", review.Reason)
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
			current, err = f.store.FinishContentRetry(ctx, domain.FinishContentRetryCommand{Finish: domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "idea", AttemptID: view.AttemptID(), AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview, ReviewEvidenceDigest: &review.EvidenceDigest, ReviewPolicyDigest: &review.PolicyDigest, At: f.clock.Now(), IdempotencyKey: coordinatorID("retry", "content")}, Reason: review.Reason})
			if err != nil || current.State != domain.RunRunning {
				t.Fatalf("retry=%+v %v", current, err)
			}
			view = f.begin(t, "idea", f.snapshot.SnapshotDigest, 2)
			executor, err := application.NewGenerationExecutor(f.executorConfig)
			if err != nil {
				t.Fatal(err)
			}
			for replay := 0; replay < 2; replay++ {
				result, err := executor.RunIdea(ctx, view, f.snapshot)
				if err != nil || result.Outcome.Value == nil || f.httpCalls.Load() != 2 {
					t.Fatalf("regeneration=%+v err=%v sends=%d", result, err, f.httpCalls.Load())
				}
			}
			budget, err := f.store.BudgetSnapshot(ctx, f.runID)
			if err != nil || budget.Remaining[domain.BudgetLLMCalls] != 1 {
				t.Fatalf("retry did not share budget: %+v %v", budget, err)
			}
		})
	}
}
