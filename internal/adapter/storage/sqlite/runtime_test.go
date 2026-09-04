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

var (
	testRunID     = domain.RunID("run_00000000000000000000000000000001")
	testAttemptID = domain.AttemptID("attempt_00000000000000000000000000000001")
	testNow       = time.Date(2026, 9, 1, 8, 0, 0, 0, time.UTC)
)

// TestRuntimeCreateRunIsAtomicIdempotentAndPersistent catches half-created
// fixed pipelines, mutable create-key replay, and projections that vanish on
// process restart.
func TestRuntimeCreateRunIsAtomicIdempotentAndPersistent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	store := openRuntimeStore(t, path, clock.NewFake(testNow))
	request := testCreateRunRequest(testRunID, testNow, 30*time.Second)

	created, err := store.CreateRun(ctx, request)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if created.State != domain.RunCreated || created.Version != 1 || created.CurrentStage != "prepare" || created.CurrentStageOrdinal != 1 {
		t.Fatalf("created snapshot = %+v", created)
	}
	var stages, attempts, events int
	for query, target := range map[string]*int{
		"SELECT count(*) FROM stage_records WHERE run_id = ?":  &stages,
		"SELECT count(*) FROM stage_attempts WHERE run_id = ?": &attempts,
		"SELECT count(*) FROM run_events WHERE run_id = ?":     &events,
	} {
		if err := store.db.QueryRowContext(ctx, query, string(testRunID)).Scan(target); err != nil {
			t.Fatalf("projection count %q: %v", query, err)
		}
	}
	if stages != 3 || attempts != 0 || events != 1 {
		t.Fatalf("atomic projection counts = stages:%d attempts:%d events:%d", stages, attempts, events)
	}

	replayed, err := store.CreateRun(ctx, request)
	if err != nil {
		t.Fatalf("CreateRun replay: %v", err)
	}
	if !reflect.DeepEqual(replayed, created) {
		t.Fatalf("CreateRun replay = %+v, want %+v", replayed, created)
	}
	drifted := request
	drifted.WorkflowDigest = domain.SumBytes([]byte("different workflow"))
	if _, err := store.CreateRun(ctx, drifted); !errors.Is(err, ErrConsistency) {
		t.Fatalf("CreateRun changed replay = %v, want ErrConsistency", err)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}
	reopened := openRuntimeStore(t, path, clock.NewFake(testNow.Add(time.Hour)))
	got, err := reopened.GetRun(ctx, testRunID)
	if err != nil {
		t.Fatalf("GetRun after reopen: %v", err)
	}
	if !reflect.DeepEqual(got, created) {
		t.Fatalf("reopened snapshot = %+v, want %+v", got, created)
	}
	ordered, err := reopened.Events(ctx, testRunID, 0)
	if err != nil {
		t.Fatalf("Events after reopen: %v", err)
	}
	if len(ordered) != 1 || ordered[0].Version != 1 || ordered[0].Type != domain.EventRunCreated {
		t.Fatalf("reopened events = %+v", ordered)
	}
}

