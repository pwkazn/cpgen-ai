package sqlite

import (
	"context"
	"errors"
	"fmt"
	"math"
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
			snapshot, binding := driveRunToNeedsReview(t, store, runID, attemptID, kind == domain.ReviewWaive)
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
				newConfigJSON := []byte(`{"schema_version":"cpgen.config/v2"}`)
				newConfig := domain.SumBytes(newConfigJSON)
				apply.NewInputDigest = &newInput
				apply.NewConfigJSON = newConfigJSON
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

// TestRetryBudgetIncreasesAreAppliedAndBackedByReservations keeps the stored
// run limits, metered accounts, immutable request and live reservation path in
// agreement across repeated approvals and an idempotent apply replay.
func TestRetryBudgetIncreasesAreAppliedAndBackedByReservations(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run_00000000000000000000000000000041")
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "retry-budget.db"), clock.NewFake(testNow))
	attemptID := domain.AttemptID("attempt_00000000000000000000000000000041")
	snapshot, binding := driveRunToNeedsReview(t, store, runID, attemptID)
	var submitted []byte
	var requestDigest string
	if err := store.db.QueryRowContext(ctx, `SELECT submitted_request_json,submitted_request_digest FROM runs WHERE run_id=?`, string(runID)).Scan(&submitted, &requestDigest); err != nil {
		t.Fatal(err)
	}

	request := testCreateReviewRequest(runID, snapshot.Version, domain.ReviewRetry, binding, 41)
	request.BudgetIncrease = domain.BudgetLimits{MaxSimilarityCostMicroUSD: 100000, MaxPackageBytes: 512, MaxActiveTimeMilliseconds: 5000}
	decision, err := store.CreateReview(ctx, request)
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	command := domain.ApplyReviewCommand{
		RunID: runID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID,
		StageName: binding.stage, StageInputDigest: binding.input, EvidenceDigest: binding.evidence, PolicyDigest: binding.policy,
		IdempotencyKey: "reviewapply_00000000000000000000000000000041", At: testNow.Add(4 * time.Second),
	}
	first, err := store.ApplyReview(ctx, command)
	if err != nil {
		t.Fatalf("ApplyReview: %v", err)
	}
	replayed, err := store.ApplyReview(ctx, command)
	if err != nil || !reflect.DeepEqual(replayed, first) {
		t.Fatalf("ApplyReview replay = %+v, %v", replayed, err)
	}
	budget, err := store.BudgetSnapshot(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if budget.Limits.MaxSimilarityCostMicroUSD != 107000 || budget.Limits.MaxPackageBytes != (1<<20)+512 || budget.Limits.MaxActiveTimeMilliseconds != 35000 || budget.Limits.MaxMutationsPerStage != 2 {
		t.Fatalf("first effective budget = %+v", budget.Limits)
	}
	account := readBudgetAccount(t, store, runID, domain.BudgetSimilarityCostMicroUSD)
	if account.Limit != 107000 || account.Version != 2 || account.Reserved != 0 || account.Consumed != 0 {
		t.Fatalf("similarity account after first review = %+v", account)
	}

	// A later RETRY adds to the already approved effective cap.
	secondAttempt := domain.AttemptID("attempt_00000000000000000000000000000042")
	mustBeginStage(t, store, runID, secondAttempt, first.Version, binding.stage, binding.input, testNow.Add(5*time.Second), "begin_00000000000000000000000000000042")
	evidence := domain.SumBytes([]byte("second review evidence"))
	policy := binding.policy
	secondSnapshot, err := store.FinishStage(ctx, domain.FinishStageCommand{
		RunID: runID, ExpectedRunVersion: first.Version + 1, StageName: binding.stage, AttemptID: secondAttempt,
		AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview,
		ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &policy,
		IdempotencyKey: "finish_00000000000000000000000000000042", At: testNow.Add(6 * time.Second),
	})
	if err != nil {
		t.Fatalf("FinishStage second review: %v", err)
	}
	secondBinding := reviewBinding{stage: binding.stage, input: binding.input, evidence: evidence, policy: policy, workflowRevision: secondSnapshot.WorkflowRevision}
	secondRequest := testCreateReviewRequest(runID, secondSnapshot.Version, domain.ReviewRetry, secondBinding, 42)
	secondRequest.BudgetIncrease = domain.BudgetLimits{MaxSimilarityCostMicroUSD: 11}
	secondDecision, err := store.CreateReview(ctx, secondRequest)
	if err != nil {
		t.Fatalf("CreateReview second: %v", err)
	}
	secondCommand := domain.ApplyReviewCommand{
		RunID: runID, ExpectedRunVersion: secondDecision.RunVersion, ReviewDecisionID: secondDecision.ID,
		StageName: secondBinding.stage, StageInputDigest: secondBinding.input, EvidenceDigest: secondBinding.evidence, PolicyDigest: secondBinding.policy,
		IdempotencyKey: "reviewapply_00000000000000000000000000000042", At: testNow.Add(7 * time.Second),
	}
	resumed, err := store.ApplyReview(ctx, secondCommand)
	if err != nil {
		t.Fatalf("ApplyReview second: %v", err)
	}
	budget, err = store.BudgetSnapshot(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	account = readBudgetAccount(t, store, runID, domain.BudgetSimilarityCostMicroUSD)
	if budget.Limits.MaxSimilarityCostMicroUSD != 107011 || account.Limit != 107011 || account.Version != 3 {
		t.Fatalf("accumulated similarity budget = limit:%d account:%+v", budget.Limits.MaxSimilarityCostMicroUSD, account)
	}
	var applications int
	if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM review_budget_applications WHERE run_id=? AND field='max_similarity_cost_micro_usd'`, string(runID)).Scan(&applications); err != nil || applications != 2 {
		t.Fatalf("budget application audit count = %d, %v", applications, err)
	}

	var submittedAfter []byte
	var digestAfter string
	if err := store.db.QueryRowContext(ctx, `SELECT submitted_request_json,submitted_request_digest FROM runs WHERE run_id=?`, string(runID)).Scan(&submittedAfter, &digestAfter); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(submittedAfter, submitted) || digestAfter != requestDigest {
		t.Fatal("review changed the immutable submitted request or digest")
	}

	// The increased account now admits a reservation that exceeded the original
	// similarity-cost limit, through the same metering path used by the runner.
	thirdAttempt := domain.AttemptID("attempt_00000000000000000000000000000043")
	mustBeginStage(t, store, runID, thirdAttempt, resumed.Version, binding.stage, binding.input, testNow.Add(8*time.Second), "begin_00000000000000000000000000000043")
	running, err := store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	activeStart, err := store.AccountActiveTime(ctx, domain.ActiveTimeCommand{
		RunID: runID, ExpectedRunVersion: running.Version, Action: domain.ActiveTimeStart,
		IdempotencyKey: "active_00000000000000000000000000000041", At: testNow.Add(9 * time.Second),
	})
	if err != nil {
		t.Fatalf("start active time after retry: %v", err)
	}
	activeHeartbeat, err := store.AccountActiveTime(ctx, domain.ActiveTimeCommand{
		RunID: runID, ExpectedRunVersion: activeStart.RunVersion, Action: domain.ActiveTimeHeartbeat,
		IdempotencyKey: "active_00000000000000000000000000000042", At: testNow.Add(43 * time.Second),
	})
	if err != nil || activeHeartbeat.ActiveElapsed != 34*time.Second || activeHeartbeat.Remaining != time.Second {
		t.Fatalf("active-time allowance beyond original 30s cap = %+v, %v", activeHeartbeat, err)
	}
	running, err = store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	fixture := meteringFixture{store: store, runID: runID, attemptID: thirdAttempt, stage: binding.stage, now: testNow.Add(44 * time.Second)}
	open := openCallRequest(fixture, 41, domain.CallSimilaritySearch)
	open.ExpectedRunVersion = running.Version
	open.At = fixture.now
	call, err := store.OpenCall(ctx, open)
	if err != nil {
		t.Fatalf("OpenCall after retry: %v", err)
	}
	prepared, err := store.PrepareCalls(ctx, domain.PrepareCallsRequest{
		RunID: runID, ExpectedRunVersion: running.Version, StageName: binding.stage, AttemptID: thirdAttempt,
		CallRecordID: call.ID, PlanDigest: domain.SumBytes([]byte("review retry reservation")),
		Calls:          []domain.PhysicalCallPlan{oneReservationPhysicalPlan(41, 1, domain.PhysicalSimilarityRequest, domain.BudgetSimilarityCostMicroUSD, 100000)},
		IdempotencyKey: "prepare_00000000000000000000000000000041", At: fixture.now,
	})
	if err != nil || prepared.Failure != nil {
		t.Fatalf("PrepareCalls above original limit = %+v, %v", prepared.Failure, err)
	}
	account = readBudgetAccount(t, store, runID, domain.BudgetSimilarityCostMicroUSD)
	if account.Reserved != 100000 || account.Remaining() != 7011 {
		t.Fatalf("reservation was not charged against the increased limit: %+v", account)
	}
}

func TestRetryBudgetFailureRollsBackAndOverflowIsRejected(t *testing.T) {
	t.Parallel()
	t.Run("account failure rolls back approval application", func(t *testing.T) {
		ctx := context.Background()
		runID := domain.RunID("run_00000000000000000000000000000044")
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "retry-rollback.db"), clock.NewFake(testNow))
		snapshot, binding := driveRunToNeedsReview(t, store, runID, domain.AttemptID("attempt_00000000000000000000000000000044"))
		request := testCreateReviewRequest(runID, snapshot.Version, domain.ReviewRetry, binding, 44)
		request.BudgetIncrease = domain.BudgetLimits{MaxSimilarityCostMicroUSD: 100}
		decision, err := store.CreateReview(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER test_fail_review_budget_account BEFORE UPDATE OF limit_value ON budget_accounts
			WHEN NEW.run_id='run_00000000000000000000000000000044' AND NEW.dimension='SIMILARITY_COST_MICRO_USD'
			BEGIN SELECT RAISE(ABORT,'injected account failure'); END`); err != nil {
			t.Fatal(err)
		}
		command := domain.ApplyReviewCommand{
			RunID: runID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID,
			StageName: binding.stage, StageInputDigest: binding.input, EvidenceDigest: binding.evidence, PolicyDigest: binding.policy,
			IdempotencyKey: "reviewapply_00000000000000000000000000000044", At: testNow.Add(4 * time.Second),
		}
		if _, err := store.ApplyReview(ctx, command); err == nil {
			t.Fatal("ApplyReview succeeded despite injected account failure")
		}
		pending, err := store.PendingReview(ctx, runID)
		if err != nil || pending == nil || pending.ID != decision.ID {
			t.Fatalf("failed transaction did not restore pending decision: %+v %v", pending, err)
		}
		account := readBudgetAccount(t, store, runID, domain.BudgetSimilarityCostMicroUSD)
		var runLimit, ledgerRows int64
		if err := store.db.QueryRowContext(ctx, `SELECT max_similarity_cost_micro_usd FROM runs WHERE run_id=?`, string(runID)).Scan(&runLimit); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM review_budget_applications WHERE review_id=?`, string(decision.ID)).Scan(&ledgerRows); err != nil {
			t.Fatal(err)
		}
		if account.Limit != 7000 || account.Version != 1 || runLimit != 7000 || ledgerRows != 0 {
			t.Fatalf("failed apply leaked changes: account=%+v run_limit=%d ledger=%d", account, runLimit, ledgerRows)
		}
		if _, err := store.db.ExecContext(ctx, `DROP TRIGGER test_fail_review_budget_account`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.ApplyReview(ctx, command); err != nil {
			t.Fatalf("retry after rolled-back apply: %v", err)
		}
		account = readBudgetAccount(t, store, runID, domain.BudgetSimilarityCostMicroUSD)
		if account.Limit != 7100 || account.Version != 2 {
			t.Fatalf("retry after rollback produced wrong account: %+v", account)
		}
	})

	t.Run("effective cap overflow is rejected before creating approval", func(t *testing.T) {
		ctx := context.Background()
		runID := domain.RunID("run_00000000000000000000000000000045")
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "retry-overflow.db"), clock.NewFake(testNow))
		snapshot, binding := driveRunToNeedsReview(t, store, runID, domain.AttemptID("attempt_00000000000000000000000000000045"))
		review := testCreateReviewRequest(runID, snapshot.Version, domain.ReviewRetry, binding, 45)
		review.BudgetIncrease = domain.BudgetLimits{MaxSimilarityCostMicroUSD: math.MaxInt64}
		if _, err := store.CreateReview(ctx, review); !errors.Is(err, ErrConsistency) {
			t.Fatalf("overflowing review amount = %v, want ErrConsistency", err)
		}
		var reviews int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM review_decisions WHERE run_id=?`, string(runID)).Scan(&reviews); err != nil {
			t.Fatal(err)
		}
		if reviews != 0 {
			t.Fatalf("overflowing budget persisted %d review decisions", reviews)
		}
		account := readBudgetAccount(t, store, runID, domain.BudgetSimilarityCostMicroUSD)
		if account.Limit != 7000 {
			t.Fatalf("overflow changed the authoritative account: %+v", account)
		}
	})

	t.Run("frozen mutation quota cannot be increased by RETRY", func(t *testing.T) {
		ctx := context.Background()
		runID := domain.RunID("run_00000000000000000000000000000046")
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "retry-mutation.db"), clock.NewFake(testNow))
		snapshot, binding := driveRunToNeedsReview(t, store, runID, domain.AttemptID("attempt_00000000000000000000000000000046"))
		review := testCreateReviewRequest(runID, snapshot.Version, domain.ReviewRetry, binding, 46)
		review.BudgetIncrease = domain.BudgetLimits{MaxMutationsPerStage: 1}
		if _, err := store.CreateReview(ctx, review); err == nil {
			t.Fatal("RETRY accepted a mutation quota detached from the immutable request")
		}
	})
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

