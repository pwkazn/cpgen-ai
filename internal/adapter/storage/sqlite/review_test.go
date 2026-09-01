package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

// TestReviewRequiresNeedsReviewExactBindingAndSinglePending catches reviews
// created against stale evidence, while work is cancellable, or alongside a
// second decision that could make resume ambiguous.
func TestReviewRequiresNeedsReviewExactBindingAndSinglePending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	snapshot, binding := driveRunToNeedsReview(t, store, testRunID, testAttemptID)
	request := testCreateReviewRequest(testRunID, snapshot.Version, domain.ReviewRetry, binding, 1)
	request.BudgetIncrease = domain.BudgetLimits{MaxLLMCalls: 1}
	decision, err := store.CreateReview(ctx, request)
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	if decision.State != domain.ReviewPending || decision.Kind != domain.ReviewRetry || decision.Reviewer != "reviewer@example.test" {
		t.Fatalf("decision = %+v", decision)
	}
	replayed, err := store.CreateReview(ctx, request)
	if err != nil || !reflect.DeepEqual(replayed, decision) {
		t.Fatalf("CreateReview replay = %+v, %v", replayed, err)
	}
	drifted := request
	drifted.Reason = "different reason"
	if _, err := store.CreateReview(ctx, drifted); !errors.Is(err, ErrConsistency) {
		t.Fatalf("changed review replay = %v, want ErrConsistency", err)
	}
	second := request
	second.ID = "review_00000000000000000000000000000002"
	second.IdempotencyKey = "reviewcreate_00000000000000000000000000000002"
	second.ExpectedRunVersion = decision.RunVersion
	if _, err := store.CreateReview(ctx, second); !errors.Is(err, ErrReviewPending) {
		t.Fatalf("second pending review = %v, want ErrReviewPending", err)
	}
	pending, err := store.PendingReview(ctx, testRunID)
	if err != nil {
		t.Fatalf("PendingReview: %v", err)
	}
	if pending == nil || pending.ID != decision.ID {
		t.Fatalf("pending review = %+v", pending)
	}
}

// TestReviewRejectsActiveCancel catches a reviewer racing a cancellation and
// creating a decision that the executor must never apply.
func TestReviewRejectsActiveCancel(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run_00000000000000000000000000000005")
	attemptID := domain.AttemptID("attempt_00000000000000000000000000000006")
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	snapshot, binding := driveRunToNeedsReview(t, store, runID, attemptID)
	control, err := store.RequestCancel(ctx, domain.CancelRequest{
		ID: "control_00000000000000000000000000000003", RunID: runID, ExpectedRunVersion: snapshot.Version,
		Reason: "cancel before review", IdempotencyKey: "cancel_00000000000000000000000000000003", At: testNow.Add(4 * time.Second),
	})
	if err != nil || !control.Active {
		t.Fatalf("RequestCancel = %+v, %v", control, err)
	}
	request := testCreateReviewRequest(runID, control.RunVersion, domain.ReviewReject, binding, 3)
	if _, err := store.CreateReview(ctx, request); !errors.Is(err, ErrCancelPending) {
		t.Fatalf("review during cancel = %v, want ErrCancelPending", err)
	}
}

