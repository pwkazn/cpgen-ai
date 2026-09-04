package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cpgen/internal/domain"
)

const historicalMigrationOneHash = "sha256:868896872f3ef7d686e50431eb35e5d6e22c0a058ce3c17ab323775cfe70050c"

//go:embed testdata/000001_workflow_core_691b611.sql
var historicalMigrationOneSQL []byte

// TestMigrationSimultaneousFirstOpenIsIdempotent catches reading migration
// history before writer serialization and applying DDL from a stale snapshot.
func TestMigrationSimultaneousFirstOpenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	preflight, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open empty WAL preflight: %v", err)
	}
	if _, err := preflight.ExecContext(ctx, "PRAGMA journal_mode = WAL"); err != nil {
		_ = preflight.Close()
		t.Fatalf("prepare empty WAL database: %v", err)
	}
	if err := preflight.Close(); err != nil {
		t.Fatalf("close empty WAL preflight: %v", err)
	}
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})
	hook := func() {
		arrived <- struct{}{}
		<-release
	}
	results := make(chan error, 2)
	stores := make(chan *Store, 2)
	var workers sync.WaitGroup
	workers.Add(2)
	for range 2 {
		go func() {
			defer workers.Done()
			store, err := Open(ctx, Config{
				Path: path, BusyTimeout: 5 * time.Second, MaxReaders: 2,
				migrationStartHook: hook,
			})
			if store != nil {
				stores <- store
			}
			results <- err
		}()
	}
	for range 2 {
		select {
		case <-arrived:
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatal("simultaneous open did not reach migration boundary")
		}
	}
	close(release)
	workers.Wait()
	close(results)
	close(stores)
	for err := range results {
		if err != nil {
			t.Fatalf("simultaneous Open: %v", err)
		}
	}
	for store := range stores {
		defer store.Close()
	}
	check, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1})
	if err != nil {
		t.Fatalf("reopen after simultaneous first open: %v", err)
	}
	defer check.Close()
	var count int
	if err := check.db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count migration history: %v", err)
	}
	if count != 2 {
		t.Fatalf("migration rows = %d, want 2", count)
	}
}

// TestMigrationOnePreservesHistoricalBytes catches editing an immutable,
// already-recorded migration instead of adding a forward migration.
func TestMigrationOnePreservesHistoricalBytes(t *testing.T) {
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migrations) < 2 {
		t.Fatalf("loaded %d migrations, want at least 2", len(migrations))
	}
	if got := string(domain.SumBytes(historicalMigrationOneSQL)); got != historicalMigrationOneHash {
		t.Fatalf("historical fixture hash = %q, want %q", got, historicalMigrationOneHash)
	}
	if migrations[0].name != "000001_workflow_core.sql" || migrations[0].hash != historicalMigrationOneHash {
		t.Fatalf("migration 1 = (%q, %q), want immutable historical name/hash", migrations[0].name, migrations[0].hash)
	}
}

// TestMigrationFreshOpenAppliesForwardWorkflowMigration catches fresh stores
// stopping after the historical schema instead of traversing the full chain.
func TestMigrationFreshOpenAppliesForwardWorkflowMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "workflow.db"), BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	assertMigrationHistory(t, store, 2)
	assertForwardWorkflowSchema(t, store)
}