// TestRuntimeBeginFinishAndEventOrdering catches a stage projection update that
// is not atomically paired with its append-only run-version event.
func TestRuntimeBeginFinishAndEventOrdering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	created := mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, 30*time.Second))
	input := domain.SumBytes([]byte("prepare input"))
	begin := domain.BeginStageCommand{
		RunID: testRunID, ExpectedRunVersion: created.Version, StageName: "prepare",
		AttemptID: testAttemptID, InputDigest: input,
		IdempotencyKey: "begin_00000000000000000000000000000001", At: testNow.Add(time.Second),
	}
	attempt, err := store.BeginStage(ctx, begin)
	if err != nil {
		t.Fatalf("BeginStage: %v", err)
	}
	if attempt.State != domain.StageAttemptRunning || attempt.Ordinal != 1 || attempt.InputDigest != input {
		t.Fatalf("attempt = %+v", attempt)
	}
	if replayed, err := store.BeginStage(ctx, begin); err != nil || !reflect.DeepEqual(replayed, attempt) {
		t.Fatalf("BeginStage replay = %+v, %v", replayed, err)
	}
	output := domain.SumBytes([]byte("prepare output"))
	finished, err := store.FinishStage(ctx, domain.FinishStageCommand{
		RunID: testRunID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: testAttemptID,
		AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning,
		OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output,
		IdempotencyKey: "finish_00000000000000000000000000000001", At: testNow.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("FinishStage: %v", err)
	}
	if finished.Version != 3 || finished.State != domain.RunRunning || finished.CurrentStage != "exercise" || finished.CurrentStageOrdinal != 2 {
		t.Fatalf("finished snapshot = %+v", finished)
	}
	events, err := store.Events(ctx, testRunID, 0)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	wantTypes := []domain.RunEventType{domain.EventRunCreated, domain.EventStageBegan, domain.EventStageFinished}
	if len(events) != len(wantTypes) {
		t.Fatalf("event count = %d, want %d", len(events), len(wantTypes))
	}
	for index := range events {
		if events[index].Version != int64(index+1) || events[index].Type != wantTypes[index] {
			t.Fatalf("event %d = %+v", index, events[index])
		}
	}
	after, err := store.Events(ctx, testRunID, 1)
	if err != nil {
		t.Fatalf("Events after version: %v", err)
	}
	if len(after) != 2 || after[0].Version != 2 {
		t.Fatalf("events after 1 = %+v", after)
	}
}

// TestRuntimeRejectsVersionConflictsAndReady catches stale writers and any
// attempt to make READY reachable before package verification exists.
func TestRuntimeRejectsVersionConflictsAndReady(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, 30*time.Second))
	input := domain.SumBytes([]byte("input"))
	if _, err := store.BeginStage(ctx, domain.BeginStageCommand{
		RunID: testRunID, ExpectedRunVersion: 99, StageName: "prepare", AttemptID: testAttemptID,
		InputDigest: input, IdempotencyKey: "begin_00000000000000000000000000000002", At: testNow.Add(time.Second),
	}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale BeginStage = %v, want ErrVersionConflict", err)
	}
	mustBeginStage(t, store, testRunID, testAttemptID, 1, "prepare", input, testNow.Add(time.Second), "begin_00000000000000000000000000000003")
	output := domain.SumBytes([]byte("output"))
	if _, err := store.FinishStage(ctx, domain.FinishStageCommand{
		RunID: testRunID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: testAttemptID,
		AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunReady, OutputDigest: &output,
		IdempotencyKey: "finish_00000000000000000000000000000002", At: testNow.Add(2 * time.Second),
	}); !errors.Is(err, domain.ErrStageBoundary) {
		t.Fatalf("FinishStage READY = %v, want ErrStageBoundary", err)
	}
}

// TestRuntimeInterruptsRunningAttemptForResume catches overwriting the old
// attempt or resuming it in place instead of preserving INTERRUPTED audit.
func TestRuntimeInterruptsRunningAttemptForResume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, 30*time.Second))
	input := domain.SumBytes([]byte("resume input"))
	mustBeginStage(t, store, testRunID, testAttemptID, 1, "prepare", input, testNow.Add(time.Second), "begin_00000000000000000000000000000004")
	resumed, err := store.InterruptStage(ctx, domain.InterruptStageCommand{
		RunID: testRunID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: testAttemptID,
		Cause:          domain.CauseRevisionInvalidated,
		IdempotencyKey: "interrupt_00000000000000000000000000000001", At: testNow.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("InterruptStage: %v", err)
	}
	if resumed.State != domain.RunCreated || resumed.Version != 3 || resumed.CurrentStage != "prepare" {
		t.Fatalf("interrupted run = %+v", resumed)
	}
	var state string
	var finishedAt *string
	if err := store.db.QueryRowContext(ctx, "SELECT state, finished_at FROM stage_attempts WHERE attempt_id = ?", string(testAttemptID)).Scan(&state, &finishedAt); err != nil {
		t.Fatalf("load interrupted attempt: %v", err)
	}
	if state != string(domain.StageAttemptInterrupted) || finishedAt == nil {
		t.Fatalf("old attempt = state:%q finished:%v", state, finishedAt)
	}
	newAttemptID := domain.AttemptID("attempt_00000000000000000000000000000002")
	newAttempt := mustBeginStage(t, store, testRunID, newAttemptID, 3, "prepare", input, testNow.Add(3*time.Second), "begin_00000000000000000000000000000005")
	if newAttempt.Ordinal != 2 {
		t.Fatalf("retry attempt ordinal = %d, want 2", newAttempt.Ordinal)
	}
}

