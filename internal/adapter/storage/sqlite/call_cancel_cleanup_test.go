package sqlite

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"cpgen/internal/domain"
)

func TestOpenCallNoSendCleanupSurvivesPendingCancellation(t *testing.T) {
	for _, kind := range []domain.CallKind{domain.CallLLMGenerate, domain.CallSimilaritySearch, domain.CallCacheReuse} {
		t.Run(string(kind), func(t *testing.T) {
			f := newMeteringFixture(t, "ca", domain.BudgetLimits{})
			ctx := context.Background()
			call := mustOpenMeteringCall(t, f, 2401, kind)
			_, err := f.store.RequestCancel(ctx, domain.CancelRequest{ID: "control_00000000000000000000000000002401", RunID: f.runID, ExpectedRunVersion: 2, Reason: "stop before call planning", IdempotencyKey: meteringID("cancel", "open-call"), At: f.now})
			if err != nil {
				t.Fatal(err)
			}
			command := domain.FinishCallRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID, CallRecordID: call.ID, DispatchKind: domain.DispatchNone, Failure: &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}, IdempotencyKey: meteringID("finish", "open-call-cancel"), At: f.now}
			if kind == domain.CallCacheReuse {
				cache := command
				source := domain.CallRecordID("callrec_00000000000000000000000000002400")
				cache.ExpectedRunVersion, cache.DispatchKind, cache.Failure = 3, domain.DispatchCacheHit, nil
				cache.CacheSourceCallRecordID, cache.CacheHitCallRecordID = &source, &call.ID
				if _, err := f.store.FinishCall(ctx, cache); !errors.Is(err, ErrCancelPending) {
					t.Fatalf("cancel admitted cache reuse: %v", err)
				}
			}
			trace, err := f.store.FinishCall(ctx, command)
			if err != nil || trace.DispatchKind != domain.DispatchNone || len(trace.PhysicalAttemptCallIDs) != 0 {
				t.Fatalf("no-send cleanup=%+v err=%v", trace, err)
			}
			replayed, err := f.store.FinishCall(ctx, command)
			if err != nil || !reflect.DeepEqual(trace, replayed) {
				t.Fatalf("cleanup replay=%+v err=%v", replayed, err)
			}
			stored, err := f.store.LoadCall(ctx, call.ID)
			if err != nil || stored.Call.State != domain.CallRecordTerminal || len(stored.PhysicalCalls) != 0 || len(stored.Reservations) != 0 {
				t.Fatalf("cleanup created effects: %+v %v", stored, err)
			}
			fresh := openCallRequest(f, 2402, kind)
			fresh.ExpectedRunVersion = 3
			if _, err := f.store.OpenCall(ctx, fresh); !errors.Is(err, ErrCancelPending) {
				t.Fatalf("cancel admitted new logical work: %v", err)
			}
		})
	}
}

func TestOpenCallCleanupRejectsObsoleteAttemptAndForgedDispatch(t *testing.T) {
	f := newMeteringFixture(t, "cb", domain.BudgetLimits{})
	ctx := context.Background()
	call := mustOpenMeteringCall(t, f, 2403, domain.CallLLMGenerate)
	command := domain.FinishCallRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID, CallRecordID: call.ID, DispatchKind: domain.DispatchNone, Failure: &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}, IdempotencyKey: meteringID("finish", "invalid-open-cleanup"), At: f.now}
	bad := command
	bad.AttemptID = "attempt_00000000000000000000000000002403"
	if _, err := f.store.FinishCall(ctx, bad); !errors.Is(err, ErrConsistency) {
		t.Fatalf("foreign cleanup admitted: %v", err)
	}
	resultID := domain.AttemptCallID("call_00000000000000000000000000002403")
	bad = command
	bad.DispatchKind, bad.ResultAttemptCallID = domain.DispatchDispatched, &resultID
	if _, err := f.store.FinishCall(ctx, bad); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("forged dispatch admitted: %v", err)
	}
	stored, err := f.store.ReadLogicalCall(ctx, call.ID)
	if err != nil || stored.State != domain.CallRecordOpen {
		t.Fatalf("rejected cleanup changed call: %+v %v", stored, err)
	}
	_, err = f.store.InterruptStage(ctx, domain.InterruptStageCommand{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID, Cause: domain.CauseStepDeadline, IdempotencyKey: meteringID("interrupt", "open-cleanup"), At: f.now})
	if err != nil {
		t.Fatal(err)
	}
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	command.ExpectedRunVersion = current.Version
	if _, err := f.store.FinishCall(ctx, command); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("obsolete attempt cleanup admitted: %v", err)
	}
}