// TestMigrationUpgradesHistoricalWorkflowDatabase catches drift rejection or
// destructive rebuilds when opening a database created by commit 691b611.
func TestMigrationUpgradesHistoricalWorkflowDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	fixture := seedHistoricalWorkflowDatabase(t, path)

	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("upgrade historical database: %v", err)
	}
	defer store.Close()
	assertMigrationHistory(t, store, 2)
	assertForwardWorkflowSchema(t, store)

	for table, want := range fixture.rowCounts {
		var got int
		if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&got); err != nil {
			t.Fatalf("count upgraded %s: %v", table, err)
		}
		if got != want {
			t.Fatalf("upgraded %s rows = %d, want %d", table, got, want)
		}
	}
	var workflowRevision, schemaVersion string
	var reviewWaivable int
	if err := store.db.QueryRowContext(ctx, `
		SELECT workflow_revision, schema_version, review_waivable
		FROM stage_records WHERE run_id = ? AND stage_name = 'prepare'`,
		string(fixture.reviewRunID),
	).Scan(&workflowRevision, &schemaVersion, &reviewWaivable); err != nil {
		t.Fatalf("read upgraded stage binding: %v", err)
	}
	if workflowRevision != string(fixture.workflowDigest) || schemaVersion != "cpgen.request/v1" || reviewWaivable != 0 {
		t.Fatalf("upgraded stage binding = (%q, %q, %d)", workflowRevision, schemaVersion, reviewWaivable)
	}
	var startedAt, finishedAt string
	if err := store.db.QueryRowContext(ctx, `
		SELECT started_at, finished_at FROM stage_attempts WHERE attempt_id = ?`,
		string(fixture.attemptID),
	).Scan(&startedAt, &finishedAt); err != nil {
		t.Fatalf("read upgraded attempt: %v", err)
	}
	if startedAt != "2026-09-01T08:00:01.000000000Z" || finishedAt != "2026-09-01T08:00:02.000000000Z" {
		t.Fatalf("normalized attempt times = (%q, %q)", startedAt, finishedAt)
	}
	var eventResult []byte
	if err := store.db.QueryRowContext(ctx, `
		SELECT result_json FROM run_events WHERE run_id = ? AND version = 4`,
		string(fixture.reviewRunID),
	).Scan(&eventResult); err != nil {
		t.Fatalf("read preserved event result: %v", err)
	}
	if string(eventResult) != `{"legacy":"review-created"}` {
		t.Fatalf("event result changed during upgrade: %s", eventResult)
	}
	var configJSON []byte
	var configDigest string
	if err := store.db.QueryRowContext(ctx, `
		SELECT redacted_effective_config_json, redacted_effective_config_digest
		FROM run_config_revisions WHERE run_id = ? AND revision = 1`,
		string(fixture.reviewRunID),
	).Scan(&configJSON, &configDigest); err != nil {
		t.Fatalf("read seeded config revision: %v", err)
	}
	if string(configJSON) != string(fixture.configJSON) || configDigest != string(domain.SumBytes(fixture.configJSON)) {
		t.Fatalf("seeded config binding = (%s, %q)", configJSON, configDigest)
	}

	if _, err := store.ApplyReview(ctx, domain.ApplyReviewCommand{
		RunID: fixture.reviewRunID, ExpectedRunVersion: 4, ReviewDecisionID: fixture.reviewID,
		StageName: "prepare", StageInputDigest: fixture.requestDigest,
		EvidenceDigest: fixture.evidenceDigest, PolicyDigest: fixture.policyDigest,
		IdempotencyKey: "apply_legacy_waive_00000000000000000000000000000001", At: testNow.Add(5 * time.Second),
	}); !errors.Is(err, ErrConsistency) {
		t.Fatalf("apply unverifiable legacy waiver = %v, want ErrConsistency", err)
	}
	cancelled, err := store.FinalizeCancel(ctx, domain.FinalizeCancelCommand{
		RunID: fixture.cancelRunID, ExpectedRunVersion: 2, ControlRequestID: fixture.controlID,
		ReconciliationDigest: domain.SumBytes([]byte("legacy cancel reconciled")),
		IdempotencyKey:       "finalize_legacy_cancel_00000000000000000000000000000001", At: testNow.Add(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("FinalizeCancel after upgrade: %v", err)
	}
	if cancelled.State != domain.RunCancelled || cancelled.SchemaVersion != "cpgen.request/v1" {
		t.Fatalf("finalized upgraded run = %+v", cancelled)
	}
	var eventType string
	if err := store.db.QueryRowContext(ctx, `
		SELECT event_type FROM run_events WHERE run_id = ? AND version = 3`,
		string(fixture.cancelRunID),
	).Scan(&eventType); err != nil {
		t.Fatalf("read finalized cancel event: %v", err)
	}
	if eventType != string(domain.EventCancelFinalized) {
		t.Fatalf("finalized cancel event = %q", eventType)
	}

	if err := store.Close(); err != nil {
		t.Fatalf("close upgraded store: %v", err)
	}
	reopened, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("idempotent reopen after upgrade: %v", err)
	}
	defer reopened.Close()
	assertMigrationHistory(t, reopened, 2)
}

