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

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

const historicalMigrationOneHash = "sha256:868896872f3ef7d686e50431eb35e5d6e22c0a058ce3c17ab323775cfe70050c"

const historicalMigrationFourHash = "sha256:4a97274482009bda039717a935b1e7d38909875873c05500d22fa97a8c9fd188"

const (
	historicalMigrationTwoHash   = "sha256:fe1e3bbf31aa14df8f69e5be161266a2b7f8fc6ae05217f0b15424f78eebd094"
	historicalMigrationThreeHash = "sha256:e2abddad5db8e12611c2373f4e2d821cd1c255faab77df5984be1500c4e017be"
)

//go:embed testdata/000001_workflow_core_691b611.sql
var historicalMigrationOneSQL []byte

// TestMigrationSimultaneousFirstOpenIsIdempotent catches reading migration
// history before writer serialization and applying DDL from a stale snapshot.
func TestMigrationSimultaneousFirstOpenIsIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	var releaseOnce sync.Once
	releaseWorkers := func() { releaseOnce.Do(func() { close(release) }) }
	hook := func() {
		arrived <- struct{}{}
		<-release
	}
	results := make(chan error, 2)
	stores := make(chan *Store, 2)
	var workers sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		releaseWorkers()
		workers.Wait()
		close(stores)
		for store := range stores {
			if err := store.Close(); err != nil {
				t.Errorf("close simultaneous store: %v", err)
			}
		}
	})
	workers.Add(2)
	for range 2 {
		go func() {
			defer workers.Done()
			store, err := Open(ctx, Config{
				// This verifies migration serialization, not a five-second
				// execution deadline. Instrumented table rebuilds can exceed
				// that deadline while the other opener holds the writer lock.
				Path: path, BusyTimeout: 30 * time.Second, MaxReaders: 2,
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
			t.Fatal("simultaneous open did not reach migration boundary")
		}
	}
	releaseWorkers()
	workers.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatalf("simultaneous Open: %v", err)
		}
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
	if count != 32 {
		t.Fatalf("migration rows = %d, want 32", count)
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

// TestMigrationPreservesAppliedCallBudgetBytesAndUpgradesTerminalGuards
// catches editing the already-applied M4 instead of adding an ordered forward
// migration for the terminal projection guards.
func TestMigrationPreservesAppliedCallBudgetBytesAndUpgradesTerminalGuards(t *testing.T) {
	t.Parallel()
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	want := []struct {
		name string
		hash string
	}{
		{"000001_workflow_core.sql", historicalMigrationOneHash},
		{"000002_workflow_invariants.sql", historicalMigrationTwoHash},
		{"000003_call_budget.sql", historicalMigrationThreeHash},
		{"000004_call_budget_hardening.sql", historicalMigrationFourHash},
	}
	if len(migrations) < len(want)+1 {
		t.Fatalf("loaded %d migrations, want at least %d", len(migrations), len(want)+1)
	}
	for index, expected := range want {
		if migrations[index].name != expected.name || migrations[index].hash != expected.hash {
			t.Fatalf("migration %d = (%q, %q), want immutable (%q, %q)", index+1,
				migrations[index].name, migrations[index].hash, expected.name, expected.hash)
		}
	}
	if migrations[4].name != "000005_call_budget_terminal_guards.sql" {
		t.Fatalf("migration 5 name = %q, want terminal guard forward migration", migrations[4].name)
	}

	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open historical M4 database: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, migration := range migrations[:4] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			_ = db.Close()
			t.Fatalf("apply historical migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO schema_migrations(version, name, sha256, applied_at)
			VALUES (?, ?, ?, ?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			_ = db.Close()
			t.Fatalf("record historical migration %d: %v", migration.version, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close historical M4 database: %v", err)
	}

	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("upgrade historical M4 database: %v", err)
	}
	assertMigrationHistory(t, store, 32)
	for _, name := range []string{"call_records_terminal_matrix_insert", "physical_calls_terminal_parent_update"} {
		var count int
		if err := store.db.QueryRowContext(ctx,
			"SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND name = ?", name).Scan(&count); err != nil {
			t.Fatalf("inspect %s: %v", name, err)
		}
		if count != 1 {
			t.Fatalf("trigger %s count = %d, want 1", name, count)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close upgraded M4 database: %v", err)
	}
	reopened, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("reopen upgraded M4 database: %v", err)
	}
	defer reopened.Close()
	assertMigrationHistory(t, reopened, 32)
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
	assertMigrationHistory(t, store, 32)
	assertForwardWorkflowSchema(t, store)
}

// TestMigrationRecoversAppliedRetryBudgets records the previously ignored
// approved deltas in the immutable audit tables, projects their aggregate into
// run limits/accounts, and verifies a later open cannot credit them twice.
func TestMigrationRecoversAppliedRetryBudgets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-reviews.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:29] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("apply legacy migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,sha256,applied_at) VALUES(?,?,?,?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			t.Fatal(err)
		}
	}
	legacy := &Store{db: db, config: Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}, clock: clock.NewFake(testNow)}
	runID := domain.RunID("run_000000000000000000000000000000d9")
	snapshot, binding := driveRunToNeedsReview(t, legacy, runID, domain.AttemptID("attempt_000000000000000000000000000000d9"))
	var originalRequest []byte
	var originalDigest string
	if err := db.QueryRowContext(ctx, `SELECT submitted_request_json,submitted_request_digest FROM runs WHERE run_id=?`, string(runID)).Scan(&originalRequest, &originalDigest); err != nil {
		t.Fatal(err)
	}
	for index, item := range []struct {
		id    string
		key   string
		delta int64
	}{
		{id: "review_000000000000000000000000000000d1", key: "reviewcreate_000000000000000000000000000000d1", delta: 200000},
		{id: "review_000000000000000000000000000000d2", key: "reviewcreate_000000000000000000000000000000d2", delta: 300000},
	} {
		increase := domain.BudgetLimits{MaxSimilarityCostMicroUSD: item.delta}
		budgetJSON, err := json.Marshal(increase)
		if err != nil {
			t.Fatal(err)
		}
		created := testNow.Add(time.Duration(3+index*2) * time.Second)
		applied := created.Add(time.Second)
		if _, err := db.ExecContext(ctx, `INSERT INTO review_decisions(
			review_id,run_id,kind,state,expected_run_version,run_version,workflow_revision,stage_name,
			stage_input_digest,evidence_digest,policy_digest,budget_increase_json,waivable_gate,reviewer,reason,
			idempotency_key,command_digest,created_at,applied_at
		) VALUES(?,?, 'RETRY','APPLIED', ?, ?, ?, ?, ?, ?, ?, ?, 0, 'migration-test', 'legacy approved budget', ?, ?, ?, ?)`,
			item.id, string(runID), snapshot.Version+int64(index*2), snapshot.Version+int64(index*2)+1,
			binding.workflowRevision, string(binding.stage), string(binding.input), string(binding.evidence), string(binding.policy),
			budgetJSON, item.key, string(domain.SumBytes([]byte(item.id))), formatTime(created), formatTime(applied)); err != nil {
			t.Fatalf("insert legacy applied review: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `UPDATE stage_records SET state='PENDING',version=version+1,review_evidence_digest=NULL,review_policy_digest=NULL,review_waivable=NULL WHERE run_id=? AND stage_name=?`, string(runID), string(binding.stage)); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE runs SET state='CREATED',version=?,updated_at=? WHERE run_id=?`, snapshot.Version+4, formatTime(testNow.Add(8*time.Second)), string(runID)); err != nil {
		t.Fatal(err)
	}
	if err := legacy.db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("upgrade legacy applied reviews: %v", err)
	}
	checkRecovered := func() {
		t.Helper()
		assertMigrationHistory(t, store, 32)
		var runLimit, accountLimit, accountVersion int64
		if err := store.db.QueryRowContext(ctx, `SELECT max_similarity_cost_micro_usd FROM runs WHERE run_id=?`, string(runID)).Scan(&runLimit); err != nil {
			t.Fatal(err)
		}
		if err := store.db.QueryRowContext(ctx, `SELECT limit_value,account_version FROM budget_accounts WHERE run_id=? AND dimension='SIMILARITY_COST_MICRO_USD'`, string(runID)).Scan(&accountLimit, &accountVersion); err != nil {
			t.Fatal(err)
		}
		var requestAfter []byte
		var digestAfter string
		if err := store.db.QueryRowContext(ctx, `SELECT submitted_request_json,submitted_request_digest FROM runs WHERE run_id=?`, string(runID)).Scan(&requestAfter, &digestAfter); err != nil {
			t.Fatal(err)
		}
		var applications, recovered int
		if err := store.db.QueryRowContext(ctx, `SELECT count(*),sum(source='LEGACY_RECOVERY') FROM review_budget_applications WHERE run_id=?`, string(runID)).Scan(&applications, &recovered); err != nil {
			t.Fatal(err)
		}
		if runLimit != 507000 || accountLimit != runLimit || accountVersion != 2 || applications != 2 || recovered != 2 {
			t.Fatalf("recovered budget: run=%d account=%d version=%d applications=%d legacy=%d", runLimit, accountLimit, accountVersion, applications, recovered)
		}
		if !reflect.DeepEqual(requestAfter, originalRequest) || digestAfter != originalDigest {
			t.Fatal("migration changed the immutable submitted request or digest")
		}
	}
	checkRecovered()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("reopen recovered database: %v", err)
	}
	defer store.Close()
	checkRecovered()
}

