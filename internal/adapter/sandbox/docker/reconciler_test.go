package docker

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
)

func TestReconcilerCreatingWithoutEngineIDSettlesNoCreate(t *testing.T) {
	now := time.Unix(123, 0).UTC()
	execution := reconcilerTestExecution()
	resource := domain.SandboxResource{
		ID: resourceID("creating"), ExecutionID: execution.ID, PlanOrdinal: 0, Kind: "CONTAINER", Role: "TARGET",
		DeterministicName: "cpgen-creating", ExpectedLabelsDigest: digestLabels(expectedResourceBaseLabels(execution, domain.SandboxResource{Kind: "CONTAINER", Role: "TARGET", PlanOrdinal: 0, DeterministicName: "cpgen-creating", ExecutionID: execution.ID})), EngineIdentityDigest: execution.EngineIdentityDigest,
		Phase: domain.SandboxResourceCreating, Version: 3, CreatedAt: now,
	}
	store := &reconcilerTestStore{}
	got, err := recordResourceNoCreate(context.Background(), store, execution, resource)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != domain.SandboxResourceInterrupted || got.EngineResourceID != "" {
		t.Fatalf("settled CREATING resource = %#v, want INTERRUPTED without engine ID", got)
	}
	if store.interrupted != 1 {
		t.Fatalf("interrupted calls = %d, want 1", store.interrupted)
	}
	if err := validatePersistedResourceIdentity(execution, resource); err != nil {
		t.Fatalf("CREATING resource without labels should remain recoverable as no-create: %v", err)
	}
}

func TestReconcilerRecoveryAfterCancellationUsesCleanupOnlySettlement(t *testing.T) {
	now := time.Unix(123, 0).UTC()
	execution := reconcilerTestExecution()
	resource := domain.SandboxResource{
		ID: resourceID("cleanup-recover"), ExecutionID: execution.ID, PlanOrdinal: 0, Kind: "CONTAINER", Role: "TARGET",
		DeterministicName: "cpgen-cleanup-recover", EngineIdentityDigest: execution.EngineIdentityDigest,
		Phase: domain.SandboxResourceSent, Version: 3, CreatedAt: now, PhysicalCallID: callID("cleanup-recover"),
	}
	labels, err := exactResourceLabels(execution, resource)
	if err != nil {
		t.Fatal(err)
	}
	resource.LabelsDigest = digestLabels(labels)
	resource.ExpectedLabelsDigest = digestLabels(expectedResourceBaseLabels(execution, resource))
	engine := &reconcilerTestEngine{inspect: moby.ContainerInspectResult{Container: container.InspectResponse{
		ID: "engine-container-id", Name: "/" + resource.DeterministicName, Config: &container.Config{Labels: labels},
	}}}
	store := &reconcilerTestStore{}
	reconciler := &sandboxReconciler{engine: engine, store: store}
	got, err := reconciler.recoverCreatedResource(context.Background(), execution, resource)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != domain.SandboxResourceCleanupPending || got.EngineResourceID != "engine-container-id" {
		t.Fatalf("recovered resource = %#v, want CLEANUP_PENDING with exact engine ID", got)
	}
	if store.advances != 1 {
		t.Fatalf("recovery advances = %d, want one cleanup-only CAS", store.advances)
	}
}

func TestValidatePersistedResourceIdentityRejectsExpectedLabelsDigestMismatch(t *testing.T) {
	execution := reconcilerTestExecution()
	resource := domain.SandboxResource{
		ID: resourceID("expected-labels"), ExecutionID: execution.ID, PlanOrdinal: 0, Kind: "CONTAINER", Role: "TARGET",
		DeterministicName: "cpgen-expected-labels", EngineIdentityDigest: execution.EngineIdentityDigest,
		Phase: domain.SandboxResourceSent, Version: 3, CreatedAt: execution.CreatedAt, PhysicalCallID: callID("expected-labels"),
	}
	labels, err := exactResourceLabels(execution, resource)
	if err != nil {
		t.Fatal(err)
	}
	resource.LabelsDigest = digestLabels(labels)
	resource.ExpectedLabelsDigest = domain.SumBytes([]byte("tampered expected labels"))
	if err := validatePersistedResourceIdentity(execution, resource); err == nil || !strings.Contains(err.Error(), "expected ownership labels digest") {
		t.Fatalf("validation error = %v, want expected-labels mismatch", err)
	}
}