// TestCreateRunReplaysLegacyCreateAfterHistoricalMigration catches rejecting
// the exact create command emitted by 691b611 after its database is upgraded.
func TestCreateRunReplaysLegacyCreateAfterHistoricalMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	fixture := seedHistoricalWorkflowDatabase(t, path)
	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("upgrade historical database: %v", err)
	}
	defer store.Close()

	var beforeDigest, beforeEventDigest string
	var beforeResult, beforeEventResult []byte
	if err := store.db.QueryRowContext(ctx, `
		SELECT create_command_digest, create_result_json FROM runs WHERE run_id = ?`,
		string(fixture.legacyCreateRequest.RunID),
	).Scan(&beforeDigest, &beforeResult); err != nil {
		t.Fatalf("read historical create binding: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `
		SELECT command_digest, result_json FROM run_events WHERE run_id = ? AND version = 1`,
		string(fixture.legacyCreateRequest.RunID),
	).Scan(&beforeEventDigest, &beforeEventResult); err != nil {
		t.Fatalf("read historical create event: %v", err)
	}
	mustBeginStage(t, store, fixture.legacyCreateRequest.RunID,
		"attempt_00000000000000000000000000000013", 1, "prepare", fixture.requestDigest,
		testNow.Add(time.Second), "begin_00000000000000000000000000000013")

	replayed, err := store.CreateRun(ctx, fixture.legacyCreateRequest)
	if err != nil {
		t.Fatalf("replay historical CreateRun: %v", err)
	}
	wantReplay := fixture.legacyCreateResult
	wantReplay.SchemaVersion = fixture.legacyCreateRequest.SchemaVersion
	if !reflect.DeepEqual(replayed, wantReplay) {
		t.Fatalf("legacy replay result = %+v, want current persisted projection %+v", replayed, wantReplay)
	}

	var afterDigest, afterEventDigest string
	var afterResult, afterEventResult []byte
	if err := store.db.QueryRowContext(ctx, `
		SELECT create_command_digest, create_result_json FROM runs WHERE run_id = ?`,
		string(fixture.legacyCreateRequest.RunID),
	).Scan(&afterDigest, &afterResult); err != nil {
		t.Fatalf("read replayed create binding: %v", err)
	}
	if err := store.db.QueryRowContext(ctx, `
		SELECT command_digest, result_json FROM run_events WHERE run_id = ? AND version = 1`,
		string(fixture.legacyCreateRequest.RunID),
	).Scan(&afterEventDigest, &afterEventResult); err != nil {
		t.Fatalf("read replayed create event: %v", err)
	}
	if afterDigest != beforeDigest || afterEventDigest != beforeEventDigest ||
		string(afterResult) != string(beforeResult) || string(afterEventResult) != string(beforeEventResult) {
		t.Fatal("legacy replay rewrote historical create or audit bindings")
	}
	stored, err := store.GetRun(ctx, fixture.legacyCreateRequest.RunID)
	if err != nil {
		t.Fatalf("read upgraded historical run: %v", err)
	}
	if stored.WorkflowRevision != fixture.legacyCreateRequest.WorkflowRevision || stored.SchemaVersion != fixture.legacyCreateRequest.SchemaVersion {
		t.Fatalf("upgraded immutable bindings = (%q, %q), want (%q, %q)", stored.WorkflowRevision, stored.SchemaVersion, fixture.legacyCreateRequest.WorkflowRevision, fixture.legacyCreateRequest.SchemaVersion)
	}
}

// TestCreateRunRejectsChangedLegacyReplay catches using an old digest to bypass
// the explicit workflow and schema bindings introduced after 691b611.
func TestCreateRunRejectsChangedLegacyReplay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	fixture := seedHistoricalWorkflowDatabase(t, path)
	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("upgrade historical database: %v", err)
	}
	defer store.Close()

	for name, mutate := range map[string]func(*domain.CreateRunRequest){
		"submitted request": func(request *domain.CreateRunRequest) {
			request.SubmittedRequestJSON = []byte(`{"brief":"changed","schema_version":"cpgen.request/v1"}`)
			request.SubmittedRequestDigest = domain.SumBytes(request.SubmittedRequestJSON)
		},
		"workflow revision": func(request *domain.CreateRunRequest) {
			request.WorkflowRevision = "slice1/changed"
		},
		"schema version": func(request *domain.CreateRunRequest) {
			request.SubmittedRequestJSON = []byte(`{"brief":"legacy","schema_version":"cpgen.request/v2"}`)
			request.SubmittedRequestDigest = domain.SumBytes(request.SubmittedRequestJSON)
			request.SchemaVersion = "cpgen.request/v2"
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := fixture.legacyCreateRequest
			mutate(&request)
			if _, err := store.CreateRun(ctx, request); !errors.Is(err, ErrConsistency) {
				t.Fatalf("changed legacy replay = %v, want ErrConsistency", err)
			}
		})
	}
}

// TestCreateRunWritesCurrentDigestAfterHistoricalMigration catches new creates
// accidentally using the old 691b611 command representation.
func TestCreateRunWritesCurrentDigestAfterHistoricalMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	_ = seedHistoricalWorkflowDatabase(t, path)
	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("upgrade historical database: %v", err)
	}
	defer store.Close()

	request := testCreateRunRequest("run_00000000000000000000000000000014", testNow, 30*time.Second)
	request.IdempotencyKey = "create_00000000000000000000000000000014"
	currentDigest, _, err := digestJSON(request)
	if err != nil {
		t.Fatalf("digest current CreateRun: %v", err)
	}
	if _, err := store.CreateRun(ctx, request); err != nil {
		t.Fatalf("fresh CreateRun after upgrade: %v", err)
	}
	var storedDigest string
	if err := store.db.QueryRowContext(ctx, "SELECT create_command_digest FROM runs WHERE run_id = ?", string(request.RunID)).Scan(&storedDigest); err != nil {
		t.Fatalf("read fresh create digest: %v", err)
	}
	if storedDigest != string(currentDigest) {
		t.Fatalf("fresh create digest = %q, want current %q", storedDigest, currentDigest)
	}
	if storedDigest == string(historicalCreateRunDigestV691b611(t, request)) {
		t.Fatal("fresh CreateRun stored the legacy digest")
	}
}

// TestMigrationRecordsVersionNameAndHashAndReopens catches migrations that
// cannot prove which immutable SQL bytes produced a database.
func TestMigrationRecordsVersionNameAndHashAndReopens(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	assertMigrationHistory(t, store, 2)
	if err := store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}
	reopened, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("idempotent reopen: %v", err)
	}
	defer reopened.Close()
	var count int
	if err := reopened.db.QueryRowContext(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if count != 2 {
		t.Fatalf("migration count = %d, want 2", count)
	}
}

// TestMigrationRejectsChangedHash catches silently accepting a database whose
// applied migration bytes no longer match the compiled migration.
func TestMigrationRejectsChangedHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.db.ExecContext(ctx, "UPDATE schema_migrations SET sha256 = ? WHERE version = 1", "sha256:"+strings.Repeat("0", 64)); err != nil {
		t.Fatalf("corrupt migration hash: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}); !errors.Is(err, ErrMigrationDrift) {
		t.Fatalf("reopen changed hash = %v, want ErrMigrationDrift", err)
	}
}

// TestMigrationRejectsMissingVersion catches accepting a non-contiguous or
// future schema without the exact compiled predecessor chain.
func TestMigrationRejectsMissingVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, "DELETE FROM schema_migrations WHERE version = 1"); err != nil {
		t.Fatalf("create migration gap: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}); !errors.Is(err, ErrMigrationGap) {
		t.Fatalf("reopen missing version = %v, want ErrMigrationGap", err)
	}
}

// TestMigrationLeavesHealthyDatabase catches DDL that disables referential
// checks or leaves structural corruption after a normal migration.
func TestMigrationLeavesHealthyDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "workflow.db"), BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()
	rows, err := store.db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign_key_check returned a violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("foreign_key_check rows: %v", err)
	}
	var integrity string
	if err := store.db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		t.Fatalf("integrity_check: %v", err)
	}
	if integrity != "ok" {
		t.Fatalf("integrity_check = %q, want ok", integrity)
	}
}

func migrationOneHash(t *testing.T) string {
	t.Helper()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migrations) < 1 {
		t.Fatal("loaded no migrations")
	}
	return migrations[0].hash
}

func assertMigrationHistory(t *testing.T, store *Store, want int) {
	t.Helper()
	rows, err := store.db.Query(`SELECT version, name, sha256 FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read migration history: %v", err)
	}
	defer rows.Close()
	type record struct {
		version int
		name    string
		hash    string
	}
	var got []record
	for rows.Next() {
		var item record
		if err := rows.Scan(&item.version, &item.name, &item.hash); err != nil {
			t.Fatalf("scan migration history: %v", err)
		}
		got = append(got, item)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate migration history: %v", err)
	}
	if len(got) != want {
		t.Fatalf("migration history length = %d, want %d: %+v", len(got), want, got)
	}
	for index, item := range got {
		if item.version != index+1 {
			t.Fatalf("migration[%d] version = %d", index, item.version)
		}
	}
	if got[0].name != "000001_workflow_core.sql" || got[0].hash != historicalMigrationOneHash {
		t.Fatalf("migration 1 = %+v, want historical identity", got[0])
	}
	if want >= 2 && got[1].name != "000002_workflow_invariants.sql" {
		t.Fatalf("migration 2 name = %q", got[1].name)
	}
}