// TestReviewWaiveUsesPersistedGatePolicy catches granting WAIVE authority to
// the review caller instead of deriving it from the immutable stage result.
func TestReviewWaiveUsesPersistedGatePolicy(t *testing.T) {
	t.Parallel()
	for _, waivable := range []bool{false, true} {
		waivable := waivable
		t.Run(fmt.Sprintf("waivable=%t", waivable), func(t *testing.T) {
			ctx := context.Background()
			suffix := 30
			if waivable {
				suffix = 31
			}
			runID := domain.RunID(fmt.Sprintf("run_%032x", suffix))
			attemptID := domain.AttemptID(fmt.Sprintf("attempt_%032x", suffix))
			store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
			snapshot, binding := driveRunToNeedsReview(t, store, runID, attemptID, waivable)
			request := testCreateReviewRequest(runID, snapshot.Version, domain.ReviewWaive, binding, suffix)
			scope := domain.SumBytes([]byte("waiver scope"))
			request.WaiverScopeDigest = &scope
			decision, err := store.CreateReview(ctx, request)
			if !waivable {
				if !errors.Is(err, ErrInvalidTransition) {
					t.Fatalf("non-waivable CreateReview = %v, want ErrInvalidTransition", err)
				}
				return
			}
			if err != nil || !decision.WaivableGate {
				t.Fatalf("waivable CreateReview = %+v, %v", decision, err)
			}
		})
	}
}

