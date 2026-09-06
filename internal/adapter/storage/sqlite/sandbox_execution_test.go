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
	"cpgen/internal/port"
)

func TestSandboxExecutionMigrationAndPreparePersistCompletePlanBeforeCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := Open(context.Background(), Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()
	var name string
	if err := store.db.QueryRowContext(context.Background(), "SELECT name FROM schema_migrations WHERE version = 14").Scan(&name); err != nil {
		t.Fatalf("read sandbox migration: %v", err)
	}
	if name != "000014_sandbox_execution.sql" {
		t.Fatalf("migration name = %q", name)
	}
	if _, err := store.PrepareExecution(context.Background(), domain.PrepareExecutionRequest{}); err == nil {
		t.Fatal("expected incomplete prepare request to be rejected")
	}
	_ = port.ResourceContainer
}

func TestSandboxResourceLifecycleUsesCAS(t *testing.T) {
	var _ port.SandboxLifecycleRecorder = (*Store)(nil)
	if _, err := (&Store{}).AdvanceResource(context.Background(), domain.AdvanceResourceRequest{}); err == nil {
		t.Fatal("expected closed store to reject lifecycle advance")
	}
}

func TestSandboxExecutionLifecyclePersistsBeforeCreateAndSettlesWithCAS(t *testing.T) {
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "workflow.db"), clock.NewFake(testNow))
	created := mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, 30*time.Second))
	mustBeginStage(t, store, testRunID, testAttemptID, created.Version, "prepare", domain.SumBytes([]byte("sandbox input")), testNow.Add(time.Second), "begin_sandbox_00000000000000000000000000000001")

	engineIdentity := domain.SumBytes([]byte("engine identity"))
	scopeDigest := domain.SumBytes([]byte("scope"))
	planDigest := domain.SumBytes([]byte("sealed plan"))
	tokenDigest := domain.SumBytes([]byte("watchdog token"))
	executionID := domain.SandboxExecutionID("sandbox_00000000000000000000000000000001")
	resourceID := domain.SandboxResourceID("resource_00000000000000000000000000000001")
	createdAt := testNow.Add(2 * time.Second)
	resource := domain.SandboxResource{
		ID: resourceID, ExecutionID: executionID, PlanOrdinal: 0, Kind: "CONTAINER", Role: "TARGET",
		DeterministicName: "cpgen-target-0001", ExpectedLabelsDigest: domain.SumBytes([]byte("expected labels")),
		EngineIdentityDigest: engineIdentity, Phase: domain.SandboxResourcePlanned, Version: 1,
		CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	prepare := domain.PrepareExecutionRequest{
		ExecutionID: executionID, RunID: testRunID, AttemptID: testAttemptID, StageName: "prepare",
		LogicalOperationID: "sandbox-lifecycle-test", ScopeDigest: scopeDigest, PlanDigest: planDigest,
		EngineIdentityDigest: engineIdentity, Resources: []domain.SandboxResource{resource},
		WatchdogControlRef: "sandbox-control-0001", WatchdogTokenDigest: tokenDigest,
		SafetyDeadlineUTC: createdAt.Add(time.Minute), CleanupDeadlineUTC: createdAt.Add(2 * time.Minute),
		IdempotencyKey: "prepare_00000000000000000000000000000001", At: createdAt,
	}
	execution, err := store.PrepareExecution(ctx, prepare)
	if err != nil {
		t.Fatalf("PrepareExecution: %v", err)
	}
	if execution.State != domain.SandboxExecutionPlanned || execution.LifecycleVersion != 1 || len(execution.Resources) != 1 || execution.Resources[0].Phase != domain.SandboxResourcePlanned {
		t.Fatalf("prepared execution = %+v", execution)
	}
	if replay, err := store.PrepareExecution(ctx, prepare); err != nil || !reflect.DeepEqual(replay, execution) {
		t.Fatalf("PrepareExecution replay = %+v, %v", replay, err)
	}

	armAt := createdAt.Add(time.Second)
	if err := store.RecordWatchdogArmed(ctx, domain.WatchdogArmed{
		ExecutionID: executionID, ExpectedVersion: 1, ControlRecordRef: prepare.WatchdogControlRef,
		ControlFileDigest: domain.SumBytes([]byte("control file")), TokenDigest: tokenDigest,
		IdempotencyKey: "arm_00000000000000000000000000000001", At: armAt,
	}); err != nil {
		t.Fatalf("RecordWatchdogArmed: %v", err)
	}
	if err := store.RecordWatchdogArmed(ctx, domain.WatchdogArmed{
		ExecutionID: executionID, ExpectedVersion: 1, ControlRecordRef: prepare.WatchdogControlRef,
		ControlFileDigest: domain.SumBytes([]byte("control file")), TokenDigest: tokenDigest,
		IdempotencyKey: "arm_00000000000000000000000000000001", At: armAt,
	}); err != nil {
		t.Fatalf("RecordWatchdogArmed replay: %v", err)
	}
	if err := store.RecordWatchdogArmed(ctx, domain.WatchdogArmed{
		ExecutionID: executionID, ExpectedVersion: 1, ControlRecordRef: prepare.WatchdogControlRef,
		ControlFileDigest: domain.SumBytes([]byte("different control file")), TokenDigest: tokenDigest,
		IdempotencyKey: "arm_00000000000000000000000000000002", At: armAt,
	}); !errors.Is(err, ErrVersionConflict) && !errors.Is(err, ErrConsistency) {
		t.Fatalf("changed watchdog arm = %v, want conflict/consistency", err)
	}

	pre, err := store.BeginResourceCreate(ctx, domain.BeginResourceCreate{ExecutionID: executionID, ResourceID: resourceID, ExpectedVersion: 1, IdempotencyKey: "creating_00000000000000000000000000000001", At: armAt.Add(time.Second)})
	if err != nil {
		t.Fatalf("BeginResourceCreate: %v", err)
	}
	if pre.Version != 2 || pre.Resource.Phase != domain.SandboxResourceCreating {
		t.Fatalf("pre-create = %+v", pre)
	}
	labelsDigest := domain.SumBytes([]byte("actual labels"))
	if err := store.RecordPreCreateACK(ctx, domain.PreCreateACK{ExecutionID: executionID, ResourceID: resourceID, ResourceVersion: pre.Version, LabelsDigest: labelsDigest, WatchdogRecordRef: prepare.WatchdogControlRef, IdempotencyKey: "precreate_00000000000000000000000000000001", At: armAt.Add(2 * time.Second)}); err != nil {
		t.Fatalf("RecordPreCreateACK: %v", err)
	}
	if _, err := store.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: executionID, ResourceID: resourceID, ExpectedVersion: pre.Version, Phase: domain.SandboxResourceDispatching, EngineIdentityDigest: engineIdentity, LabelsDigest: labelsDigest, IdempotencyKey: "dispatch_00000000000000000000000000000001", At: armAt.Add(3 * time.Second)}); err != nil {
		t.Fatalf("AdvanceResource DISPATCHING: %v", err)
	}
	if _, err := store.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: executionID, ResourceID: resourceID, ExpectedVersion: 3, Phase: domain.SandboxResourceSent, EngineIdentityDigest: engineIdentity, LabelsDigest: labelsDigest, IdempotencyKey: "sent_00000000000000000000000000000001", At: armAt.Add(4 * time.Second)}); err != nil {
		t.Fatalf("AdvanceResource SENT: %v", err)
	}
	completed, err := store.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: executionID, ResourceID: resourceID, ExpectedVersion: 4, Phase: domain.SandboxResourceCompleted, EngineResourceID: "container-exact-id", EngineIdentityDigest: engineIdentity, LabelsDigest: labelsDigest, IdempotencyKey: "complete_00000000000000000000000000000001", At: armAt.Add(5 * time.Second)})
	if err != nil {
		t.Fatalf("AdvanceResource COMPLETED: %v", err)
	}
	if completed.Phase != domain.SandboxResourceCompleted || completed.EngineResourceID != "container-exact-id" || completed.Version != 5 {
		t.Fatalf("completed resource = %+v", completed)
	}
	started, err := store.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: executionID, ResourceID: resourceID, ExpectedVersion: 5, Phase: domain.SandboxResourceStarted, EngineResourceID: completed.EngineResourceID, EngineIdentityDigest: engineIdentity, IdempotencyKey: "started_00000000000000000000000000000001", At: armAt.Add(6 * time.Second)})
	if err != nil {
		t.Fatalf("AdvanceResource STARTED: %v", err)
	}
	if started.Phase != domain.SandboxResourceStarted || started.Version != 6 {
		t.Fatalf("started resource = %+v", started)
	}
	if _, err := store.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: executionID, ResourceID: resourceID, ExpectedVersion: 5, Phase: domain.SandboxResourceStarted, EngineResourceID: completed.EngineResourceID, EngineIdentityDigest: engineIdentity, IdempotencyKey: "started_00000000000000000000000000000002", At: armAt.Add(6 * time.Second)}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale resource transition = %v, want ErrVersionConflict", err)
	}

	pending, err := store.MarkCleanupPending(ctx, domain.MarkCleanupPendingCommand{ExecutionID: executionID, ExpectedVersion: 2, Reason: "test cleanup", IdempotencyKey: "cleanup_00000000000000000000000000000001", At: armAt.Add(6 * time.Second)})
	if err != nil {
		t.Fatalf("MarkCleanupPending: %v", err)
	}
	if pending.State != domain.SandboxExecutionCleanupPending || pending.LifecycleVersion != 3 {
		t.Fatalf("pending execution = %+v", pending)
	}
	stopped, err := store.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: executionID, ResourceID: resourceID, ExpectedVersion: 6, Phase: domain.SandboxResourceCleanupPending, EngineResourceID: completed.EngineResourceID, EngineIdentityDigest: engineIdentity, IdempotencyKey: "cleanup_00000000000000000000000000000002", At: armAt.Add(7 * time.Second)})
	if err != nil {
		t.Fatalf("resource cleanup pending: %v", err)
	}
	proofDigest := domain.SumBytes([]byte("stop proof"))
	stopped, err = store.RecordResourceStopProof(ctx, domain.RecordResourceStopProofCommand{ExecutionID: executionID, ResourceID: resourceID, ExpectedVersion: stopped.Version, EngineResourceID: stopped.EngineResourceID, EngineIdentityDigest: engineIdentity, LabelsDigest: labelsDigest, ProofDigest: proofDigest, ProofKind: "STOP_KILL_WAIT_INSPECT", At: armAt.Add(8 * time.Second)})
	if err != nil {
		t.Fatalf("resource stopped: %v", err)
	}
	cleaned, err := store.RecordResourceCleaned(ctx, domain.RecordResourceCleanedCommand{ExecutionID: executionID, ResourceID: resourceID, ExpectedVersion: stopped.Version, EngineResourceID: stopped.EngineResourceID, EngineIdentityDigest: engineIdentity, EvidenceDigest: domain.SumBytes([]byte("remove evidence")), At: armAt.Add(9 * time.Second)})
	if err != nil {
		t.Fatalf("resource cleaned: %v", err)
	}
	if cleaned.Phase != domain.SandboxResourceCleaned {
		t.Fatalf("cleaned resource = %+v", cleaned)
	}
	finished, err := store.FinishCleanup(ctx, domain.FinishCleanupCommand{ExecutionID: executionID, ExpectedVersion: pending.LifecycleVersion, ReconciliationDigest: domain.SumBytes([]byte("reconciliation")), IdempotencyKey: "finish_00000000000000000000000000000001", At: armAt.Add(10 * time.Second)})
	if err != nil {
		t.Fatalf("FinishCleanup: %v", err)
	}
	if finished.State != domain.SandboxExecutionCleaned || finished.LifecycleVersion != 4 {
		t.Fatalf("finished execution = %+v", finished)
	}
}
