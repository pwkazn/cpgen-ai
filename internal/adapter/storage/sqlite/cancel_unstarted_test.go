package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func requestUnstartedCancellation(t *testing.T, store *Store, run domain.RunSnapshot) domain.CancelUnstartedCommand {
	t.Helper()
	control, err := store.RequestCancel(context.Background(), domain.CancelRequest{
		ID: "control_000000000000000000000000000001c1", RunID: run.RunID, ExpectedRunVersion: run.Version,
		Reason: "cancel without dispatch", IdempotencyKey: "cancel_000000000000000000000000000001c1", At: testNow.Add(time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	return domain.CancelUnstartedCommand{RunID: run.RunID, ExpectedRunVersion: control.RunVersion, ControlRequestID: control.ID,
		IdempotencyKey: "cancelunstarted_000000000000000000000000000001c1", At: testNow.Add(2 * time.Second)}
}

func TestTryFinalizeUnstartedCancelCommitsOnceAndChecksIdentity(t *testing.T) {
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "unstarted.db"), clock.NewFake(testNow))
	run := mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, time.Minute))
	command := requestUnstartedCancellation(t, store, run)
	for _, change := range []struct {
		name string
		edit func(*domain.CancelUnstartedCommand)
		want error
	}{
		{"stale version", func(c *domain.CancelUnstartedCommand) { c.ExpectedRunVersion-- }, ErrVersionConflict},
		{"different control", func(c *domain.CancelUnstartedCommand) {
			c.ControlRequestID = "control_000000000000000000000000000001c2"
		}, ErrConsistency},
	} {
		t.Run(change.name, func(t *testing.T) {
			wrong := command
			change.edit(&wrong)
			if _, handled, err := store.TryFinalizeUnstartedCancel(ctx, wrong); handled || !errors.Is(err, change.want) {
				t.Fatalf("handled=%v err=%v, want %v", handled, err, change.want)
			}
		})
	}
	cancelled, handled, err := store.TryFinalizeUnstartedCancel(ctx, command)
	if err != nil || !handled || cancelled.State != domain.RunCancelled || cancelled.Version != command.ExpectedRunVersion+1 {
		t.Fatalf("cancelled=%+v handled=%v err=%v", cancelled, handled, err)
	}
	again, handled, err := store.TryFinalizeUnstartedCancel(ctx, command)
	if err != nil || !handled || !reflect.DeepEqual(cancelled, again) {
		t.Fatalf("replayed=%+v handled=%v err=%v", again, handled, err)
	}
	changed := command
	changed.At = changed.At.Add(time.Second)
	if _, _, err := store.TryFinalizeUnstartedCancel(ctx, changed); !errors.Is(err, ErrConsistency) {
		t.Fatalf("changed replay=%v", err)
	}
	pending, err := store.PendingCancel(ctx, run.RunID)
	if err != nil || pending != nil {
		t.Fatalf("pending=%+v err=%v", pending, err)
	}
	events, err := store.Events(ctx, run.RunID, 0)
	if err != nil || len(events) != 3 || events[2].Type != domain.EventCancelFinalized {
		t.Fatalf("events=%+v err=%v", events, err)
	}
}

func TestTryFinalizeUnstartedCancelPreservesInterruptedExecutionEvidence(t *testing.T) {
	for _, boundary := range []string{"attempt", "open call", "reservation", "sandbox"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			f := unsentSandboxFixture(t, "prepare")
			if boundary != "attempt" {
				call := mustOpenMeteringCall(t, f, 1, domain.CallSandboxCompile)
				if boundary != "open call" {
					prepared, err := f.store.PrepareCalls(ctx, prepareOneRequest(f, call, 1, domain.PhysicalDockerContainerCreate, domain.BudgetDockerContainerCreates, 1))
					if err != nil {
						t.Fatal(err)
					}
					if boundary == "sandbox" {
						retainUnsentExecution(t, f, prepared.PhysicalCalls[0].ID, false)
					}
				}
			}
			// Interrupted runs legitimately return to CREATED while retaining
			// durable attempts and possibly cleanup obligations.
			run, err := f.store.InterruptStage(ctx, domain.InterruptStageCommand{
				RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID,
				Cause: domain.CauseRevisionInvalidated, IdempotencyKey: meteringID("test", "cancel-unstarted-interrupt"), At: testNow,
			})
			if err != nil || run.State != domain.RunCreated {
				t.Fatalf("interrupted=%+v err=%v", run, err)
			}
			command := requestUnstartedCancellation(t, f.store, run)
			before, err := f.store.GetRun(ctx, run.RunID)
			if err != nil {
				t.Fatal(err)
			}
			budgetBefore, err := f.store.BudgetSnapshot(ctx, run.RunID)
			if err != nil {
				t.Fatal(err)
			}
			result, handled, err := f.store.TryFinalizeUnstartedCancel(ctx, command)
			if err != nil || handled || !reflect.DeepEqual(result, before) {
				t.Fatalf("unsafe shortcut: %+v handled=%v err=%v", result, handled, err)
			}
			budgetAfter, err := f.store.BudgetSnapshot(ctx, run.RunID)
			if err != nil || !reflect.DeepEqual(budgetBefore, budgetAfter) {
				t.Fatalf("budget changed: %+v err=%v", budgetAfter, err)
			}
			pending, err := f.store.PendingCancel(ctx, run.RunID)
			if err != nil || pending == nil || pending.ID != command.ControlRequestID {
				t.Fatalf("cancel must remain pending: %+v err=%v", pending, err)
			}
		})
	}
}

func TestTryFinalizeUnstartedCancelRequiresEmptyLedgersDespiteCreatedProjection(t *testing.T) {
	for _, boundary := range []string{"historical attempt", "reserved budget", "consumed budget"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			store := openRuntimeStore(t, filepath.Join(t.TempDir(), "proof.db"), clock.NewFake(testNow))
			run := mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, time.Minute))
			var err error
			switch boundary {
			case "historical attempt":
				_, err = store.db.ExecContext(ctx, `INSERT INTO stage_attempts
					(attempt_id,run_id,stage_name,ordinal,state,input_digest,cause,started_at,finished_at)
					VALUES (?,?,'prepare',1,'INTERRUPTED',?,'revision_invalidated',?,?)`,
					testAttemptID, testRunID, domain.SumBytes([]byte("historical input")), formatTime(testNow), formatTime(testNow))
			case "reserved budget":
				_, err = store.db.ExecContext(ctx, `UPDATE budget_accounts SET reserved_value=1 WHERE run_id=? AND dimension='LLM_CALLS'`, run.RunID)
			case "consumed budget":
				_, err = store.db.ExecContext(ctx, `UPDATE budget_accounts SET consumed_value=1 WHERE run_id=? AND dimension='LLM_CALLS'`, run.RunID)
			}
			if err != nil {
				t.Fatal(err)
			}
			command := requestUnstartedCancellation(t, store, run)
			got, handled, err := store.TryFinalizeUnstartedCancel(ctx, command)
			if err != nil || handled || got.State != domain.RunCreated || got.Version != command.ExpectedRunVersion {
				t.Fatalf("unsafe shortcut: %+v handled=%v err=%v", got, handled, err)
			}
		})
	}
}