// TestReviewKindsValidateAndApply catches weakening REVISE, RETRY, WAIVE, or
// REJECT into an unbound generic state setter.
func TestReviewKindsValidateAndApply(t *testing.T) {
	t.Parallel()
	for index, kind := range []domain.ReviewDecisionKind{
		domain.ReviewRevise, domain.ReviewRetry, domain.ReviewWaive, domain.ReviewReject,
	} {
		kind := kind
		index := index
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			runID := domain.RunID([]string{
				"run_00000000000000000000000000000006",
				"run_00000000000000000000000000000007",
				"run_00000000000000000000000000000008",
				"run_00000000000000000000000000000009",
			}[index])
			attemptID := domain.AttemptID([]string{
				"attempt_00000000000000000000000000000007",
				"attempt_00000000000000000000000000000008",
				"attempt_00000000000000000000000000000009",
				"attempt_00000000000000000000000000000010",
			}[index])
			store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
			snapshot, binding := driveRunToNeedsReview(t, store, runID, attemptID)
			request := testCreateReviewRequest(runID, snapshot.Version, kind, binding, 10+index)
			switch kind {
			case domain.ReviewRevise:
				digest := domain.SumBytes([]byte("requested edits"))
				request.RequestedEditsDigest = &digest
			case domain.ReviewRetry:
				request.BudgetIncrease = domain.BudgetLimits{MaxSandboxCreates: 1}
			case domain.ReviewWaive:
				digest := domain.SumBytes([]byte("waiver scope"))
				request.WaiverScopeDigest = &digest
				request.WaivableGate = true
			}
			decision, err := store.CreateReview(ctx, request)
			if err != nil {
				t.Fatalf("CreateReview(%s): %v", kind, err)
			}
			apply := domain.ApplyReviewCommand{
				RunID: runID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID,
				StageName: binding.stage, StageInputDigest: binding.input, EvidenceDigest: binding.evidence, PolicyDigest: binding.policy,
				IdempotencyKey: fmt.Sprintf("reviewapply_%032x", index+1),
				At:             testNow.Add(5 * time.Second),
			}
			if kind == domain.ReviewRevise {
				newInput := domain.SumBytes([]byte("revised stage input"))
				newConfig := domain.SumBytes([]byte("revised config"))
				apply.NewInputDigest = &newInput
				apply.NewConfigDigest = &newConfig
				apply.InvalidatedStages = []domain.StageName{"prepare", "exercise", "checkpoint"}
			}
			result, err := store.ApplyReview(ctx, apply)
			if err != nil {
				t.Fatalf("ApplyReview(%s): %v", kind, err)
			}
			wantState := domain.RunCreated
			wantDecisionState := domain.ReviewApplied
			if kind == domain.ReviewReject {
				wantState = domain.RunFailed
				wantDecisionState = domain.ReviewRejected
			}
			if result.State != wantState {
				t.Fatalf("ApplyReview(%s) run state = %s, want %s", kind, result.State, wantState)
			}
			pending, err := store.PendingReview(ctx, runID)
			if err != nil || pending != nil {
				t.Fatalf("pending after apply = %+v, %v", pending, err)
			}
			var state string
			if err := store.db.QueryRowContext(ctx, "SELECT state FROM review_decisions WHERE review_id = ?", string(decision.ID)).Scan(&state); err != nil {
				t.Fatalf("load applied review: %v", err)
			}
			if state != string(wantDecisionState) {
				t.Fatalf("review state = %q, want %q", state, wantDecisionState)
			}
		})
	}
}

// TestReviewApplyRechecksBinding catches applying an otherwise valid decision
// after its stage input, evidence, or policy binding has changed.
func TestReviewApplyRechecksBinding(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run_00000000000000000000000000000010")
	attemptID := domain.AttemptID("attempt_00000000000000000000000000000011")
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	snapshot, binding := driveRunToNeedsReview(t, store, runID, attemptID)
	request := testCreateReviewRequest(runID, snapshot.Version, domain.ReviewRetry, binding, 20)
	condition := domain.SumBytes([]byte("new external condition"))
	request.ExternalConditionDigest = &condition
	decision, err := store.CreateReview(ctx, request)
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	wrong := domain.SumBytes([]byte("wrong evidence"))
	_, err = store.ApplyReview(ctx, domain.ApplyReviewCommand{
		RunID: runID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID,
		StageName: binding.stage, StageInputDigest: binding.input, EvidenceDigest: wrong, PolicyDigest: binding.policy,
		IdempotencyKey: fmt.Sprintf("reviewapply_%032x", 9), At: testNow.Add(5 * time.Second),
	})
	if !errors.Is(err, ErrConsistency) {
		t.Fatalf("ApplyReview wrong binding = %v, want ErrConsistency", err)
	}
}