func TestReconcilerRetriesRemoveFromStoppedWithoutNewStopProof(t *testing.T) {
	now := time.Unix(123, 0).UTC()
	execution := reconcilerTestExecution()
	resource := domain.SandboxResource{
		ID: resourceID("stopped-retry"), ExecutionID: execution.ID, PlanOrdinal: 0, Kind: "CONTAINER", Role: "TARGET",
		DeterministicName: "cpgen-stopped-retry", EngineResourceID: "engine-container-id",
		EngineIdentityDigest: execution.EngineIdentityDigest, Phase: domain.SandboxResourceStopped, Version: 7,
		CreatedAt: now, UpdatedAt: now, StopProofDigest: domain.SumBytes([]byte("stop-proof")), StopProofKind: "STOP_KILL_WAIT_INSPECT",
		PhysicalCallID: callID("stopped-retry"),
	}
	labels, err := exactResourceLabels(execution, resource)
	if err != nil {
		t.Fatal(err)
	}
	resource.LabelsDigest = digestLabels(labels)
	resource.ExpectedLabelsDigest = digestLabels(expectedResourceBaseLabels(execution, resource))
	engine := &reconcilerTestEngine{inspect: moby.ContainerInspectResult{Container: container.InspectResponse{
		ID: resource.EngineResourceID, Name: "/" + resource.DeterministicName, Config: &container.Config{Labels: labels},
	}}}
	store := &reconcilerTestStore{}
	reconciler := &sandboxReconciler{engine: engine, store: store}
	got, cleaned, err := reconciler.cleanupResource(context.Background(), execution, resource)
	if err != nil {
		t.Fatal(err)
	}
	if !cleaned || got.Phase != domain.SandboxResourceCleaned {
		t.Fatalf("cleanup result = %#v cleaned=%v, want CLEANED/true", got, cleaned)
	}
	if store.cleaned != 1 || engine.removeCalls != 1 {
		t.Fatalf("retry calls = cleaned %d remove %d, want 1/1", store.cleaned, engine.removeCalls)
	}
}

func TestReconcilerRecoversDispatchedResourceWithExactEngineID(t *testing.T) {
	now := time.Unix(123, 0).UTC()
	execution := reconcilerTestExecution()
	execution.State = domain.SandboxExecutionRunning
	resource := domain.SandboxResource{
		ID: resourceID("recover"), ExecutionID: execution.ID, PlanOrdinal: 0, Kind: "CONTAINER", Role: "TARGET",
		DeterministicName: "cpgen-recover", EngineIdentityDigest: execution.EngineIdentityDigest,
		Phase: domain.SandboxResourceDispatching, Version: 3, CreatedAt: now, PhysicalCallID: callID("recover"),
	}
	labels, err := exactResourceLabels(execution, resource)
	if err != nil {
		t.Fatal(err)
	}
	resource.LabelsDigest = digestLabels(labels)
	resource.ExpectedLabelsDigest = digestLabels(expectedResourceBaseLabels(execution, resource))
	engine := &reconcilerTestEngine{inspect: moby.ContainerInspectResult{Container: container.InspectResponse{
		ID: "engine-container-id", Name: "/" + resource.DeterministicName, Config: &container.Config{Labels: labels},
	}}}
	store := &reconcilerTestStore{}
	reconciler := &sandboxReconciler{engine: engine, store: store}
	got, err := reconciler.recoverCreatedResource(context.Background(), execution, resource)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != domain.SandboxResourceCompleted || got.EngineResourceID != "engine-container-id" {
		t.Fatalf("recovered resource = %#v, want COMPLETED with exact engine ID", got)
	}
	if engine.inspectCalls != 1 || store.advances != 2 {
		t.Fatalf("recovery calls = inspect %d advances %d, want 1/2", engine.inspectCalls, 2)
	}
}

func TestReconcilerDoesNotTreatMissingDispatchedResourceAsNoCreate(t *testing.T) {
	execution := reconcilerTestExecution()
	resource := domain.SandboxResource{
		ID: resourceID("missing"), ExecutionID: execution.ID, PlanOrdinal: 0, Kind: "CONTAINER", Role: "TARGET",
		DeterministicName: "cpgen-missing", EngineIdentityDigest: execution.EngineIdentityDigest,
		Phase: domain.SandboxResourceSent, Version: 3, CreatedAt: time.Unix(123, 0).UTC(), PhysicalCallID: callID("missing"),
	}
	labels, err := exactResourceLabels(execution, resource)
	if err != nil {
		t.Fatal(err)
	}
	resource.LabelsDigest = digestLabels(labels)
	resource.ExpectedLabelsDigest = digestLabels(expectedResourceBaseLabels(execution, resource))
	engine := &reconcilerTestEngine{inspectErr: errdefs.ErrNotFound}
	reconciler := &sandboxReconciler{engine: engine, store: &reconcilerTestStore{}}
	_, err = reconciler.recoverCreatedResource(context.Background(), execution, resource)
	if !errors.Is(err, errManualCleanup) || !strings.Contains(err.Error(), "external create outcome is unknown") {
		t.Fatalf("missing dispatched resource error = %v, want manual cleanup with unknown outcome", err)
	}
}