// TestMigrationUpgradesM14VolumeWithoutPhysicalCallID verifies the forward
// compatibility path for the historical M14 volume shape.  M14 permitted a
// post-dispatch VOLUME row without a physical call ID; upgrading that exact
// database must not be rejected by the M16 version guard and must preserve the
// row through M17.
func TestMigrationUpgradesM14VolumeWithoutPhysicalCallID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open M14 database: %v", err)
	}
	db.SetMaxOpenConns(1)
	migrations, err := loadMigrations()
	if err != nil {
		_ = db.Close()
		t.Fatalf("load migrations: %v", err)
	}
	for _, migration := range migrations[:14] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			_ = db.Close()
			t.Fatalf("apply M14 migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES (?, ?, ?, ?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			_ = db.Close()
			t.Fatalf("record M14 migration %d: %v", migration.version, err)
		}
	}
	runID := "run_00000000000000000000000000000071"
	stage := "sandbox"
	attemptID := "attempt_00000000000000000000000000000071"
	executionID := "sandbox_00000000000000000000000000000071"
	resourceID := "resource_00000000000000000000000000000071"
	scopeDigest := domain.SumBytes([]byte("m14-volume-scope"))
	planDigest := domain.SumBytes([]byte("m14-volume-plan"))
	engineDigest := domain.SumBytes([]byte("m14-volume-engine"))
	tokenDigest := domain.SumBytes([]byte("m14-volume-token"))
	labelsDigest := domain.SumBytes([]byte("m14-volume-labels"))
	commandDigest := domain.SumBytes([]byte("m14-volume-command"))
	if _, err := db.ExecContext(ctx, `
		INSERT INTO runs(
			run_id, submitted_request_json, submitted_request_digest, effective_seed,
			redacted_effective_config_json, redacted_effective_config_digest,
			workflow_digest, workflow_revision, schema_version,
			max_llm_calls, max_similarity_calls, max_llm_input_tokens, max_llm_output_tokens,
			max_llm_cost_micro_usd, max_sandbox_creates, max_artifact_bytes, max_package_bytes,
			max_mutations_per_stage, max_active_time_ns, state, current_stage,
			current_stage_ordinal, version, create_idempotency_key, create_command_digest,
			create_result_json, created_at, updated_at
		) VALUES (?, CAST('{}' AS BLOB), ?, 1, CAST('{}' AS BLOB), ?, ?, 'm14', 'cpgen.request/v1',
			1, 1, 1, 1, 1, 1, 1, 1, 1, 1000000000, 'RUNNING', ?, 1, 2,
			'create_m14_volume_00000000000000000000000000000071', ?, CAST('{}' AS BLOB), ?, ?)`,
		runID, string(commandDigest), string(commandDigest), string(commandDigest), stage, string(commandDigest),
		formatTime(testNow), formatTime(testNow)); err != nil {
		_ = db.Close()
		t.Fatalf("insert M14 run: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO stage_records(run_id, stage_name, ordinal, workflow_revision, schema_version, state, version, input_digest,
			attempt_count, current_attempt_id, logical_idempotency_key, created_at, updated_at)
		VALUES (?, ?, 1, 'm14', 'cpgen.request/v1', 'RUNNING', 1, ?, 1, ?, 'm14-volume-stage', ?, ?)`,
		runID, stage, string(commandDigest), attemptID, formatTime(testNow), formatTime(testNow)); err != nil {
		_ = db.Close()
		t.Fatalf("insert M14 stage: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO stage_attempts(attempt_id, run_id, stage_name, ordinal, state, input_digest, started_at)
		VALUES (?, ?, ?, 1, 'RUNNING', ?, ?)`, attemptID, runID, stage, string(commandDigest), formatTime(testNow)); err != nil {
		_ = db.Close()
		t.Fatalf("insert M14 attempt: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sandbox_executions(
			sandbox_execution_id, run_id, stage_name, attempt_id, logical_operation_id,
			scope_digest, plan_digest, engine_identity_digest, watchdog_control_ref,
			watchdog_token_digest, state, lifecycle_version, cleanup_version,
			safety_deadline_utc, cleanup_deadline_utc, idempotency_key, command_digest,
			created_at, updated_at
		) VALUES (?, ?, ?, ?, 'm14-volume-op', ?, ?, ?, '/tmp/m14-volume-control',
			?, 'ARMED', 2, 0, ?, ?, 'prepare_m14_volume', ?, ?, ?)`,
		executionID, runID, stage, attemptID, string(scopeDigest), string(planDigest), string(engineDigest),
		string(tokenDigest), formatTime(testNow.Add(time.Hour)), formatTime(testNow.Add(2*time.Hour)), string(commandDigest), formatTime(testNow), formatTime(testNow)); err != nil {
		_ = db.Close()
		t.Fatalf("insert M14 execution: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO sandbox_resources(
			resource_id, sandbox_execution_id, plan_ordinal, resource_kind, resource_role,
			deterministic_name, expected_labels_digest, engine_identity_digest, phase,
			version, created_at, updated_at)
		VALUES (?, ?, 0, 'VOLUME', 'INPUT', 'cpgen-m14-volume', ?, ?, 'DISPATCHING', 3, ?, ?)`,
		resourceID, executionID, string(labelsDigest), string(engineDigest), formatTime(testNow), formatTime(testNow)); err != nil {
		_ = db.Close()
		t.Fatalf("insert M14 volume: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close M14 database: %v", err)
	}

	store, err := Open(ctx, Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("upgrade M14 database: %v", err)
	}
	defer store.Close()
	assertMigrationHistory(t, store, 32)
	var phase string
	var gotDigest sql.NullString
	if err := store.db.QueryRowContext(ctx, `SELECT phase, physical_call_id FROM sandbox_resources WHERE resource_id=?`, resourceID).Scan(&phase, &gotDigest); err != nil {
		t.Fatalf("read upgraded M14 volume: %v", err)
	}
	if phase != "DISPATCHING" || gotDigest.Valid {
		t.Fatalf("upgraded M14 volume = phase %q physical_call_id %v, want DISPATCHING/NULL", phase, gotDigest)
	}
}

