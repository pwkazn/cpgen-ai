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

func TestDockerVolumeCallHasDurableBoundaryWithoutContainerCharge(t *testing.T) {
	ctx := context.Background()
	f := newMeteringFixture(t, "f1", testCreateRunRequest(testRunID, testNow, time.Minute).BudgetLimits)
	before := readBudgetAccounts(t, f.store, f.runID)
	record := mustOpenMeteringCall(t, f, 1, domain.CallSandboxCompile)
	request := prepareOneRequest(f, record, 1, domain.PhysicalDockerVolumeCreate, "", 0)
	request.Calls[0].Provider = "docker"
	prepared, err := f.store.PrepareCalls(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if len(prepared.Reservations) != 0 || len(prepared.PhysicalCalls) != 1 {
		t.Fatalf("volume plan: %+v", prepared)
	}
	physical := prepared.PhysicalCalls[0]
	grant := mustBeginDispatch(t, f, record.ID, physical.ID, "volume")
	if grant.Kind != domain.PhysicalDockerVolumeCreate {
		t.Fatalf("wrong kind: %s", grant.Kind)
	}
	if err := f.store.MarkSent(ctx, grant, f.now); err != nil {
		t.Fatal(err)
	}
	digest := domain.SumBytes([]byte("exact-created-volume"))
	if err := f.store.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID, CallRecordID: record.ID, AttemptCallID: physical.ID, State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ProviderRequestID: "exact-created-volume", ResponseDigest: &digest, IdempotencyKey: meteringID("complete", "volume"), At: f.now}); err != nil {
		t.Fatal(err)
	}
	trace, err := f.store.FinishCall(ctx, domain.FinishCallRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID, CallRecordID: record.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &physical.ID, IdempotencyKey: meteringID("finish", "volume"), At: f.now})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(trace.PhysicalAttemptCallIDs, []domain.AttemptCallID{physical.ID}) {
		t.Fatalf("missing volume receipt: %+v", trace)
	}
	if got := readBudgetAccounts(t, f.store, f.runID); !reflect.DeepEqual(got, before) {
		t.Fatalf("volume changed account limits: %+v", got)
	}
	var reserved, consumed int64
	if err := f.store.db.QueryRowContext(ctx, `SELECT reserved_value, consumed_value FROM budget_accounts WHERE run_id=? AND dimension='DOCKER_CONTAINER_CREATES'`, f.runID).Scan(&reserved, &consumed); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 || consumed != 0 {
		t.Fatalf("volume charged as container: %d/%d", reserved, consumed)
	}
	foreign := mustOpenMeteringCall(t, f, 2, domain.CallLLMGenerate)
	bad := prepareOneRequest(f, foreign, 2, domain.PhysicalDockerVolumeCreate, "", 0)
	if _, err := f.store.PrepareCalls(ctx, bad); err == nil {
		t.Fatal("LLM call authorized Docker volume creation")
	}
	bad = prepareOneRequest(f, record, 3, domain.PhysicalDockerVolumeCreate, domain.BudgetDockerContainerCreates, 1)
	bad.Calls[0].Reservations = []domain.ReservationPlan{{ID: "res_000000000000000000000000000000f1", Dimension: domain.BudgetDockerContainerCreates, Subkey: "create", UpperBound: 1}}
	if err := bad.Validate(); err == nil {
		t.Fatal("volume accepted container-count reservation")
	}
}

func TestDockerVolumeMigrationPreservesPreparedAndTerminalReceipts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m24.db")
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
	for _, m := range migrations[:24] {
		if _, err := db.ExecContext(ctx, m.sql); err != nil {
			t.Fatalf("migration %d: %v", m.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,sha256,applied_at) VALUES(?,?,?,?)`, m.version, m.name, m.hash, formatTime(testNow)); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{db: db, config: Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}, clock: clock.NewFake(testNow)}
	runID := domain.RunID("run_000000000000000000000000000000f2")
	attemptID := domain.AttemptID("attempt_000000000000000000000000000000f2")
	mustCreateRun(t, store, testCreateRunRequest(runID, testNow, time.Minute))
	mustBeginStage(t, store, runID, attemptID, 1, "prepare", domain.SumBytes([]byte("m24")), testNow, meteringID("begin", "volume upgrade"))
	f := meteringFixture{store: store, runID: runID, attemptID: attemptID, stage: "prepare", now: testNow}
	saved := make(map[domain.CallRecordID]domain.PreparedCalls)
	for i := 1; i <= 2; i++ {
		record := mustOpenMeteringCall(t, f, i, domain.CallSandboxCompile)
		p, err := store.PrepareCalls(ctx, prepareOneRequest(f, record, i, domain.PhysicalDockerContainerCreate, domain.BudgetDockerContainerCreates, 1))
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			physical := p.PhysicalCalls[0]
			grant := mustBeginDispatch(t, f, record.ID, physical.ID, "m24 completed")
			if err := store.MarkSent(ctx, grant, testNow); err != nil {
				t.Fatal(err)
			}
			digest := domain.SumBytes([]byte("m24 container"))
			if err := store.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: attemptID, CallRecordID: record.ID, AttemptCallID: physical.ID, State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ProviderRequestID: "m24-container", ResponseDigest: &digest, IdempotencyKey: meteringID("complete", "m24"), At: testNow}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.FinishCall(ctx, domain.FinishCallRequest{RunID: runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: attemptID, CallRecordID: record.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &physical.ID, IdempotencyKey: meteringID("finish", "m24"), At: testNow}); err != nil {
				t.Fatal(err)
			}
		}
		saved[record.ID], err = store.LoadCall(ctx, record.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range saved {
		got, err := store.LoadCall(ctx, id)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("receipt %s changed: got=%+v want=%+v err=%v", id, got, want, err)
		}
	}
	var violations, foreignKeys, legacy, triggers int
	for query, output := range map[string]*int{`SELECT count(*) FROM pragma_foreign_key_check`: &violations, `PRAGMA foreign_keys`: &foreignKeys, `PRAGMA legacy_alter_table`: &legacy, `SELECT count(*) FROM sqlite_master WHERE type='trigger' AND tbl_name='physical_calls'`: &triggers} {
		if err := db.QueryRowContext(ctx, query).Scan(output); err != nil {
			t.Fatal(err)
		}
	}
	if violations != 0 || foreignKeys != 1 || legacy != 0 || triggers != 4 {
		t.Fatalf("upgrade state: fk violations=%d enabled=%d legacy=%d triggers=%d", violations, foreignKeys, legacy, triggers)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("repeat migration: %v", err)
	}
}