// TestCancelWinsOrTerminalCommitWinsBySQLiteOrder catches a cancellation side
// channel that can overwrite a terminal result or be ignored after committing.
func TestCancelWinsOrTerminalCommitWinsBySQLiteOrder(t *testing.T) {
	t.Parallel()
	t.Run("cancel commits first", func(t *testing.T) {
		ctx := context.Background()
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
		mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, 30*time.Second))
		input := domain.SumBytes([]byte("cancel input"))
		mustBeginStage(t, store, testRunID, testAttemptID, 1, "prepare", input, testNow.Add(time.Second), "begin_00000000000000000000000000000006")
		control, err := store.RequestCancel(ctx, domain.CancelRequest{
			ID: "control_00000000000000000000000000000001", RunID: testRunID, ExpectedRunVersion: 2,
			Reason: "operator requested cancellation", IdempotencyKey: "cancel_00000000000000000000000000000001", At: testNow.Add(2 * time.Second),
		})
		if err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		if !control.Active {
			t.Fatalf("control not active: %+v", control)
		}
		output := domain.SumBytes([]byte("late success"))
		_, err = store.FinishStage(ctx, domain.FinishStageCommand{
			RunID: testRunID, ExpectedRunVersion: 3, StageName: "prepare", AttemptID: testAttemptID,
			AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning,
			OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output,
			IdempotencyKey: "finish_00000000000000000000000000000003", At: testNow.Add(3 * time.Second),
		})
		if !errors.Is(err, ErrCancelPending) {
			t.Fatalf("late terminal result = %v, want ErrCancelPending", err)
		}
		cancelled, err := store.FinishStage(ctx, domain.FinishStageCommand{
			RunID: testRunID, ExpectedRunVersion: 3, StageName: "prepare", AttemptID: testAttemptID,
			AttemptState: domain.StageAttemptCancelled, RunState: domain.RunCancelled, Cause: ptrCause(domain.CauseUserCancel),
			IdempotencyKey: "finish_00000000000000000000000000000004", At: testNow.Add(4 * time.Second),
		})
		if err != nil {
			t.Fatalf("finish cancellation: %v", err)
		}
		if cancelled.State != domain.RunCancelled || cancelled.CancelSummary != "operator requested cancellation" {
			t.Fatalf("cancelled snapshot = %+v", cancelled)
		}
	})

	t.Run("terminal commits first", func(t *testing.T) {
		ctx := context.Background()
		runID := domain.RunID("run_00000000000000000000000000000002")
		attemptID := domain.AttemptID("attempt_00000000000000000000000000000003")
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
		mustCreateRun(t, store, testCreateRunRequest(runID, testNow, 30*time.Second))
		input := domain.SumBytes([]byte("terminal input"))
		mustBeginStage(t, store, runID, attemptID, 1, "prepare", input, testNow.Add(time.Second), "begin_00000000000000000000000000000007")
		failed, err := store.FinishStage(ctx, domain.FinishStageCommand{
			RunID: runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: attemptID,
			AttemptState: domain.StageAttemptFailed, RunState: domain.RunFailed,
			IdempotencyKey: "finish_00000000000000000000000000000005", At: testNow.Add(2 * time.Second),
		})
		if err != nil || failed.State != domain.RunFailed {
			t.Fatalf("terminal finish = %+v, %v", failed, err)
		}
		_, err = store.RequestCancel(ctx, domain.CancelRequest{
			ID: "control_00000000000000000000000000000002", RunID: runID, ExpectedRunVersion: 3,
			Reason: "too late", IdempotencyKey: "cancel_00000000000000000000000000000002", At: testNow.Add(3 * time.Second),
		})
		if !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("cancel after terminal = %v, want ErrInvalidTransition", err)
		}
	})
}