func reconcilerTestExecution() domain.SandboxExecution {
	return domain.SandboxExecution{
		ID: "sandbox_reconciler_test", RunID: "run_reconciler_test", StageName: "sandbox", AttemptID: "attempt_reconciler_test",
		LogicalOperationID: "reconciler-test", ScopeDigest: domain.SumBytes([]byte("scope")), PlanDigest: domain.SumBytes([]byte("plan")),
		EngineIdentityDigest: domain.SumBytes([]byte("engine")), WatchdogControlRef: "C:\\cpgen-watchdog-control",
		WatchdogTokenDigest: domain.SumBytes([]byte("token")), State: domain.SandboxExecutionCleanupPending,
		LifecycleVersion: 2, CreatedAt: time.Unix(123, 0).UTC(), UpdatedAt: time.Unix(123, 0).UTC(),
		SafetyDeadlineUTC: time.Unix(456, 0).UTC(), CleanupDeadlineUTC: time.Unix(789, 0).UTC(),
	}
}

func resourceID(s string) domain.SandboxResourceID {
	return domain.SandboxResourceID("resource_reconciler_test_" + s)
}

func callID(s string) *domain.AttemptCallID {
	id := domain.AttemptCallID("attempt_call_reconciler_test_" + s)
	return &id
}

type reconcilerTestStore struct {
	interrupted int
	advances    int
	cleaned     int
}

func (*reconcilerTestStore) UnfinishedSandboxExecutions(context.Context, domain.RunID) ([]domain.SandboxExecution, error) {
	return nil, nil
}

func (*reconcilerTestStore) SandboxResources(context.Context, domain.SandboxExecutionID) ([]domain.SandboxResource, error) {
	return nil, nil
}

func (*reconcilerTestStore) MarkCleanupPending(context.Context, domain.MarkCleanupPendingCommand) (domain.SandboxExecution, error) {
	return domain.SandboxExecution{}, nil
}

func (*reconcilerTestStore) FinishCleanup(context.Context, domain.FinishCleanupCommand) (domain.SandboxExecution, error) {
	return domain.SandboxExecution{}, nil
}

func (s *reconcilerTestStore) RecordResourceInterrupted(_ context.Context, req domain.RecordResourceInterruptedCommand) (domain.SandboxResource, error) {
	s.interrupted++
	return domain.SandboxResource{ID: req.ResourceID, ExecutionID: req.ExecutionID, Phase: domain.SandboxResourceInterrupted, Version: req.ExpectedVersion + 1}, nil
}

func (s *reconcilerTestStore) AdvanceResource(_ context.Context, req domain.AdvanceResourceRequest) (domain.SandboxResource, error) {
	s.advances++
	return domain.SandboxResource{ID: req.ResourceID, ExecutionID: req.ExecutionID, Phase: req.Phase, Version: req.ExpectedVersion + 1, EngineResourceID: req.EngineResourceID, LabelsDigest: req.LabelsDigest, PhysicalCallID: req.PhysicalCallID, EngineIdentityDigest: req.EngineIdentityDigest}, nil
}

func (*reconcilerTestStore) RecordResourceStopProof(context.Context, domain.RecordResourceStopProofCommand) (domain.SandboxResource, error) {
	return domain.SandboxResource{}, nil
}

func (s *reconcilerTestStore) RecordResourceCleaned(_ context.Context, req domain.RecordResourceCleanedCommand) (domain.SandboxResource, error) {
	s.cleaned++
	return domain.SandboxResource{ID: req.ResourceID, ExecutionID: req.ExecutionID, Phase: domain.SandboxResourceCleaned, Version: req.ExpectedVersion + 1, EngineResourceID: req.EngineResourceID, EngineIdentityDigest: req.EngineIdentityDigest}, nil
}

type reconcilerTestEngine struct {
	Engine
	inspect      moby.ContainerInspectResult
	inspectErr   error
	inspectCalls int
	removeCalls  int
}

func (e *reconcilerTestEngine) ContainerInspect(context.Context, string, moby.ContainerInspectOptions) (moby.ContainerInspectResult, error) {
	e.inspectCalls++
	return e.inspect, e.inspectErr
}

func (e *reconcilerTestEngine) ContainerRemove(context.Context, string, moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error) {
	e.removeCalls++
	return moby.ContainerRemoveResult{}, nil
}

var _ port.SandboxCleanupRecorder = (*reconcilerTestStore)(nil)
var _ Engine = (*reconcilerTestEngine)(nil)