// TestReviewBindsPersistedWorkflowAndCurrentSnapshot catches accepting review
// evidence from another compiled workflow, or applying a decision after the
// persisted stage snapshot has changed underneath the pending decision.
func TestReviewBindsPersistedWorkflowAndCurrentSnapshot(t *testing.T) {
	t.Parallel()
	t.Run("creation requires persisted workflow revision", func(t *testing.T) {
		ctx := context.Background()
		runID := domain.RunID("run_00000000000000000000000000000011")
		attemptID := domain.AttemptID("attempt_00000000000000000000000000000012")
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
		snapshot, binding := driveRunToNeedsReview(t, store, runID, attemptID)
		request := testCreateReviewRequest(runID, snapshot.Version, domain.ReviewReject, binding, 21)
		request.WorkflowRevision = "other-workflow/v1"
		if _, err := store.CreateReview(ctx, request); !errors.Is(err, ErrConsistency) {
			t.Fatalf("CreateReview workflow mismatch = %v, want ErrConsistency", err)
		}
	})

	t.Run("application rereads current stage snapshot", func(t *testing.T) {
		ctx := context.Background()
		runID := domain.RunID("run_00000000000000000000000000000012")
		attemptID := domain.AttemptID("attempt_00000000000000000000000000000013")
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
		snapshot, binding := driveRunToNeedsReview(t, store, runID, attemptID)
		request := testCreateReviewRequest(runID, snapshot.Version, domain.ReviewRetry, binding, 22)
		condition := domain.SumBytes([]byte("retry after provider recovery"))
		request.ExternalConditionDigest = &condition
		decision, err := store.CreateReview(ctx, request)
		if err != nil {
			t.Fatalf("CreateReview: %v", err)
		}
		changed := domain.SumBytes([]byte("changed stage input"))
		if _, err := store.db.ExecContext(ctx,
			"UPDATE stage_records SET input_digest = ? WHERE run_id = ? AND stage_name = ?",
			string(changed), string(runID), string(binding.stage),
		); err != nil {
			t.Fatalf("change persisted stage snapshot: %v", err)
		}
		_, err = store.ApplyReview(ctx, domain.ApplyReviewCommand{
			RunID: runID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID,
			StageName: binding.stage, StageInputDigest: binding.input, EvidenceDigest: binding.evidence, PolicyDigest: binding.policy,
			IdempotencyKey: "reviewapply_00000000000000000000000000000010", At: testNow.Add(5 * time.Second),
		})
		if !errors.Is(err, ErrConsistency) {
			t.Fatalf("ApplyReview stale persisted snapshot = %v, want ErrConsistency", err)
		}
	})
}

type reviewBinding struct {
	stage            domain.StageName
	input            domain.Digest
	evidence         domain.Digest
	policy           domain.Digest
	workflowRevision string
}

func driveRunToNeedsReview(t *testing.T, store *Store, runID domain.RunID, attemptID domain.AttemptID) (domain.RunSnapshot, reviewBinding) {
	t.Helper()
	request := testCreateRunRequest(runID, testNow, 30*time.Second)
	request.IdempotencyKey = "create_" + string(runID[len(runID)-32:])
	mustCreateRun(t, store, request)
	input := domain.SumBytes([]byte("review input " + string(runID)))
	mustBeginStage(t, store, runID, attemptID, 1, "prepare", input, testNow.Add(time.Second), "begin_"+string(attemptID[len(attemptID)-32:]))
	evidence := domain.SumBytes([]byte("review evidence " + string(runID)))
	policy := domain.SumBytes([]byte("review policy v1"))
	snapshot, err := store.FinishStage(context.Background(), domain.FinishStageCommand{
		RunID: runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: attemptID,
		AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview,
		ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &policy,
		IdempotencyKey: "finish_" + string(attemptID[len(attemptID)-32:]), At: testNow.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("FinishStage NEEDS_REVIEW: %v", err)
	}
	return snapshot, reviewBinding{
		stage: "prepare", input: input, evidence: evidence, policy: policy,
		workflowRevision: snapshot.WorkflowRevision,
	}
}

func testCreateReviewRequest(runID domain.RunID, expectedVersion int64, kind domain.ReviewDecisionKind, binding reviewBinding, suffix int) domain.CreateReviewRequest {
	return domain.CreateReviewRequest{
		ID: domain.ReviewDecisionID(reviewID(suffix)), RunID: runID, ExpectedRunVersion: expectedVersion,
		Kind: kind, WorkflowRevision: binding.workflowRevision, StageName: binding.stage,
		StageInputDigest: binding.input, EvidenceDigest: binding.evidence, PolicyDigest: binding.policy,
		Reviewer: "reviewer@example.test", Reason: "review decision reason",
		IdempotencyKey: reviewCreateID(suffix), At: testNow.Add(3 * time.Second),
	}
}

func reviewID(suffix int) string {
	return fmt.Sprintf("review_%032x", suffix)
}

func reviewCreateID(suffix int) string {
	return fmt.Sprintf("reviewcreate_%032x", suffix)
}
