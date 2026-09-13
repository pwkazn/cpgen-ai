package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func TestCacheSourceMigrationPreservesHistoryAndAllowsEarlierAttempt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m20.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:20] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,sha256,applied_at) VALUES(?,?,?,?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{db: db, config: Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}, clock: clock.NewFake(testNow)}
	runID := domain.RunID("run_000000000000000000000000000000e1")
	attemptID := domain.AttemptID("attempt_000000000000000000000000000000e1")
	mustCreateRun(t, store, testCreateRunRequest(runID, testNow, time.Minute))
	input := domain.SumBytes([]byte("cache migration"))
	mustBeginStage(t, store, runID, attemptID, 1, "prepare", input, testNow, meteringID("begin", "m20 source"))
	f := meteringFixture{store: store, runID: runID, attemptID: attemptID, stage: "prepare", now: testNow}
	source := mustOpenMeteringCall(t, f, 1, domain.CallLLMGenerate)
	prepared, err := store.PrepareCalls(ctx, prepareOneRequest(f, source, 1, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1))
	if err != nil {
		t.Fatal(err)
	}
	physical := prepared.PhysicalCalls[0]
	grant := mustBeginDispatch(t, f, source.ID, physical.ID, "m20 source")
	if err := store.MarkSent(ctx, grant, testNow); err != nil {
		t.Fatal(err)
	}
	response := domain.SumBytes([]byte("migration response"))
	if err := store.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: attemptID, CallRecordID: source.ID, AttemptCallID: physical.ID, State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ProviderRequestID: "fixture", ResponseDigest: &response, IdempotencyKey: meteringID("complete", "m20 source"), At: testNow}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishCall(ctx, domain.FinishCallRequest{RunID: runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: attemptID, CallRecordID: source.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &physical.ID, IdempotencyKey: meteringID("finish", "m20 source"), At: testNow}); err != nil {
		t.Fatal(err)
	}
	oldHit := mustOpenMeteringCall(t, f, 2, domain.CallCacheReuse)
	oldTrace, err := store.FinishCall(ctx, domain.FinishCallRequest{RunID: runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: attemptID, CallRecordID: oldHit.ID, DispatchKind: domain.DispatchCacheHit, CacheSourceCallRecordID: &source.ID, CacheHitCallRecordID: &oldHit.ID, IdempotencyKey: meteringID("finish", "m20 old hit"), At: testNow})
	if err != nil {
		t.Fatal(err)
	}
	saved, err := store.LoadCall(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishStage(ctx, domain.FinishStageCommand{RunID: runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &input, NextStage: "exercise", NextInputDigest: &input, IdempotencyKey: meteringID("finish", "m20 stage"), At: testNow}); err != nil {
		t.Fatal(err)
	}
	currentAttempt := domain.AttemptID("attempt_000000000000000000000000000000e2")
	mustBeginStage(t, store, runID, currentAttempt, 3, "exercise", input, testNow, meteringID("begin", "m20 current"))
	open := openCallRequest(f, 3, domain.CallCacheReuse)
	open.StageName, open.AttemptID, open.ExpectedRunVersion = "exercise", currentAttempt, 4
	current, err := store.OpenCall(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	finish := domain.FinishCallRequest{RunID: runID, ExpectedRunVersion: 4, StageName: "exercise", AttemptID: currentAttempt, CallRecordID: current.ID, DispatchKind: domain.DispatchCacheHit, CacheSourceCallRecordID: &source.ID, CacheHitCallRecordID: &current.ID, IdempotencyKey: meteringID("finish", "m20 current"), At: testNow}
	if _, err := store.FinishCall(ctx, finish); err == nil {
		t.Fatal("M20 did not reproduce the same-attempt source restriction")
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("populated M20 upgrade: %v", err)
	}
	restored, err := store.LoadCall(ctx, source.ID)
	if err != nil || !reflect.DeepEqual(saved, restored) {
		t.Fatalf("provider call/reservations changed: %v", err)
	}
	old, err := store.LoadCall(ctx, oldHit.ID)
	if err != nil || old.CallTrace == nil || !old.CallTrace.Equal(oldTrace) {
		t.Fatalf("old cache hit changed: %v", err)
	}
	if _, err := store.FinishCall(ctx, finish); err != nil {
		t.Fatalf("same-run earlier attempt remains blocked: %v", err)
	}
	otherRun := domain.RunID("run_000000000000000000000000000000e3")
	otherAttempt := domain.AttemptID("attempt_000000000000000000000000000000e3")
	otherRequest := testCreateRunRequest(otherRun, testNow, time.Minute)
	otherRequest.IdempotencyKey = meteringID("create", "m21 other")
	mustCreateRun(t, store, otherRequest)
	mustBeginStage(t, store, otherRun, otherAttempt, 1, "prepare", input, testNow, meteringID("begin", "m21 other"))
	otherFixture := meteringFixture{store: store, runID: otherRun, attemptID: otherAttempt, stage: "prepare", now: testNow}
	other := mustOpenMeteringCall(t, otherFixture, 90, domain.CallLLMGenerate)
	if _, err := store.FinishCall(ctx, domain.FinishCallRequest{RunID: otherRun, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: otherAttempt, CallRecordID: other.ID, DispatchKind: domain.DispatchNone, Failure: &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}, IdempotencyKey: meteringID("finish", "m21 other"), At: testNow}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE call_records SET cache_source_call_record_id=? WHERE call_record_id=?`, other.ID, current.ID); err == nil {
		t.Fatal("migration admitted a cross-run cache source")
	}
	var violations, foreignKeys, legacy, triggers int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `PRAGMA legacy_alter_table`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='trigger' AND tbl_name='call_records'`).Scan(&triggers); err != nil {
		t.Fatal(err)
	}
	if violations != 0 || foreignKeys != 1 || legacy != 0 || triggers != 2 {
		t.Fatalf("upgrade state: violations=%d foreignKeys=%d legacy=%d triggers=%d", violations, foreignKeys, legacy, triggers)
	}
}