// TestActiveTimeHeartbeatPauseRecoveryAndCap catches double charging, charging
// offline paused time, unbounded crash compensation, and limit overshoot.
func TestActiveTimeHeartbeatPauseRecoveryAndCap(t *testing.T) {
	t.Parallel()
	t.Run("heartbeat once and paused time excluded", func(t *testing.T) {
		ctx := context.Background()
		source := clock.NewFake(testNow)
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), source)
		mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, 10*time.Second))
		input := domain.SumBytes([]byte("active input"))
		mustBeginStage(t, store, testRunID, testAttemptID, 1, "prepare", input, testNow, "begin_00000000000000000000000000000008")
		start := mustAccount(t, store, domain.ActiveTimeCommand{
			RunID: testRunID, ExpectedRunVersion: 2, Action: domain.ActiveTimeStart,
			IdempotencyKey: "active_00000000000000000000000000000001", At: source.Now(),
		})
		if start.ActiveElapsed != 0 || start.Remaining != 10*time.Second {
			t.Fatalf("active start = %+v", start)
		}
		source.Advance(2 * time.Second)
		heartbeatCommand := domain.ActiveTimeCommand{
			RunID: testRunID, ExpectedRunVersion: 3, Action: domain.ActiveTimeHeartbeat,
			IdempotencyKey: "active_00000000000000000000000000000002", At: source.Now(),
		}
		heartbeat := mustAccount(t, store, heartbeatCommand)
		if heartbeat.ActiveElapsed != 2*time.Second || heartbeat.Remaining != 8*time.Second {
			t.Fatalf("heartbeat = %+v", heartbeat)
		}
		replay := mustAccount(t, store, heartbeatCommand)
		if !reflect.DeepEqual(replay, heartbeat) {
			t.Fatalf("heartbeat replay = %+v, want %+v", replay, heartbeat)
		}
		source.Advance(time.Second)
		stopped := mustAccount(t, store, domain.ActiveTimeCommand{
			RunID: testRunID, ExpectedRunVersion: 4, Action: domain.ActiveTimeStop,
			IdempotencyKey: "active_00000000000000000000000000000003", At: source.Now(),
		})
		if stopped.ActiveElapsed != 3*time.Second || stopped.Active {
			t.Fatalf("stopped active time = %+v", stopped)
		}
		blocked, err := store.FinishStage(ctx, domain.FinishStageCommand{
			RunID: testRunID, ExpectedRunVersion: 5, StageName: "prepare", AttemptID: testAttemptID,
			AttemptState: domain.StageAttemptBlocked, RunState: domain.RunBlocked,
			IdempotencyKey: "finish_00000000000000000000000000000006", At: source.Now(),
		})
		if err != nil || blocked.State != domain.RunBlocked {
			t.Fatalf("block run = %+v, %v", blocked, err)
		}
		source.Advance(time.Hour)
		got, err := store.GetRun(ctx, testRunID)
		if err != nil {
			t.Fatalf("GetRun paused: %v", err)
		}
		if got.ActiveElapsed != 3*time.Second {
			t.Fatalf("paused elapsed = %v, want 3s", got.ActiveElapsed)
		}
	})

	t.Run("crash recovery is bounded by one heartbeat", func(t *testing.T) {
		ctx := context.Background()
		runID := domain.RunID("run_00000000000000000000000000000003")
		attemptID := domain.AttemptID("attempt_00000000000000000000000000000004")
		source := clock.NewFake(testNow)
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), source)
		mustCreateRun(t, store, testCreateRunRequest(runID, testNow, time.Minute))
		input := domain.SumBytes([]byte("crash input"))
		mustBeginStage(t, store, runID, attemptID, 1, "prepare", input, testNow, "begin_00000000000000000000000000000009")
		mustAccount(t, store, domain.ActiveTimeCommand{RunID: runID, ExpectedRunVersion: 2, Action: domain.ActiveTimeStart, IdempotencyKey: "active_00000000000000000000000000000004", At: source.Now()})
		source.Advance(2 * time.Second)
		mustAccount(t, store, domain.ActiveTimeCommand{RunID: runID, ExpectedRunVersion: 3, Action: domain.ActiveTimeHeartbeat, IdempotencyKey: "active_00000000000000000000000000000005", At: source.Now()})
		source.Advance(time.Hour)
		recovered := mustAccount(t, store, domain.ActiveTimeCommand{
			RunID: runID, ExpectedRunVersion: 4, Action: domain.ActiveTimeRecover, HeartbeatInterval: 5 * time.Second,
			IdempotencyKey: "active_00000000000000000000000000000006", At: source.Now(),
		})
		if recovered.ActiveElapsed != 7*time.Second || recovered.Active {
			t.Fatalf("recovered elapsed = %+v, want 7s and closed", recovered)
		}
		_ = ctx
	})

	t.Run("accumulation caps exactly at immutable limit", func(t *testing.T) {
		runID := domain.RunID("run_00000000000000000000000000000004")
		attemptID := domain.AttemptID("attempt_00000000000000000000000000000005")
		source := clock.NewFake(testNow)
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), source)
		mustCreateRun(t, store, testCreateRunRequest(runID, testNow, 5*time.Second))
		input := domain.SumBytes([]byte("cap input"))
		mustBeginStage(t, store, runID, attemptID, 1, "prepare", input, testNow, "begin_00000000000000000000000000000010")
		mustAccount(t, store, domain.ActiveTimeCommand{RunID: runID, ExpectedRunVersion: 2, Action: domain.ActiveTimeStart, IdempotencyKey: "active_00000000000000000000000000000007", At: source.Now()})
		source.Advance(20 * time.Second)
		capped := mustAccount(t, store, domain.ActiveTimeCommand{RunID: runID, ExpectedRunVersion: 3, Action: domain.ActiveTimeHeartbeat, IdempotencyKey: "active_00000000000000000000000000000008", At: source.Now()})
		if capped.ActiveElapsed != 5*time.Second || capped.Remaining != 0 || !capped.Exhausted {
			t.Fatalf("capped result = %+v", capped)
		}
	})
}