// TestReviewRevisePreservesCanonicalConfigHistory catches replacing only a
// digest, destroying the original binding, or duplicating history on replay.
func TestReviewRevisePreservesCanonicalConfigHistory(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	runID := domain.RunID("run_00000000000000000000000000000032")
	attemptID := domain.AttemptID("attempt_00000000000000000000000000000032")
	store := openRuntimeStore(t, path, clock.NewFake(testNow))
	snapshot, binding := driveRunToNeedsReview(t, store, runID, attemptID)
	request := testCreateReviewRequest(runID, snapshot.Version, domain.ReviewRevise, binding, 32)
	edits := domain.SumBytes([]byte("config edits"))
	request.RequestedEditsDigest = &edits
	decision, err := store.CreateReview(ctx, request)
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	newInput := domain.SumBytes([]byte("revised input"))
	newConfigJSON := []byte(`{"schema_version":"cpgen.config/v2","temperature":0}`)
	newConfigDigest := domain.SumBytes(newConfigJSON)
	command := domain.ApplyReviewCommand{
		RunID: runID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID,
		StageName: binding.stage, StageInputDigest: binding.input, EvidenceDigest: binding.evidence, PolicyDigest: binding.policy,
		NewInputDigest: &newInput, NewConfigJSON: newConfigJSON, NewConfigDigest: &newConfigDigest,
		InvalidatedStages: []domain.StageName{"prepare", "exercise", "checkpoint"},
		IdempotencyKey:    "reviewapply_00000000000000000000000000000032", At: testNow.Add(5 * time.Second),
	}
	applied, err := store.ApplyReview(ctx, command)
	if err != nil {
		t.Fatalf("ApplyReview: %v", err)
	}
	if applied.ConfigDigest != newConfigDigest {
		t.Fatalf("current config digest = %s, want %s", applied.ConfigDigest, newConfigDigest)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close before audit reopen: %v", err)
	}
	reopened := openRuntimeStore(t, path, clock.NewFake(testNow.Add(time.Hour)))
	replayed, err := reopened.ApplyReview(ctx, command)
	if err != nil || !reflect.DeepEqual(replayed, applied) {
		t.Fatalf("ApplyReview replay after reopen = %+v, %v", replayed, err)
	}
	rows, err := reopened.db.QueryContext(ctx, `
		SELECT redacted_effective_config_json, redacted_effective_config_digest
		FROM run_config_revisions WHERE run_id = ? ORDER BY revision`, string(runID))
	if err != nil {
		t.Fatalf("query config revisions: %v", err)
	}
	defer rows.Close()
	var bindings []struct {
		json   []byte
		digest string
	}
	for rows.Next() {
		var binding struct {
			json   []byte
			digest string
		}
		if err := rows.Scan(&binding.json, &binding.digest); err != nil {
			t.Fatalf("scan config revision: %v", err)
		}
		bindings = append(bindings, binding)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("config revision rows: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close config revision rows: %v", err)
	}
	if len(bindings) != 2 {
		t.Fatalf("config revision count = %d, want 2", len(bindings))
	}
	for index, binding := range bindings {
		if domain.SumBytes(binding.json) != domain.Digest(binding.digest) {
			t.Fatalf("config revision %d bytes/digest mismatch", index+1)
		}
	}
	if !reflect.DeepEqual(bindings[1].json, newConfigJSON) {
		t.Fatalf("current config bytes = %s, want %s", bindings[1].json, newConfigJSON)
	}
	var currentJSON []byte
	var currentDigest string
	if err := reopened.db.QueryRowContext(ctx, `
		SELECT redacted_effective_config_json, redacted_effective_config_digest FROM runs WHERE run_id = ?`,
		string(runID),
	).Scan(&currentJSON, &currentDigest); err != nil {
		t.Fatalf("query current config: %v", err)
	}
	if !reflect.DeepEqual(currentJSON, newConfigJSON) || currentDigest != string(newConfigDigest) {
		t.Fatalf("current config binding = %s / %s", currentJSON, currentDigest)
	}
	if _, err := reopened.db.ExecContext(ctx, `
		UPDATE run_config_revisions SET redacted_effective_config_digest = ? WHERE run_id = ? AND revision = 1`,
		string(newConfigDigest), string(runID),
	); err == nil {
		t.Fatal("append-only config history accepted UPDATE")
	}
	if _, err := reopened.db.ExecContext(ctx, `
		DELETE FROM run_config_revisions WHERE run_id = ? AND revision = 1`, string(runID),
	); err == nil {
		t.Fatal("append-only config history accepted DELETE")
	}
}