func assertForwardWorkflowSchema(t *testing.T, store *Store) {
	t.Helper()
	rows, err := store.db.Query(`PRAGMA table_info(stage_records)`)
	if err != nil {
		t.Fatalf("stage_records columns: %v", err)
	}
	defer rows.Close()
	wantColumns := map[string]bool{
		"workflow_revision": false,
		"schema_version":    false,
		"review_waivable":   false,
	}
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatalf("scan stage_records column: %v", err)
		}
		if _, ok := wantColumns[name]; ok {
			wantColumns[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stage_records columns: %v", err)
	}
	for name, found := range wantColumns {
		if !found {
			t.Errorf("stage_records missing %s", name)
		}
	}
	var exists int
	if err := store.db.QueryRow(`
		SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'run_config_revisions'`,
	).Scan(&exists); err != nil {
		t.Fatalf("inspect run_config_revisions: %v", err)
	}
	if exists != 1 {
		t.Errorf("run_config_revisions table count = %d", exists)
	}
}

type historicalWorkflowFixture struct {
	reviewRunID         domain.RunID
	cancelRunID         domain.RunID
	attemptID           domain.AttemptID
	reviewID            domain.ReviewDecisionID
	controlID           domain.ControlRequestID
	requestDigest       domain.Digest
	configJSON          []byte
	workflowDigest      domain.Digest
	evidenceDigest      domain.Digest
	policyDigest        domain.Digest
	legacyCreateRequest domain.CreateRunRequest
	legacyCreateResult  domain.RunSnapshot
	legacyCreateDigest  domain.Digest
	rowCounts           map[string]int
}