// TestRuntimePersistsExplicitWorkflowAndSchemaBindings catches deriving the
// revision from a digest or omitting compatibility bindings from stage rows.
func TestRuntimePersistsExplicitWorkflowAndSchemaBindings(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	request := testCreateRunRequest(testRunID, testNow, 30*time.Second)
	request.WorkflowRevision = "compiled-fake/v7"
	created := mustCreateRun(t, store, request)
	if created.WorkflowRevision != request.WorkflowRevision || created.SchemaVersion != request.SchemaVersion {
		t.Fatalf("created bindings = revision:%q schema:%q", created.WorkflowRevision, created.SchemaVersion)
	}
	var mismatched int
	if err := store.db.QueryRowContext(ctx, `
		SELECT count(*) FROM stage_records
		WHERE run_id = ? AND (workflow_revision <> ? OR schema_version <> ?)`,
		string(testRunID), request.WorkflowRevision, string(request.SchemaVersion),
	).Scan(&mismatched); err != nil {
		t.Fatalf("query stage bindings: %v", err)
	}
	if mismatched != 0 {
		t.Fatalf("stage rows with mismatched bindings = %d", mismatched)
	}
	invalid := testCreateRunRequest("run_00000000000000000000000000000021", testNow, time.Second)
	invalid.SchemaVersion = "cpgen.request/v2"
	invalid.IdempotencyKey = "create_00000000000000000000000000000021"
	if _, err := store.CreateRun(ctx, invalid); err == nil {
		t.Fatal("CreateRun accepted schema version detached from canonical request JSON")
	}
}