// TestM16RejectsLegacyContainerWithoutPhysicalCallID verifies that the
// tightened resource-call scope does not silently reinterpret an ambiguous
// historical CONTAINER dispatch as a no-create interruption.  The migration
// must fail with the documented typed compatibility error so the caller can
// remediate the exact database.
func TestM16RejectsLegacyContainerWithoutPhysicalCallID(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatalf("open M15 database: %v", err)
	}
	db.SetMaxOpenConns(1)
	migrations, err := loadMigrations()
	if err != nil {
		_ = db.Close()
		t.Fatalf("load migrations: %v", err)
	}
	for _, migration := range migrations[:15] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			_ = db.Close()
			t.Fatalf("apply M15 migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES (?, ?, ?, ?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			_ = db.Close()
			t.Fatalf("record M15 migration %d: %v", migration.version, err)
		}
	}
	if _, err := db.ExecContext(ctx, `PRAGMA foreign_keys = OFF`); err != nil {
		_ = db.Close()
		t.Fatalf("disable foreign keys for historical fixture: %v", err)
	}
	digest := string(domain.SumBytes([]byte("legacy-container")))
	if _, err := db.ExecContext(ctx, `INSERT INTO sandbox_resources(
		resource_id, sandbox_execution_id, plan_ordinal, resource_kind, resource_role,
		deterministic_name, expected_labels_digest, engine_identity_digest, phase,
		version, created_at, updated_at)
		VALUES ('resource_00000000000000000000000000000081',
		'sandbox_00000000000000000000000000000081', 0, 'CONTAINER', 'TARGET',
		'cpgen-legacy-container', ?, ?, 'DISPATCHING', 1, ?, ?)`,
		digest, digest, formatTime(testNow), formatTime(testNow)); err != nil {
		_ = db.Close()
		t.Fatalf("insert legacy container: %v", err)
	}
	store := &Store{db: db, config: Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}, clock: clock.Real{}}
	err = store.immediate(ctx, func(tx *immediateTx) error {
		return prepareM16VolumeCompatibility(ctx, tx)
	})
	_ = store.Close()
	if !errors.Is(err, ErrMigrationCompatibility) {
		t.Fatalf("legacy container compatibility error = %v, want ErrMigrationCompatibility", err)
	}
}