func seedHistoricalWorkflowDatabase(t *testing.T, path string) historicalWorkflowFixture {
	t.Helper()
	ctx := context.Background()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open historical fixture: %v", err)
	}
	db.SetMaxOpenConns(1)
	defer db.Close()
	if _, err := db.ExecContext(ctx, string(historicalMigrationOneSQL)); err != nil {
		t.Fatalf("apply historical migration 1: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO schema_migrations(version, name, sha256, applied_at)
		VALUES (1, '000001_workflow_core.sql', ?, '2026-09-01T08:00:00Z')`,
		historicalMigrationOneHash,
	); err != nil {
		t.Fatalf("record historical migration 1: %v", err)
	}

	fixture := historicalWorkflowFixture{
		reviewRunID:    "run_00000000000000000000000000000011",
		cancelRunID:    "run_00000000000000000000000000000012",
		attemptID:      "attempt_00000000000000000000000000000011",
		reviewID:       "review_00000000000000000000000000000011",
		controlID:      "control_00000000000000000000000000000012",
		configJSON:     []byte(`{"schema":"cpgen.config/v1"}`),
		workflowDigest: domain.SumBytes([]byte("legacy-workflow")),
		evidenceDigest: domain.SumBytes([]byte("legacy-evidence")),
		policyDigest:   domain.SumBytes([]byte("legacy-policy")),
	}
	requestJSON := []byte(`{"brief":"legacy","schema_version":"cpgen.request/v1"}`)
	fixture.requestDigest = domain.SumBytes(requestJSON)
	configDigest := domain.SumBytes(fixture.configJSON)
	fixture.legacyCreateRequest = domain.CreateRunRequest{
		RunID:                         "run_00000000000000000000000000000013",
		SubmittedRequestJSON:          requestJSON,
		SubmittedRequestDigest:        fixture.requestDigest,
		EffectiveSeed:                 7,
		RedactedEffectiveConfigJSON:   fixture.configJSON,
		RedactedEffectiveConfigDigest: configDigest,
		WorkflowRevision:              string(fixture.workflowDigest),
		SchemaVersion:                 "cpgen.request/v1",
		WorkflowDigest:                fixture.workflowDigest,
		BudgetLimits: domain.BudgetLimits{
			MaxLLMCalls: 3, MaxSimilarityCalls: 2, MaxLLMInputTokens: 1000, MaxLLMOutputTokens: 1000,
			MaxLLMCostMicroUSD: 5000, MaxSandboxCreates: 4, MaxArtifactBytes: 1048576, MaxPackageBytes: 1048576,
			MaxMutationsPerStage: 2, MaxActiveTimeMilliseconds: 30000,
		},
		StageSequence:  []domain.StageName{"prepare", "exercise"},
		CreatedAt:      testNow,
		IdempotencyKey: "create_00000000000000000000000000000013",
	}
	fixture.legacyCreateDigest = historicalCreateRunDigestV691b611(t, fixture.legacyCreateRequest)
	fixture.legacyCreateResult = domain.RunSnapshot{
		RunID: fixture.legacyCreateRequest.RunID, State: domain.RunCreated, Version: 1,
		WorkflowRevision: string(fixture.workflowDigest), SchemaVersion: "",
		RequestDigest: fixture.requestDigest, ConfigDigest: configDigest, WorkflowDigest: fixture.workflowDigest,
		CurrentStage: "prepare", CurrentStageOrdinal: 1, CreatedAt: testNow, UpdatedAt: testNow,
	}
	legacyResultJSON, err := json.Marshal(fixture.legacyCreateResult)
	if err != nil {
		t.Fatalf("marshal historical CreateRun result: %v", err)
	}
	for _, run := range []struct {
		id      domain.RunID
		state   domain.RunState
		version int
		key     string
		result  string
	}{
		{fixture.reviewRunID, domain.RunNeedsReview, 4, "create_legacy_review_00000000000000001", `{"legacy":"review-run"}`},
		{fixture.cancelRunID, domain.RunCreated, 2, "create_legacy_cancel_00000000000000001", `{"legacy":"cancel-run"}`},
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO runs(
				run_id, submitted_request_json, submitted_request_digest, effective_seed,
				redacted_effective_config_json, redacted_effective_config_digest,
				workflow_digest, workflow_revision, schema_version,
				max_llm_calls, max_similarity_calls, max_llm_input_tokens, max_llm_output_tokens,
				max_llm_cost_micro_usd, max_sandbox_creates, max_artifact_bytes, max_package_bytes,
				max_mutations_per_stage, max_active_time_ns,
				state, current_stage, current_stage_ordinal, version,
				create_idempotency_key, create_command_digest, create_result_json, created_at, updated_at
			) VALUES (?, ?, ?, 7, ?, ?, ?, ?, '', 3, 2, 1000, 1000, 5000, 4, 1048576, 1048576, 2, 30000000000,
				?, 'prepare', 1, ?, ?, ?, ?, '2026-09-01T08:00:00Z', '2026-09-01T08:00:03Z')`,
			string(run.id), requestJSON, string(fixture.requestDigest), fixture.configJSON, string(configDigest),
			string(fixture.workflowDigest), string(fixture.workflowDigest), string(run.state), run.version,
			run.key, string(domain.SumBytes([]byte(run.key))), []byte(run.result),
		); err != nil {
			t.Fatalf("insert historical run %s: %v", run.id, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO runs(
			run_id, submitted_request_json, submitted_request_digest, effective_seed,
			redacted_effective_config_json, redacted_effective_config_digest,
			workflow_digest, workflow_revision, schema_version,
			max_llm_calls, max_similarity_calls, max_llm_input_tokens, max_llm_output_tokens,
			max_llm_cost_micro_usd, max_sandbox_creates, max_artifact_bytes, max_package_bytes,
			max_mutations_per_stage, max_active_time_ns,
			state, current_stage, current_stage_ordinal, version,
			create_idempotency_key, create_command_digest, create_result_json, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
			'CREATED', 'prepare', 1, 1, ?, ?, ?, '2026-09-01T08:00:00Z', '2026-09-01T08:00:00Z')`,
		string(fixture.legacyCreateRequest.RunID), fixture.legacyCreateRequest.SubmittedRequestJSON, string(fixture.requestDigest), fixture.legacyCreateRequest.EffectiveSeed,
		fixture.legacyCreateRequest.RedactedEffectiveConfigJSON, string(configDigest), string(fixture.workflowDigest), string(fixture.workflowDigest),
		fixture.legacyCreateRequest.BudgetLimits.MaxLLMCalls, fixture.legacyCreateRequest.BudgetLimits.MaxSimilarityCalls,
		fixture.legacyCreateRequest.BudgetLimits.MaxLLMInputTokens, fixture.legacyCreateRequest.BudgetLimits.MaxLLMOutputTokens,
		fixture.legacyCreateRequest.BudgetLimits.MaxLLMCostMicroUSD, fixture.legacyCreateRequest.BudgetLimits.MaxSandboxCreates,
		fixture.legacyCreateRequest.BudgetLimits.MaxArtifactBytes, fixture.legacyCreateRequest.BudgetLimits.MaxPackageBytes,
		fixture.legacyCreateRequest.BudgetLimits.MaxMutationsPerStage, fixture.legacyCreateRequest.BudgetLimits.MaxActiveTimeMilliseconds*int64(time.Millisecond),
		fixture.legacyCreateRequest.IdempotencyKey, string(fixture.legacyCreateDigest), legacyResultJSON,
	); err != nil {
		t.Fatalf("insert historical replay run: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO stage_records(
			run_id, stage_name, ordinal, state, version, input_digest, attempt_count,
			current_attempt_id, logical_idempotency_key, review_evidence_digest, review_policy_digest,
			created_at, updated_at
		) VALUES
			(?, 'prepare', 1, 'NEEDS_REVIEW', 3, ?, 1, NULL, 'legacy:prepare', ?, ?, '2026-09-01T08:00:00Z', '2026-09-01T08:00:02Z'),
			(?, 'exercise', 2, 'PENDING', 1, ?, 0, NULL, 'legacy:exercise', NULL, NULL, '2026-09-01T08:00:00Z', '2026-09-01T08:00:00Z'),
			(?, 'prepare', 1, 'PENDING', 1, ?, 0, NULL, 'legacy-cancel:prepare', NULL, NULL, '2026-09-01T08:00:00Z', '2026-09-01T08:00:00Z'),
			(?, 'prepare', 1, 'PENDING', 1, ?, 0, NULL, ?, NULL, NULL, '2026-09-01T08:00:00Z', '2026-09-01T08:00:00Z'),
			(?, 'exercise', 2, 'PENDING', 1, ?, 0, NULL, ?, NULL, NULL, '2026-09-01T08:00:00Z', '2026-09-01T08:00:00Z')`,
		string(fixture.reviewRunID), string(fixture.requestDigest), string(fixture.evidenceDigest), string(fixture.policyDigest),
		string(fixture.reviewRunID), string(fixture.requestDigest),
		string(fixture.cancelRunID), string(fixture.requestDigest),
		string(fixture.legacyCreateRequest.RunID), string(fixture.requestDigest), fixture.legacyCreateRequest.IdempotencyKey+":prepare",
		string(fixture.legacyCreateRequest.RunID), string(fixture.requestDigest), fixture.legacyCreateRequest.IdempotencyKey+":exercise",
	); err != nil {
		t.Fatalf("insert historical stages: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO stage_attempts(
			attempt_id, run_id, stage_name, ordinal, state, input_digest, started_at, finished_at
		) VALUES (?, ?, 'prepare', 1, 'NEEDS_REVIEW', ?, '2026-09-01T08:00:01Z', '2026-09-01T08:00:02Z')`,
		string(fixture.attemptID), string(fixture.reviewRunID), string(fixture.requestDigest),
	); err != nil {
		t.Fatalf("insert historical attempt: %v", err)
	}
	for _, event := range []struct {
		runID    domain.RunID
		version  int
		typeName string
		key      string
		result   string
	}{
		{fixture.reviewRunID, 1, "RUN_CREATED", "event_legacy_review_created_000000001", `{"legacy":"created"}`},
		{fixture.reviewRunID, 2, "STAGE_BEGAN", "event_legacy_review_began_0000000001", `{"legacy":"began"}`},
		{fixture.reviewRunID, 3, "STAGE_FINISHED", "event_legacy_review_finished_0000001", `{"legacy":"needs-review"}`},
		{fixture.reviewRunID, 4, "REVIEW_CREATED", "event_legacy_review_requested_000001", `{"legacy":"review-created"}`},
		{fixture.cancelRunID, 1, "RUN_CREATED", "event_legacy_cancel_created_000000001", `{"legacy":"created"}`},
		{fixture.cancelRunID, 2, "CANCEL_REQUESTED", "event_legacy_cancel_requested_000001", `{"legacy":"cancel-requested"}`},
	} {
		if _, err := db.ExecContext(ctx, `
			INSERT INTO run_events(
				run_id, version, event_type, stage_name, idempotency_key,
				command_digest, result_json, occurred_at
			) VALUES (?, ?, ?, 'prepare', ?, ?, ?, '2026-09-01T08:00:03Z')`,
			string(event.runID), event.version, event.typeName, event.key,
			string(domain.SumBytes([]byte(event.key))), []byte(event.result),
		); err != nil {
			t.Fatalf("insert historical event %s/%d: %v", event.runID, event.version, err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO run_events(run_id, version, event_type, stage_name, idempotency_key, command_digest, result_json, occurred_at)
		VALUES (?, 1, 'RUN_CREATED', NULL, ?, ?, ?, '2026-09-01T08:00:00Z')`,
		string(fixture.legacyCreateRequest.RunID), fixture.legacyCreateRequest.IdempotencyKey,
		string(fixture.legacyCreateDigest), legacyResultJSON,
	); err != nil {
		t.Fatalf("insert historical CreateRun event: %v", err)
	}
	waiverDigest := domain.SumBytes([]byte("legacy-waiver"))
	if _, err := db.ExecContext(ctx, `
		INSERT INTO review_decisions(
			review_id, run_id, kind, state, expected_run_version, run_version,
			workflow_revision, stage_name, stage_input_digest, evidence_digest, policy_digest,
			waiver_scope_digest, waivable_gate, reviewer, reason,
			idempotency_key, command_digest, created_at
		) VALUES (?, ?, 'WAIVE', 'PENDING', 3, 4, ?, 'prepare', ?, ?, ?, ?, 1,
			'legacy-reviewer', 'legacy waiver', 'review_legacy_waive_0000000000000001', ?, '2026-09-01T08:00:03Z')`,
		string(fixture.reviewID), string(fixture.reviewRunID), string(fixture.workflowDigest),
		string(fixture.requestDigest), string(fixture.evidenceDigest), string(fixture.policyDigest), string(waiverDigest),
		string(domain.SumBytes([]byte("legacy review command"))),
	); err != nil {
		t.Fatalf("insert historical review: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO control_requests(
			control_id, run_id, kind, state, reason, expected_run_version,
			idempotency_key, command_digest, created_at
		) VALUES (?, ?, 'CANCEL', 'PENDING', 'legacy cancel', 1,
			'cancel_legacy_0000000000000000000001', ?, '2026-09-01T08:00:03Z')`,
		string(fixture.controlID), string(fixture.cancelRunID), string(domain.SumBytes([]byte("legacy cancel command"))),
	); err != nil {
		t.Fatalf("insert historical cancel: %v", err)
	}
	fixture.rowCounts = map[string]int{
		"runs": 3, "stage_records": 5, "stage_attempts": 1,
		"run_events": 7, "control_requests": 1, "review_decisions": 1,
	}
	return fixture
}

// historicalCreateRunDigestV691b611 reproduces the persisted create-command
// digest before workflow_revision and schema_version became explicit fields.
func historicalCreateRunDigestV691b611(t *testing.T, request domain.CreateRunRequest) domain.Digest {
	t.Helper()
	type historicalCreateRunRequestV691b611 struct {
		RunID                         domain.RunID        `json:"run_id"`
		SubmittedRequestJSON          []byte              `json:"submitted_request_json"`
		SubmittedRequestDigest        domain.Digest       `json:"submitted_request_digest"`
		EffectiveSeed                 int64               `json:"effective_seed"`
		RedactedEffectiveConfigJSON   []byte              `json:"redacted_effective_config_json"`
		RedactedEffectiveConfigDigest domain.Digest       `json:"redacted_effective_config_digest"`
		WorkflowDigest                domain.Digest       `json:"workflow_digest"`
		BudgetLimits                  domain.BudgetLimits `json:"budget_limits"`
		StageSequence                 []domain.StageName  `json:"stage_sequence"`
		CreatedAt                     time.Time           `json:"created_at"`
		IdempotencyKey                string              `json:"idempotency_key"`
	}
	encoded, err := json.Marshal(historicalCreateRunRequestV691b611{
		RunID: request.RunID, SubmittedRequestJSON: request.SubmittedRequestJSON, SubmittedRequestDigest: request.SubmittedRequestDigest,
		EffectiveSeed: request.EffectiveSeed, RedactedEffectiveConfigJSON: request.RedactedEffectiveConfigJSON,
		RedactedEffectiveConfigDigest: request.RedactedEffectiveConfigDigest, WorkflowDigest: request.WorkflowDigest,
		BudgetLimits: request.BudgetLimits, StageSequence: request.StageSequence, CreatedAt: request.CreatedAt,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		t.Fatalf("marshal 691b611 CreateRun command: %v", err)
	}
	return domain.SumBytes(encoded)
}