// TestFinalizeCancelHandlesPausedAndInterruptedRuns catches active CANCEL rows
// that can never reach a named, versioned terminal transition after external
// reconciliation has completed.
func TestFinalizeCancelHandlesPausedAndInterruptedRuns(t *testing.T) {
	t.Parallel()
	for _, state := range []domain.RunState{domain.RunCreated, domain.RunBlocked, domain.RunNeedsReview} {
		state := state
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			suffix := map[domain.RunState]string{
				domain.RunCreated: "22", domain.RunBlocked: "23", domain.RunNeedsReview: "24",
			}[state]
			suffixNumber := map[domain.RunState]int{
				domain.RunCreated: 22, domain.RunBlocked: 23, domain.RunNeedsReview: 24,
			}[state]
			runID := domain.RunID("run_000000000000000000000000000000" + suffix)
			attemptID := domain.AttemptID("attempt_000000000000000000000000000000" + suffix)
			store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
			request := testCreateRunRequest(runID, testNow, 30*time.Second)
			request.IdempotencyKey = "create_000000000000000000000000000000" + suffix
			snapshot := mustCreateRun(t, store, request)
			if state != domain.RunCreated {
				input := domain.SumBytes([]byte("cancel pause " + suffix))
				mustBeginStage(t, store, runID, attemptID, 1, "prepare", input, testNow.Add(time.Second), "begin_000000000000000000000000000000"+suffix)
				finish := domain.FinishStageCommand{
					RunID: runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: attemptID,
					IdempotencyKey: "finish_000000000000000000000000000000" + suffix,
					At:             testNow.Add(2 * time.Second),
				}
				if state == domain.RunBlocked {
					finish.AttemptState, finish.RunState = domain.StageAttemptBlocked, domain.RunBlocked
				} else {
					evidence := domain.SumBytes([]byte("review evidence " + suffix))
					policy := domain.SumBytes([]byte("review policy " + suffix))
					finish.AttemptState, finish.RunState = domain.StageAttemptNeedsReview, domain.RunNeedsReview
					finish.ReviewEvidenceDigest, finish.ReviewPolicyDigest = &evidence, &policy
					finish.ReviewGateWaivable = false
				}
				var err error
				snapshot, err = store.FinishStage(ctx, finish)
				if err != nil {
					t.Fatalf("pause run: %v", err)
				}
			}
			controlID := domain.ControlRequestID("control_000000000000000000000000000000" + suffix)
			control, err := store.RequestCancel(ctx, domain.CancelRequest{
				ID: controlID, RunID: runID, ExpectedRunVersion: snapshot.Version,
				Reason:         "operator cancel " + suffix,
				IdempotencyKey: "cancel_000000000000000000000000000000" + suffix,
				At:             testNow.Add(3 * time.Second),
			})
			if err != nil {
				t.Fatalf("RequestCancel: %v", err)
			}
			command := domain.FinalizeCancelCommand{
				RunID: runID, ExpectedRunVersion: control.RunVersion, ControlRequestID: controlID,
				ReconciliationDigest: domain.SumBytes([]byte("reconciled exact resources " + suffix)),
				IdempotencyKey:       fmt.Sprintf("cancelfinalize_%032x", suffixNumber),
				At:                   testNow.Add(4 * time.Second),
			}
			cancelled, err := store.FinalizeCancel(ctx, command)
			if err != nil {
				t.Fatalf("FinalizeCancel: %v", err)
			}
			if cancelled.State != domain.RunCancelled || cancelled.CancelSummary == "" {
				t.Fatalf("cancelled snapshot = %+v", cancelled)
			}
			replayed, err := store.FinalizeCancel(ctx, command)
			if err != nil || !reflect.DeepEqual(replayed, cancelled) {
				t.Fatalf("FinalizeCancel replay = %+v, %v", replayed, err)
			}
		})
	}

	t.Run("RUNNING requires interruption before finalize", func(t *testing.T) {
		ctx := context.Background()
		runID := domain.RunID("run_00000000000000000000000000000025")
		attemptID := domain.AttemptID("attempt_00000000000000000000000000000025")
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
		request := testCreateRunRequest(runID, testNow, 30*time.Second)
		request.IdempotencyKey = "create_00000000000000000000000000000025"
		mustCreateRun(t, store, request)
		input := domain.SumBytes([]byte("running cancel"))
		mustBeginStage(t, store, runID, attemptID, 1, "prepare", input, testNow.Add(time.Second), "begin_00000000000000000000000000000025")
		control, err := store.RequestCancel(ctx, domain.CancelRequest{
			ID: "control_00000000000000000000000000000025", RunID: runID, ExpectedRunVersion: 2,
			Reason: "stop running", IdempotencyKey: "cancel_00000000000000000000000000000025", At: testNow.Add(2 * time.Second),
		})
		if err != nil {
			t.Fatalf("RequestCancel: %v", err)
		}
		finalize := domain.FinalizeCancelCommand{
			RunID: runID, ExpectedRunVersion: control.RunVersion,
			ControlRequestID:     "control_00000000000000000000000000000025",
			ReconciliationDigest: domain.SumBytes([]byte("resources stopped")),
			IdempotencyKey:       fmt.Sprintf("cancelfinalize_%032x", 25), At: testNow.Add(3 * time.Second),
		}
		if _, err := store.FinalizeCancel(ctx, finalize); !errors.Is(err, ErrInvalidTransition) {
			t.Fatalf("FinalizeCancel while RUNNING = %v, want ErrInvalidTransition", err)
		}
		interrupted, err := store.InterruptStage(ctx, domain.InterruptStageCommand{
			RunID: runID, ExpectedRunVersion: control.RunVersion, StageName: "prepare", AttemptID: attemptID,
			Cause: domain.CauseUserCancel, IdempotencyKey: "interrupt_00000000000000000000000000000025", At: testNow.Add(3 * time.Second),
		})
		if err != nil {
			t.Fatalf("InterruptStage with pending cancel: %v", err)
		}
		finalize.ExpectedRunVersion = interrupted.Version
		finalize.At = testNow.Add(4 * time.Second)
		cancelled, err := store.FinalizeCancel(ctx, finalize)
		if err != nil || cancelled.State != domain.RunCancelled {
			t.Fatalf("FinalizeCancel after interrupt = %+v, %v", cancelled, err)
		}
	})
}

