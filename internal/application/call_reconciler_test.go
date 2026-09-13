package application_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/domain"
)

type reconciliationReceipt struct {
	execution domain.PhysicalExecution[string]
	err       error
	reads     int
}

func (r *reconciliationReceipt) Replay(_ context.Context, _ domain.DispatchGrant) (domain.PhysicalExecution[string], error) {
	r.reads++
	return r.execution, r.err
}

func TestCallReconcilerClosesUnstartedCallsWithoutPlanningOrDispatch(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		f := newCoordinatorFixture(t, "c1", domain.BudgetLimits{MaxLLMCalls: 2, MaxLLMInputTokens: 20, MaxLLMOutputTokens: 20, MaxLLMCostMicroUSD: 20})
		ctx := context.Background()
		open := f.openRequest(2501)
		if _, err := f.store.OpenCall(ctx, open); err != nil {
			t.Fatal(err)
		}
		if prepared {
			prepareReconciliationCalls(t, f, open)
		}
		_, err := f.store.RequestCancel(ctx, domain.CancelRequest{ID: "control_00000000000000000000000000002501", RunID: f.runID, ExpectedRunVersion: 2, Reason: "terminal cleanup", IdempotencyKey: coordinatorID("cancel", "reconciler"), At: f.clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		receipt := &reconciliationReceipt{err: errors.New("unstarted call must not read a receipt")}
		// The facade has no opening, planning or dispatch-start methods.
		ledger := struct {
			application.CallReconciliationLedger
		}{f.store}
		service, err := application.NewCallReconciler[string](ledger, receipt, f.clock)
		if err != nil {
			t.Fatal(err)
		}
		first, err := service.Reconcile(ctx, open)
		if err != nil || first.Failure == nil || first.CallTrace.DispatchKind != domain.DispatchNone || receipt.reads != 0 || len(f.clock.delays()) != 0 {
			t.Fatalf("cleanup=%+v err=%v receipt reads=%d", first, err, receipt.reads)
		}
		second, err := service.Reconcile(ctx, open)
		if err != nil || !reflect.DeepEqual(first, second) {
			t.Fatalf("cleanup replay: %v", err)
		}
		stored, err := f.store.LoadCall(ctx, open.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, reservation := range stored.Reservations {
			if reservation.State != domain.ReservationReleased || reservation.SettledValue == nil || *reservation.SettledValue != 0 {
				t.Fatalf("unstarted work consumed budget: %+v", reservation)
			}
		}
	}
}

func TestCallReconcilerSettlesOnlyExistingDispatchAndNeverStartsNextRetry(t *testing.T) {
	for _, mode := range []string{"receipt", "unknown", "completed_failure", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			f := newCoordinatorFixture(t, "c2", domain.BudgetLimits{MaxLLMCalls: 2, MaxLLMInputTokens: 20, MaxLLMOutputTokens: 20, MaxLLMCostMicroUSD: 20})
			ctx := context.Background()
			open := f.openRequest(2502)
			if _, err := f.store.OpenCall(ctx, open); err != nil {
				t.Fatal(err)
			}
			calls := prepareReconciliationCalls(t, f, open)
			grant, err := f.store.BeginDispatch(ctx, domain.BeginDispatchRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: open.StageName, AttemptID: f.attemptID, CallRecordID: open.ID, AttemptCallID: calls.PhysicalCalls[0].ID, IdempotencyKey: coordinatorID("begin", "reconcile-original"), At: f.clock.Now()})
			if err != nil {
				t.Fatal(err)
			}
			receipt := &reconciliationReceipt{execution: domain.PhysicalExecution[string]{Boundary: domain.BoundaryCompleted, Value: stringPointer("verified"), ProviderRequestID: "original-provider", ResponseDigest: coordinatorDigestPointer("original-response")}}
			if mode == "unknown" {
				receipt.execution = domain.PhysicalExecution[string]{Boundary: domain.BoundaryUnknown, Failure: &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}}
			}
			if mode == "unreadable" {
				receipt.err = errors.New("receipt storage temporarily unavailable")
			}
			if mode == "completed_failure" {
				if err := f.store.MarkSent(ctx, grant, f.clock.Now()); err != nil {
					t.Fatal(err)
				}
				err := f.store.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: open.StageName, AttemptID: f.attemptID, CallRecordID: open.ID, AttemptCallID: grant.AttemptCallID, State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeRetryableFailure, Failure: &domain.PortFailure{Code: domain.FailureUnavailable, Class: domain.FailureRetryable}, ProviderRequestID: "original-failure", ResponseDigest: coordinatorDigestPointer("failure"), IdempotencyKey: coordinatorID("complete", "known-retry-failure"), At: f.clock.Now()})
				if err != nil {
					t.Fatal(err)
				}
				receipt.err = errors.New("known failure must not read or retry")
			}
			_, err = f.store.RequestCancel(ctx, domain.CancelRequest{ID: "control_00000000000000000000000000002502", RunID: f.runID, ExpectedRunVersion: 2, Reason: "settle original boundary", IdempotencyKey: coordinatorID("cancel", "reconcile-dispatched"), At: f.clock.Now()})
			if err != nil {
				t.Fatal(err)
			}
			service, err := application.NewCallReconciler[string](f.store, receipt, f.clock)
			if err != nil {
				t.Fatal(err)
			}
			bad := open
			bad.PolicyDigest = domain.SumBytes([]byte("changed policy"))
			if _, err := service.Reconcile(ctx, bad); err == nil || receipt.reads != 0 {
				t.Fatal("changed identity reached receipt recovery")
			}
			outcome, err := service.Reconcile(ctx, open)
			stored, loadErr := f.store.LoadCall(ctx, open.ID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			if mode == "unreadable" {
				if err == nil || stored.Call.State != domain.CallRecordPrepared || stored.PhysicalCalls[0].State != domain.PhysicalDispatching || stored.PhysicalCalls[1].State != domain.PhysicalPrepared {
					t.Fatalf("unreadable receipt was discarded: %+v %v", stored, err)
				}
				return
			}
			if err != nil || outcome.Validate() != nil || outcome.CallTrace.DispatchKind != domain.DispatchDispatched || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 1 || stored.PhysicalCalls[1].State != domain.PhysicalAbortedNoDispatch || len(f.clock.delays()) != 0 {
				t.Fatalf("cleanup=%+v err=%v", outcome, err)
			}
			if mode == "receipt" && (outcome.Value == nil || *outcome.Value != "verified") {
				t.Fatal("original response lost")
			}
			if mode == "completed_failure" && (receipt.reads != 0 || outcome.Failure == nil || outcome.Failure.Code != domain.FailureUnavailable) {
				t.Fatal("known failure was retried")
			}
			replayed, err := service.Reconcile(ctx, open)
			if err != nil || !reflect.DeepEqual(outcome, replayed) {
				t.Fatalf("terminal reconciliation changed: %v", err)
			}
		})
	}
}

func prepareReconciliationCalls(t *testing.T, f coordinatorFixture, open domain.OpenCallRequest) domain.PreparedCalls {
	t.Helper()
	calls, err := f.store.PrepareCalls(context.Background(), domain.PrepareCallsRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: open.StageName, AttemptID: f.attemptID, CallRecordID: open.ID, PlanDigest: domain.SumBytes([]byte("reconciliation plan")), Calls: []domain.PhysicalCallPlan{coordinatorPhysicalPlan(1, 1), coordinatorPhysicalPlan(2, 2)}, IdempotencyKey: coordinatorID("prepare", "reconciliation"), At: f.clock.Now()})
	if err != nil || calls.Failure != nil {
		t.Fatalf("prepare=%+v err=%v", calls, err)
	}
	return calls
}