func TestMigrationNineBackfillsPhysicalBytesByHistoricalPinIdentity(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	migrations, err := loadMigrations()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, migration := range migrations[:8] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			_ = db.Close()
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES (?, ?, ?, ?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			_ = db.Close()
			t.Fatalf("record migration %d: %v", migration.version, err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	ref := domain.BlobRef{Digest: domain.SumBytes([]byte("historical-owner")), Size: int64(len("historical-owner"))}
	if _, err := db.ExecContext(ctx, `INSERT INTO blobs(digest, size, state, canonical_relative_path) VALUES (?, ?, 'STAGING', ?)`, ref.Digest, ref.Size, "blobs/sha256/historical-owner"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	// Insert the later pin first but give both rows the same historical clock
	// value. The persisted row identity, not a random pin id or timestamp,
	// must determine the one physical owner.
	for _, pin := range []string{"pin_zzzz", "pin_aaaa"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO blob_pins(pin_id, writer_token_id, digest, size, state, created_at) VALUES (?, ?, ?, ?, 'ACTIVE', ?)`, pin, "writer_"+pin, ref.Digest, ref.Size, formatTime(testNow)); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	// This deliberately partial fixture isolates M9's owner backfill. Full
	// upgrades with valid writer/occurrence references are exercised separately.
	if _, err := db.ExecContext(ctx, migrations[8].sql); err != nil {
		_ = db.Close()
		t.Fatalf("upgrade historical M8 database: %v", err)
	}
	var owner string
	if err := db.QueryRowContext(ctx, `SELECT pin_id FROM artifact_blob_publication_owners WHERE digest = ? AND size = ?`, ref.Digest, ref.Size).Scan(&owner); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if owner != "pin_zzzz" {
		t.Fatalf("historical publication owner = %q, want first inserted pin pin_zzzz", owner)
	}
	rows, err := db.QueryContext(ctx, `SELECT pin_id, physical_new_bytes FROM blob_pins WHERE digest = ? AND size = ? ORDER BY rowid`, ref.Digest, ref.Size)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	defer rows.Close()
	var got []struct {
		pin   string
		bytes int64
	}
	for rows.Next() {
		var item struct {
			pin   string
			bytes int64
		}
		if err := rows.Scan(&item.pin, &item.bytes); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		got = append(got, item)
	}
	if err := rows.Err(); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].pin != "pin_zzzz" || got[0].bytes != ref.Size || got[1].bytes != 0 {
		t.Fatalf("historical physical byte owners = %+v", got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationTenBindsAndProtectsPublicationOwner(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "workflow.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	migrations, err := loadMigrations()
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, migration := range migrations[:8] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			_ = db.Close()
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES (?, ?, ?, ?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			_ = db.Close()
			t.Fatalf("record migration %d: %v", migration.version, err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	baseDigest := "sha256:" + strings.Repeat("a", 64)
	otherDigest := "sha256:" + strings.Repeat("b", 64)
	if _, err := db.ExecContext(ctx, `INSERT INTO blobs(digest, size, state, canonical_relative_path) VALUES (?, ?, 'STAGING', ?)`, baseDigest, 1, "blobs/sha256/aa"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	for _, pin := range []string{"pin_zzzz", "pin_aaaa"} {
		if _, err := db.ExecContext(ctx, `INSERT INTO blob_pins(pin_id, writer_token_id, digest, size, state, created_at) VALUES (?, ?, ?, ?, 'ACTIVE', ?)`, pin, "writer_"+pin, baseDigest, 1, formatTime(testNow)); err != nil {
			_ = db.Close()
			t.Fatalf("seed pin %s: %v", pin, err)
		}
	}
	for _, migration := range migrations[8:10] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			_ = db.Close()
			t.Fatalf("apply migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES (?, ?, ?, ?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			_ = db.Close()
			t.Fatalf("record migration %d: %v", migration.version, err)
		}
	}
	if _, err := db.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO blobs(digest, size, state, canonical_relative_path) VALUES (?, ?, 'STAGING', ?)`, otherDigest, 1, "blobs/sha256/bb"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO artifact_blob_publication_owners(digest, size, pin_id) VALUES (?, ?, ?)`, otherDigest, 1, "pin_aaaa"); err == nil {
		t.Fatal("cross-wired publication owner insert succeeded")
	}
	if _, err := db.ExecContext(ctx, `UPDATE artifact_blob_publication_owners SET pin_id = ? WHERE digest = ? AND size = ?`, "pin_aaaa", baseDigest, 1); err == nil {
		t.Fatal("publication owner update succeeded")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM artifact_blob_publication_owners WHERE digest = ? AND size = ?`, baseDigest, 1); err == nil {
		t.Fatal("publication owner delete succeeded")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
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
	assertMigrationHistory(t, store, 32)
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
	assertMigrationHistory(t, reopened, 32)
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
			request.SubmittedRequestJSON = canonicalSQLiteRunRequestJSONFor("changed", "cpgen.request/v1", request.BudgetLimits)
			request.SubmittedRequestDigest = domain.SumBytes(request.SubmittedRequestJSON)
		},
		"workflow revision": func(request *domain.CreateRunRequest) {
			request.WorkflowRevision = "slice1/changed"
		},
		"schema version": func(request *domain.CreateRunRequest) {
			request.SubmittedRequestJSON = canonicalSQLiteRunRequestJSONFor("legacy", "cpgen.request/v2", request.BudgetLimits)
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
	assertMigrationHistory(t, store, 32)
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
	if count != 32 {
		t.Fatalf("migration count = %d, want 32", count)
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
	if want >= 3 && got[2].name != "000003_call_budget.sql" {
		t.Fatalf("migration 3 name = %q", got[2].name)
	}
	if want >= 4 && got[3].name != "000004_call_budget_hardening.sql" {
		t.Fatalf("migration 4 name = %q", got[3].name)
	}
	if want >= 5 && got[4].name != "000005_call_budget_terminal_guards.sql" {
		t.Fatalf("migration 5 name = %q", got[4].name)
	}
	if want >= 6 && got[5].name != "000006_artifacts.sql" {
		t.Fatalf("migration 6 name = %q", got[5].name)
	}
	if want >= 7 && got[6].name != "000007_artifact_scope_guards.sql" {
		t.Fatalf("migration 7 name = %q", got[6].name)
	}
	if want >= 8 && got[7].name != "000008_artifact_recovery.sql" {
		t.Fatalf("migration 8 name = %q", got[7].name)
	}
	if want >= 9 && got[8].name != "000009_artifact_publication_owners.sql" {
		t.Fatalf("migration 9 name = %q", got[8].name)
	}
	if want >= 10 && got[9].name != "000010_artifact_publication_owner_guards.sql" {
		t.Fatalf("migration 10 name = %q", got[9].name)
	}
	if want >= 11 && got[10].name != "000011_cache_mutation.sql" {
		t.Fatalf("migration 11 name = %q", got[10].name)
	}
	if want >= 12 && got[11].name != "000012_cache_mutation_hardening.sql" {
		t.Fatalf("migration 12 name = %q", got[11].name)
	}
	if want >= 13 && got[12].name != "000013_cache_blob_order.sql" {
		t.Fatalf("migration 13 name = %q", got[12].name)
	}
	if want >= 14 && got[13].name != "000014_sandbox_execution.sql" {
		t.Fatalf("migration 14 name = %q", got[13].name)
	}
	if want >= 15 && got[14].name != "000015_sandbox_cleanup_evidence.sql" {
		t.Fatalf("migration 15 name = %q", got[14].name)
	}
	if want >= 16 && got[15].name != "000016_sandbox_resource_call_scope.sql" {
		t.Fatalf("migration 16 name = %q", got[15].name)
	}
	if want >= 17 && got[16].name != "000017_sandbox_volume_call_compat.sql" {
		t.Fatalf("migration 17 name = %q", got[16].name)
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
	legacyLimits := domain.BudgetLimits{
		MaxLLMCalls: 3, MaxSimilarityCalls: 2, MaxLLMInputTokens: 1000, MaxLLMOutputTokens: 1000,
		MaxLLMCostMicroUSD: 5000, MaxSimilarityCostMicroUSD: 0,
		MaxSandboxCreates: 4, MaxArtifactBytes: 1048576, MaxPackageBytes: 1048576,
		MaxMutationsPerStage: 2, MaxActiveTimeMilliseconds: 30000,
	}
	requestJSON := canonicalSQLiteRunRequestJSONFor("legacy", "cpgen.request/v1", legacyLimits)
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
		BudgetLimits:                  legacyLimits,
		StageSequence:                 []domain.StageName{"prepare", "exercise"},
		CreatedAt:                     testNow,
		IdempotencyKey:                "create_00000000000000000000000000000013",
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
	type historicalBudgetLimitsV691b611 struct {
		MaxLLMCalls               int64 `json:"max_llm_calls"`
		MaxSimilarityCalls        int64 `json:"max_similarity_calls"`
		MaxLLMInputTokens         int64 `json:"max_llm_input_tokens"`
		MaxLLMOutputTokens        int64 `json:"max_llm_output_tokens"`
		MaxLLMCostMicroUSD        int64 `json:"max_llm_cost_micro_usd"`
		MaxSandboxCreates         int64 `json:"max_sandbox_creates"`
		MaxArtifactBytes          int64 `json:"max_artifact_bytes"`
		MaxPackageBytes           int64 `json:"max_package_bytes"`
		MaxMutationsPerStage      int64 `json:"max_mutations_per_stage"`
		MaxActiveTimeMilliseconds int64 `json:"max_active_time_milliseconds"`
	}
	type historicalCreateRunRequestV691b611 struct {
		RunID                         domain.RunID                   `json:"run_id"`
		SubmittedRequestJSON          []byte                         `json:"submitted_request_json"`
		SubmittedRequestDigest        domain.Digest                  `json:"submitted_request_digest"`
		EffectiveSeed                 int64                          `json:"effective_seed"`
		RedactedEffectiveConfigJSON   []byte                         `json:"redacted_effective_config_json"`
		RedactedEffectiveConfigDigest domain.Digest                  `json:"redacted_effective_config_digest"`
		WorkflowDigest                domain.Digest                  `json:"workflow_digest"`
		BudgetLimits                  historicalBudgetLimitsV691b611 `json:"budget_limits"`
		StageSequence                 []domain.StageName             `json:"stage_sequence"`
		CreatedAt                     time.Time                      `json:"created_at"`
		IdempotencyKey                string                         `json:"idempotency_key"`
	}
	legacyLimits := historicalBudgetLimitsV691b611{
		MaxLLMCalls: request.BudgetLimits.MaxLLMCalls, MaxSimilarityCalls: request.BudgetLimits.MaxSimilarityCalls,
		MaxLLMInputTokens: request.BudgetLimits.MaxLLMInputTokens, MaxLLMOutputTokens: request.BudgetLimits.MaxLLMOutputTokens,
		MaxLLMCostMicroUSD: request.BudgetLimits.MaxLLMCostMicroUSD, MaxSandboxCreates: request.BudgetLimits.MaxSandboxCreates,
		MaxArtifactBytes: request.BudgetLimits.MaxArtifactBytes, MaxPackageBytes: request.BudgetLimits.MaxPackageBytes,
		MaxMutationsPerStage:      request.BudgetLimits.MaxMutationsPerStage,
		MaxActiveTimeMilliseconds: request.BudgetLimits.MaxActiveTimeMilliseconds,
	}
	encoded, err := json.Marshal(historicalCreateRunRequestV691b611{
		RunID: request.RunID, SubmittedRequestJSON: request.SubmittedRequestJSON, SubmittedRequestDigest: request.SubmittedRequestDigest,
		EffectiveSeed: request.EffectiveSeed, RedactedEffectiveConfigJSON: request.RedactedEffectiveConfigJSON,
		RedactedEffectiveConfigDigest: request.RedactedEffectiveConfigDigest, WorkflowDigest: request.WorkflowDigest,
		BudgetLimits: legacyLimits, StageSequence: request.StageSequence, CreatedAt: request.CreatedAt,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		t.Fatalf("marshal 691b611 CreateRun command: %v", err)
	}
	return domain.SumBytes(encoded)
}