// TestFinishStageRejectsNonMonotonePersistedTime catches an accepted command
// that would make the stored attempt fail its own domain validation.
func TestFinishStageRejectsNonMonotonePersistedTime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, 30*time.Second))
	input := domain.SumBytes([]byte("time input"))
	mustBeginStage(t, store, testRunID, testAttemptID, 1, "prepare", input, testNow.Add(2*time.Second), "begin_00000000000000000000000000000031")
	_, err := store.FinishStage(ctx, domain.FinishStageCommand{
		RunID: testRunID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: testAttemptID,
		AttemptState: domain.StageAttemptFailed, RunState: domain.RunFailed,
		IdempotencyKey: "finish_00000000000000000000000000000031", At: testNow.Add(time.Second),
	})
	if err == nil {
		t.Fatal("FinishStage accepted time before attempt start/current update")
	}
}

// TestWorkflowSQLStateConstraintsRejectDirectAttacks proves callers cannot
// bypass the domain matrices with direct SQL writes.
func TestWorkflowSQLStateConstraintsRejectDirectAttacks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, 30*time.Second))
	digest := string(domain.SumBytes([]byte("attack")))
	for name, statement := range map[string]string{
		"active interval outside running": `UPDATE runs SET active_started_at = '2026-09-01T08:00:00Z', last_accounting_heartbeat_at = '2026-09-01T08:00:00Z' WHERE run_id = '` + string(testRunID) + `'`,
		"empty run workflow revision":     `UPDATE runs SET workflow_revision = '' WHERE run_id = '` + string(testRunID) + `'`,
		"empty stage schema":              `UPDATE stage_records SET schema_version = '' WHERE run_id = '` + string(testRunID) + `' AND stage_name = 'prepare'`,
		"failed review binding":           `UPDATE stage_records SET state = 'FAILED', attempt_count = 1, review_evidence_digest = '` + digest + `', review_policy_digest = '` + digest + `', review_waivable = 1 WHERE run_id = '` + string(testRunID) + `' AND stage_name = 'prepare'`,
		"cancelled review binding":        `UPDATE stage_records SET state = 'CANCELLED', review_evidence_digest = '` + digest + `', review_policy_digest = '` + digest + `', review_waivable = 1 WHERE run_id = '` + string(testRunID) + `' AND stage_name = 'prepare'`,
	} {
		if _, err := store.db.ExecContext(ctx, statement); err == nil {
			t.Fatalf("direct SQL attack %q succeeded", name)
		}
	}
	input := domain.SumBytes([]byte("attempt attack"))
	mustBeginStage(t, store, testRunID, testAttemptID, 1, "prepare", input, testNow.Add(2*time.Second), "begin_00000000000000000000000000000032")
	if _, err := store.db.ExecContext(ctx, `
		UPDATE stage_attempts SET state = 'FAILED', finished_at = ? WHERE attempt_id = ?`,
		formatTime(testNow.Add(time.Second)), string(testAttemptID),
	); err == nil {
		t.Fatal("direct SQL accepted finished_at before started_at")
	}
}