// TestReviewReviseRequiresExactSuffixAndRestartsEarliestStage catches gaps,
// order changes, partial suffixes, and restarting at the reviewed stage when
// an earlier compiled stage is the true invalidation boundary.
func TestReviewReviseRequiresExactSuffixAndRestartsEarliestStage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runID := domain.RunID("run_00000000000000000000000000000033")
	prepareAttempt := domain.AttemptID("attempt_00000000000000000000000000000033")
	exerciseAttempt := domain.AttemptID("attempt_00000000000000000000000000000034")
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	request := testCreateRunRequest(runID, testNow, 30*time.Second)
	request.IdempotencyKey = "create_00000000000000000000000000000033"
	mustCreateRun(t, store, request)
	prepareInput := domain.SumBytes([]byte("prepare original"))
	mustBeginStage(t, store, runID, prepareAttempt, 1, "prepare", prepareInput, testNow.Add(time.Second), "begin_00000000000000000000000000000033")
	prepareOutput := domain.SumBytes([]byte("prepare output"))
	advanced, err := store.FinishStage(ctx, domain.FinishStageCommand{
		RunID: runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: prepareAttempt,
		AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning,
		OutputDigest: &prepareOutput, NextStage: "exercise", NextInputDigest: &prepareOutput,
		IdempotencyKey: "finish_00000000000000000000000000000033", At: testNow.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("finish prepare: %v", err)
	}
	mustBeginStage(t, store, runID, exerciseAttempt, advanced.Version, "exercise", prepareOutput, testNow.Add(3*time.Second), "begin_00000000000000000000000000000034")
	evidence := domain.SumBytes([]byte("exercise evidence"))
	policy := domain.SumBytes([]byte("exercise policy"))
	reviewSnapshot, err := store.FinishStage(ctx, domain.FinishStageCommand{
		RunID: runID, ExpectedRunVersion: 4, StageName: "exercise", AttemptID: exerciseAttempt,
		AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview,
		ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &policy,
		IdempotencyKey: "finish_00000000000000000000000000000034", At: testNow.Add(4 * time.Second),
	})
	if err != nil {
		t.Fatalf("finish exercise review: %v", err)
	}
	binding := reviewBinding{stage: "exercise", input: prepareOutput, evidence: evidence, policy: policy, workflowRevision: reviewSnapshot.WorkflowRevision}
	reviewRequest := testCreateReviewRequest(runID, reviewSnapshot.Version, domain.ReviewRevise, binding, 33)
	target := domain.StageName("prepare")
	reviewRequest.RevisionTargetStage = &target
	reviewRequest.At = testNow.Add(5 * time.Second)
	edits := domain.SumBytes([]byte("revise from prepare"))
	reviewRequest.RequestedEditsDigest = &edits
	decision, err := store.CreateReview(ctx, reviewRequest)
	if err != nil {
		t.Fatalf("CreateReview: %v", err)
	}
	newInput := domain.SumBytes([]byte("prepare revised"))
	newConfigJSON := []byte(`{"revision":2,"schema_version":"cpgen.config/v2"}`)
	newConfigDigest := domain.SumBytes(newConfigJSON)
	base := domain.ApplyReviewCommand{
		RunID: runID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID,
		StageName: "exercise", StageInputDigest: prepareOutput, EvidenceDigest: evidence, PolicyDigest: policy,
		NewInputDigest: &newInput, NewConfigJSON: newConfigJSON, NewConfigDigest: &newConfigDigest,
		At: testNow.Add(6 * time.Second),
	}
	for index, stages := range [][]domain.StageName{
		{"prepare", "exercise"},
		{"exercise", "prepare", "checkpoint"},
		{"exercise"},
	} {
		invalid := base
		invalid.InvalidatedStages = stages
		invalid.IdempotencyKey = fmt.Sprintf("reviewapply_%032x", 40+index)
		if _, err := store.ApplyReview(ctx, invalid); err == nil {
			t.Fatalf("invalid suffix %v accepted", stages)
		}
	}
	base.InvalidatedStages = []domain.StageName{"prepare", "exercise", "checkpoint"}
	base.IdempotencyKey = "reviewapply_00000000000000000000000000000043"
	restarted, err := store.ApplyReview(ctx, base)
	if err != nil {
		t.Fatalf("ApplyReview exact suffix: %v", err)
	}
	feedback, err := store.ReadLatestDraftRetryFeedback(ctx, runID, "prepare")
	if err != nil || feedback == nil || feedback.SourceStage != "exercise" || feedback.TargetStage != "prepare" || feedback.Reason != reviewRequest.Reason {
		t.Fatalf("persisted revision feedback = %+v, %v", feedback, err)
	}
	unrelatedFeedback, err := store.ReadLatestDraftRetryFeedback(ctx, runID, "exercise")
	if err != nil || unrelatedFeedback != nil {
		t.Fatalf("revision feedback for a different producer = %+v, %v", unrelatedFeedback, err)
	}
	if restarted.State != domain.RunCreated || restarted.CurrentStage != "prepare" || restarted.CurrentStageOrdinal != 1 {
		t.Fatalf("restarted projection = %+v", restarted)
	}
	newPrepareAttempt := domain.AttemptID("attempt_00000000000000000000000000000035")
	if attempt := mustBeginStage(t, store, runID, newPrepareAttempt, restarted.Version, "prepare", newInput, testNow.Add(7*time.Second), "begin_00000000000000000000000000000035"); attempt.Ordinal != 2 {
		t.Fatalf("revised prepare attempt ordinal = %d, want 2", attempt.Ordinal)
	}
	newPrepareOutput := domain.SumBytes([]byte("new prepare output"))
	continued, err := store.FinishStage(ctx, domain.FinishStageCommand{
		RunID: runID, ExpectedRunVersion: restarted.Version + 1, StageName: "prepare", AttemptID: newPrepareAttempt,
		AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning,
		OutputDigest: &newPrepareOutput, NextStage: "exercise", NextInputDigest: &newPrepareOutput,
		IdempotencyKey: "finish_00000000000000000000000000000035", At: testNow.Add(8 * time.Second),
	})
	if err != nil {
		t.Fatalf("finish revised prepare: %v", err)
	}
	newExerciseAttempt := domain.AttemptID("attempt_00000000000000000000000000000036")
	if attempt := mustBeginStage(t, store, runID, newExerciseAttempt, continued.Version, "exercise", newPrepareOutput, testNow.Add(9*time.Second), "begin_00000000000000000000000000000036"); attempt.Ordinal != 2 {
		t.Fatalf("successor attempt ordinal = %d, want 2", attempt.Ordinal)
	}
}

