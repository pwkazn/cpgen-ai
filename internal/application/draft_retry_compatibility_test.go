package application_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

// These are real SQLite calls and private receipts produced through the local
// HTTP fixture. The retry row exists before the second attempt, exactly as it
// does in databases created before feedback was added to the draft contract.
func TestDraftRetryProtocolPreservesPersistedCallsAcrossUpgrade(t *testing.T) {
	for _, protocol := range []string{"legacy", "unversioned-feedback", "frozen-feedback"} {
		for _, repair := range []bool{false, true} {
			name := protocol
			if repair {
				name += "/format-repair"
			}
			t.Run(name, func(t *testing.T) {
				outputs := llmBuiltinOutputs(t)
				var bad domain.IdeaDraftV1
				if err := json.Unmarshal(outputs["idea.draft"], &bad); err != nil {
					t.Fatal(err)
				}
				bad.Candidates = append(bad.Candidates, bad.Candidates[0])
				badJSON, err := json.Marshal(bad)
				if err != nil {
					t.Fatal(err)
				}
				var bodies []string
				var bodiesMu sync.Mutex
				f := newGenerationExecutorFixtureWithWorkflow(t, 5, repair, workflow.ExecutedSamplesRevision, 1, func(w http.ResponseWriter, r *http.Request) {
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
					}
					bodiesMu.Lock()
					bodies = append(bodies, string(raw))
					ordinal := len(bodies)
					bodiesMu.Unlock()
					content := outputs["idea.draft"]
					if ordinal == 1 {
						content = badJSON
					} else if repair && ordinal == 2 {
						content = []byte(`{"private":"invalid"}`)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(content)}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
				})
				ctx := context.Background()
				view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
				first, err := f.executor.RunIdea(ctx, view, f.snapshot)
				if err != nil || first.Outcome.Review == nil {
					t.Fatalf("initial rejection: %+v %v", first, err)
				}
				review := first.Outcome.Review
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
				_, err = f.store.FinishContentRetry(ctx, domain.FinishContentRetryCommand{Finish: domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "idea", AttemptID: view.AttemptID(), AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview, ReviewEvidenceDigest: &review.EvidenceDigest, ReviewPolicyDigest: &review.PolicyDigest, At: f.clock.Now(), IdempotencyKey: coordinatorID("retry", "protocol")}, Reason: review.Reason})
				if err != nil {
					t.Fatal(err)
				}
				view = f.begin(t, "idea", f.snapshot.SnapshotDigest, 2)
				cfg := f.executorConfig
				cfg.Content.WorkflowRevision = workflow.ExecutedSamplesRevision
				if protocol != "legacy" {
					cfg.Content.DraftRetryFeedbackVersion = "v1"
				}
				producer, err := application.NewGenerationExecutor(cfg)
				if err != nil {
					t.Fatal(err)
				}
				// No persisted draft exists yet; reconciliation must not open one.
				if err := producer.ReconcileStage(ctx, f.runID); err != nil || f.httpCalls.Load() != 1 {
					t.Fatalf("unsent reconcile: %v, calls=%d", err, f.httpCalls.Load())
				}
				result, err := producer.RunIdea(ctx, view, f.snapshot)
				if err != nil || result.Outcome.Value == nil {
					t.Fatalf("second attempt: %+v %v", result, err)
				}
				wantCalls := int32(2)
				if repair {
					wantCalls++
				}
				bodiesMu.Lock()
				retryBodies := append([]string(nil), bodies[1:]...)
				bodiesMu.Unlock()
				for _, body := range retryBodies {
					if strings.Contains(body, `retry_feedback`) != (protocol != "legacy") {
						t.Fatalf("retry/repair request used the wrong protocol: %s", body)
					}
				}
				if protocol == "unversioned-feedback" {
					// The intermediate binary emitted the same feedback identity,
					// but did not yet have a frozen config selector.
					cfg.Content.DraftRetryFeedbackVersion = ""
				}
				restarted, err := application.NewGenerationExecutor(cfg)
				if err != nil {
					t.Fatal(err)
				}
				for replay := 0; replay < 2; replay++ {
					if err := restarted.ReconcileStage(ctx, f.runID); err != nil {
						t.Fatalf("running attempt reconcile: %v", err)
					}
					replayed, err := restarted.RunIdea(ctx, view, f.snapshot)
					if err != nil || replayed.Outcome.Value == nil || replayed.Outcome.Value.BatchDigest != result.Outcome.Value.BatchDigest || f.httpCalls.Load() != wantCalls {
						t.Fatalf("running attempt replay: %+v %v calls=%d", replayed, err, f.httpCalls.Load())
					}
				}
				// An identity mismatch may neither reinterpret the paid call nor
				// dispatch a new request under another protocol.
				changed := cfg
				changed.Content.MaxOutput.Tokens++
				mismatched, err := application.NewGenerationExecutor(changed)
				if err != nil {
					t.Fatal(err)
				}
				if err := mismatched.ReconcileStage(ctx, f.runID); err == nil {
					t.Fatal("changed frozen request reconciled")
				}
				if _, err := mismatched.RunIdea(ctx, view, f.snapshot); err == nil || f.httpCalls.Load() != wantCalls {
					t.Fatalf("changed frozen request replayed or sent: %v", err)
				}
				batch := *result.Outcome.Value
				ids, err := batch.OrderedFeasibleCandidateIDs(cfg.Content.SelectionPolicy)
				if err != nil {
					t.Fatal(err)
				}
				selection, err := domain.NewIdeaSelection(f.snapshot.RequestDigest, batch, ids[0], cfg.Content.SelectionPolicy, []string{"deterministic_selection"}, []domain.Digest{batch.BatchDigest})
				if err != nil {
					t.Fatal(err)
				}
				input := domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: f.snapshot.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}
				digest, err := input.Digest()
				if err != nil {
					t.Fatal(err)
				}
				f.finish(t, view, batch.BatchDigest, "statement", digest, result.Occurrences)
				for read := 0; read < 2; read++ {
					idea, err := restarted.Reader().ReadIdea(ctx, f.runID)
					if err != nil || idea.Batch.BatchDigest != batch.BatchDigest || f.httpCalls.Load() != wantCalls {
						t.Fatalf("committed retry read: %+v %v calls=%d", idea, err, f.httpCalls.Load())
					}
				}
				if _, err := mismatched.Reader().ReadIdea(ctx, f.runID); err == nil || f.httpCalls.Load() != wantCalls {
					t.Fatalf("changed frozen request read or sent: %v", err)
				}
			})
		}
	}
}