func openRuntimeStore(t *testing.T, path string, source clock.Clock) *Store {
	t.Helper()
	store, err := OpenWithClock(context.Background(), Config{Path: path, BusyTimeout: time.Second, MaxReaders: 3}, source)
	if err != nil {
		t.Fatalf("OpenWithClock: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func testCreateRunRequest(runID domain.RunID, now time.Time, activeLimit time.Duration) domain.CreateRunRequest {
	requestJSON := []byte(`{"brief":"test","schema_version":"cpgen.request/v1"}`)
	configJSON := []byte(`{"schema":"cpgen.config/v1"}`)
	return domain.CreateRunRequest{
		RunID:                runID,
		SubmittedRequestJSON: requestJSON, SubmittedRequestDigest: domain.SumBytes(requestJSON),
		EffectiveSeed:               7,
		RedactedEffectiveConfigJSON: configJSON, RedactedEffectiveConfigDigest: domain.SumBytes(configJSON),
		WorkflowRevision: "slice1/v1", SchemaVersion: "cpgen.request/v1",
		WorkflowDigest: domain.SumBytes([]byte("slice1-workflow-v1")),
		BudgetLimits: domain.BudgetLimits{
			MaxLLMCalls: 3, MaxSimilarityCalls: 2, MaxLLMInputTokens: 1000, MaxLLMOutputTokens: 1000,
			MaxLLMCostMicroUSD: 5000, MaxSandboxCreates: 4, MaxArtifactBytes: 1 << 20,
			MaxPackageBytes: 1 << 20, MaxMutationsPerStage: 2,
			MaxActiveTimeMilliseconds: activeLimit.Milliseconds(),
		},
		StageSequence: []domain.StageName{"prepare", "exercise", "checkpoint"},
		CreatedAt:     now, IdempotencyKey: "create_00000000000000000000000000000001",
	}
}

func mustCreateRun(t *testing.T, store *Store, request domain.CreateRunRequest) domain.RunSnapshot {
	t.Helper()
	snapshot, err := store.CreateRun(context.Background(), request)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return snapshot
}

func mustBeginStage(t *testing.T, store *Store, runID domain.RunID, attemptID domain.AttemptID, expectedVersion int64, stage domain.StageName, input domain.Digest, at time.Time, key string) domain.StageAttempt {
	t.Helper()
	attempt, err := store.BeginStage(context.Background(), domain.BeginStageCommand{
		RunID: runID, ExpectedRunVersion: expectedVersion, StageName: stage, AttemptID: attemptID,
		InputDigest: input, IdempotencyKey: key, At: at,
	})
	if err != nil {
		t.Fatalf("BeginStage: %v", err)
	}
	return attempt
}

func mustAccount(t *testing.T, store *Store, command domain.ActiveTimeCommand) domain.ActiveTimeResult {
	t.Helper()
	result, err := store.AccountActiveTime(context.Background(), command)
	if err != nil {
		t.Fatalf("AccountActiveTime(%s): %v", command.Action, err)
	}
	return result
}

func ptrCause(value domain.ExecutionCause) *domain.ExecutionCause { return &value }