type reviewBinding struct {
	stage            domain.StageName
	input            domain.Digest
	evidence         domain.Digest
	policy           domain.Digest
	workflowRevision string
}

func driveRunToNeedsReview(t *testing.T, store *Store, runID domain.RunID, attemptID domain.AttemptID, waivable ...bool) (domain.RunSnapshot, reviewBinding) {
	t.Helper()
	request := testCreateRunRequest(runID, testNow, 30*time.Second)
	request.IdempotencyKey = "create_" + string(runID[len(runID)-32:])
	return driveRunRequestToNeedsReview(t, store, request, attemptID, len(waivable) != 0 && waivable[0])
}

func driveRunRequestToNeedsReview(t *testing.T, store *Store, request domain.CreateRunRequest, attemptID domain.AttemptID, waivable bool) (domain.RunSnapshot, reviewBinding) {
	t.Helper()
	runID := request.RunID
	mustCreateRun(t, store, request)
	input := domain.SumBytes([]byte("review input " + string(runID)))
	mustBeginStage(t, store, runID, attemptID, 1, "prepare", input, testNow.Add(time.Second), "begin_"+string(attemptID[len(attemptID)-32:]))
	evidence := domain.SumBytes([]byte("review evidence " + string(runID)))
	policy := domain.SumBytes([]byte("review policy v1"))
	snapshot, err := store.FinishStage(context.Background(), domain.FinishStageCommand{
		RunID: runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: attemptID,
		AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview,
		ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &policy,
		ReviewGateWaivable: waivable,
		IdempotencyKey:     "finish_" + string(attemptID[len(attemptID)-32:]), At: testNow.Add(2 * time.Second),
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
	request := domain.CreateReviewRequest{
		ID: domain.ReviewDecisionID(reviewID(suffix)), RunID: runID, ExpectedRunVersion: expectedVersion,
		Kind: kind, WorkflowRevision: binding.workflowRevision, StageName: binding.stage,
		StageInputDigest: binding.input, EvidenceDigest: binding.evidence, PolicyDigest: binding.policy,
		Reviewer: "reviewer@example.test", Reason: "review decision reason",
		IdempotencyKey: reviewCreateID(suffix), At: testNow.Add(3 * time.Second),
	}
	if kind == domain.ReviewRevise {
		target := binding.stage
		request.RevisionTargetStage = &target
	}
	return request
}

func reviewID(suffix int) string {
	return fmt.Sprintf("review_%032x", suffix)
}

func reviewCreateID(suffix int) string {
	return fmt.Sprintf("reviewcreate_%032x", suffix)
}
