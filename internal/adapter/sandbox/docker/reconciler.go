package docker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

// SandboxReconciler is intentionally cleanup-only. It has no operation which
// can create, start, execute, transfer, publish, or discover resources.
type SandboxReconciler interface {
	ReconcileRun(context.Context, domain.RunID) (domain.SandboxReconcileReport, error)
}

type SandboxReconcileStore interface {
	UnfinishedSandboxExecutions(context.Context, domain.RunID) ([]domain.SandboxExecution, error)
	SandboxResources(context.Context, domain.SandboxExecutionID) ([]domain.SandboxResource, error)
	MarkCleanupPending(context.Context, domain.MarkCleanupPendingCommand) (domain.SandboxExecution, error)
	AdvanceResource(context.Context, domain.AdvanceResourceRequest) (domain.SandboxResource, error)
	FinishCleanup(context.Context, domain.FinishCleanupCommand) (domain.SandboxExecution, error)
}

// SandboxExecutionLock serializes reconciliation for one run. It is narrow
// by design: the reconciler never acquires a workflow-wide or ownership
// takeover lease. Implementations may be backed by a process-local mutex or a
// database advisory lock.
type SandboxExecutionLock interface {
	Acquire(context.Context, domain.RunID) (release func(), err error)
}

// SandboxReconcilerOptions binds a reconciler to one exact engine identity and
// the private lifecycle ledger. Engine calls are made outside SQLite writes.
type SandboxReconcilerOptions struct {
	Engine               Engine
	Store                SandboxReconcileStore
	EngineIdentityDigest domain.Digest
	CleanupTimeout       time.Duration
	ExecutionLock        SandboxExecutionLock
}

type sandboxReconciler struct {
	engine               Engine
	store                SandboxReconcileStore
	engineIdentityDigest domain.Digest
	cleanupTimeout       time.Duration
	lock                 SandboxExecutionLock
}

func NewSandboxReconciler(options SandboxReconcilerOptions) (SandboxReconciler, error) {
	if options.Engine == nil || options.Store == nil {
		return nil, errors.New("sandbox reconciler Engine and store are required")
	}
	if err := options.EngineIdentityDigest.Validate(); err != nil {
		return nil, err
	}
	if options.CleanupTimeout <= 0 {
		return nil, errors.New("sandbox reconciler cleanup timeout must be positive")
	}
	lock := options.ExecutionLock
	if lock == nil {
		lock = newRunExecutionLocks()
	}
	return &sandboxReconciler{engine: options.Engine, store: options.Store, engineIdentityDigest: options.EngineIdentityDigest, cleanupTimeout: options.CleanupTimeout, lock: lock}, nil
}

func (r *sandboxReconciler) ReconcileRun(ctx context.Context, runID domain.RunID) (domain.SandboxReconcileReport, error) {
	if ctx == nil {
		return domain.SandboxReconcileReport{}, errors.New("reconciliation context is required")
	}
	if err := runID.Validate(); err != nil {
		return domain.SandboxReconcileReport{}, err
	}
	release, err := r.lock.Acquire(ctx, runID)
	if err != nil {
		return domain.SandboxReconcileReport{}, err
	}
	defer release()
	executions, err := r.store.UnfinishedSandboxExecutions(ctx, runID)
	if err != nil {
		return domain.SandboxReconcileReport{}, err
	}
	report := domain.SandboxReconcileReport{RunID: runID, Executions: executions, Completed: true}
	for _, execution := range executions {
		resources, err := r.reconcileExecution(ctx, execution, &report)
		if err != nil {
			return report, err
		}
		report.Resources = append(report.Resources, resources...)
	}
	report.Pending = len(report.ManualCleanup)
	report.Completed = report.Pending == 0
	return report, nil
}

func (r *sandboxReconciler) reconcileExecution(ctx context.Context, execution domain.SandboxExecution, report *domain.SandboxReconcileReport) ([]domain.SandboxResource, error) {
	manualBefore := len(report.ManualCleanup)
	if execution.RunID == "" || execution.RunID != report.RunID || execution.ID == "" || execution.StageName == "" || execution.AttemptID == "" || execution.LogicalOperationID == "" {
		return nil, fmt.Errorf("persisted sandbox execution identity is incomplete or belongs to another run")
	}
	if err := execution.Validate(); err != nil {
		return nil, fmt.Errorf("persisted sandbox execution is invalid: %w", err)
	}
	resources, err := r.store.SandboxResources(ctx, execution.ID)
	if err != nil {
		return nil, err
	}
	if execution.EngineIdentityDigest != r.engineIdentityDigest {
		for _, resource := range resources {
			report.ManualCleanup = append(report.ManualCleanup, domain.SandboxCleanupBlocker{ExecutionID: execution.ID, ResourceID: resource.ID, Reason: "persisted Engine identity does not match reconciler Engine", Manual: true})
		}
		return nil, nil
	}
	for _, resource := range resources {
		if err := validatePersistedResourceIdentity(execution, resource); err != nil {
			report.ManualCleanup = append(report.ManualCleanup, domain.SandboxCleanupBlocker{ExecutionID: execution.ID, ResourceID: resource.ID, Reason: err.Error(), Manual: true})
		}
	}
	if len(report.ManualCleanup) != manualBefore {
		return resources, nil
	}
	if execution.State != domain.SandboxExecutionCleanupPending && execution.State != domain.SandboxExecutionCleaned {
		key := stableSandboxKey("reconcile_cleanup", string(execution.ID))
		execution, err = r.store.MarkCleanupPending(ctx, domain.MarkCleanupPendingCommand{ExecutionID: execution.ID, ExpectedVersion: execution.LifecycleVersion, Reason: "startup exact-resource reconciliation", IdempotencyKey: key, At: stableLifecycleTime(execution.CreatedAt, "cleanup")})
		if err != nil {
			return nil, err
		}
	}
	for index := range resources {
		resource := resources[index]
		if resource.Phase == domain.SandboxResourceCleaned || resource.Phase == domain.SandboxResourceInterrupted {
			continue
		}
		// PLANNED means no external create was authorized. It is settled as
		// interrupted without consulting the engine; cleanup must never turn
		// an uncreated plan row into a create or discovery opportunity.
		if resource.Phase == domain.SandboxResourcePlanned {
			proofs, ok := r.store.(port.SandboxCleanupRecorder)
			if !ok {
				return nil, errors.New("sandbox reconciler store lacks named cleanup proof methods")
			}
			resource, err = proofs.RecordResourceInterrupted(ctx, domain.RecordResourceInterruptedCommand{ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version, ReasonDigest: domain.SumBytes([]byte("cpgen.reconcile-no-create/v1\n" + string(resource.ID))), At: stableLifecycleTime(resource.CreatedAt, "interrupt")})
			if err != nil {
				return nil, err
			}
			resources[index] = resource
			continue
		}
		if resource.EngineIdentityDigest != r.engineIdentityDigest {
			report.ManualCleanup = append(report.ManualCleanup, domain.SandboxCleanupBlocker{ExecutionID: execution.ID, ResourceID: resource.ID, Reason: "resource Engine identity mismatch", Manual: true})
			continue
		}
		if resource.Phase != domain.SandboxResourceCleanupPending {
			key := stableSandboxKey("reconcile_resource_cleanup", string(resource.ID))
			resource, err = r.store.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version, Phase: domain.SandboxResourceCleanupPending, EngineResourceID: resource.EngineResourceID, EngineIdentityDigest: resource.EngineIdentityDigest, IdempotencyKey: key, At: stableLifecycleTime(resource.CreatedAt, "cleanup")})
			if err != nil {
				return nil, err
			}
			resources[index] = resource
		}
		if strings.TrimSpace(resource.EngineResourceID) == "" {
			report.ManualCleanup = append(report.ManualCleanup, domain.SandboxCleanupBlocker{ExecutionID: execution.ID, ResourceID: resource.ID, Reason: "resource has no persisted exact Engine identity", Manual: true})
			continue
		}
		updated, cleaned, err := r.cleanupResource(ctx, execution, resource)
		if err != nil {
			if errors.Is(err, errManualCleanup) {
				report.ManualCleanup = append(report.ManualCleanup, domain.SandboxCleanupBlocker{ExecutionID: execution.ID, ResourceID: resource.ID, Reason: err.Error(), Manual: true})
				continue
			}
			return nil, err
		}
		if cleaned {
			report.Cleaned++
		}
		resources[index] = updated
	}
	if len(report.ManualCleanup) == manualBefore {
		resourceSnapshot := make(map[int]domain.SandboxResource, len(resources))
		for _, resource := range resources {
			resourceSnapshot[resource.PlanOrdinal] = resource
		}
		key := stableSandboxKey("reconcile_finish", string(execution.ID))
		if _, err := r.store.FinishCleanup(ctx, domain.FinishCleanupCommand{ExecutionID: execution.ID, ExpectedVersion: execution.LifecycleVersion, ReconciliationDigest: reconciliationDigest(resourceSnapshot), IdempotencyKey: key, At: stableLifecycleTime(execution.CreatedAt, "finish")}); err != nil {
			return nil, err
		}
	}
	return resources, nil
}

var errManualCleanup = errors.New("manual cleanup required")

type runExecutionLocks struct {
	mu    sync.Mutex
	locks map[domain.RunID]*runExecutionLock
}

type runExecutionLock struct {
	sem  chan struct{}
	refs int
}

func newRunExecutionLocks() *runExecutionLocks {
	return &runExecutionLocks{locks: make(map[domain.RunID]*runExecutionLock)}
}

func (l *runExecutionLocks) Acquire(ctx context.Context, runID domain.RunID) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	entry := l.locks[runID]
	if entry == nil {
		entry = &runExecutionLock{sem: make(chan struct{}, 1)}
		l.locks[runID] = entry
	}
	entry.refs++
	sem := entry.sem
	l.mu.Unlock()
	select {
	case sem <- struct{}{}:
		released := false
		return func() {
			if released {
				return
			}
			released = true
			<-sem
			l.mu.Lock()
			entry.refs--
			if entry.refs == 0 {
				delete(l.locks, runID)
			}
			l.mu.Unlock()
		}, nil
	case <-ctx.Done():
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.locks, runID)
		}
		l.mu.Unlock()
		return nil, ctx.Err()
	}
}

func validatePersistedResourceIdentity(execution domain.SandboxExecution, resource domain.SandboxResource) error {
	if resource.ExecutionID != execution.ID || resource.PlanOrdinal < 0 || strings.TrimSpace(resource.DeterministicName) == "" {
		return fmt.Errorf("persisted sandbox resource %s has incomplete execution identity", resource.ID)
	}
	if resource.EngineIdentityDigest != execution.EngineIdentityDigest {
		return fmt.Errorf("persisted resource %s Engine identity does not match execution", resource.ID)
	}
	if resource.Phase == domain.SandboxResourcePlanned || resource.Phase == domain.SandboxResourceInterrupted {
		return nil
	}
	if resource.LabelsDigest == "" {
		return fmt.Errorf("persisted resource %s has no exact ownership labels digest", resource.ID)
	}
	expected, err := exactResourceLabels(execution, resource)
	if err != nil {
		return err
	}
	if digestLabels(expected) != resource.LabelsDigest {
		return fmt.Errorf("persisted resource %s ownership labels digest does not match sealed identity", resource.ID)
	}
	return nil
}

func exactResourceLabels(execution domain.SandboxExecution, resource domain.SandboxResource) (map[string]string, error) {
	labels := map[string]string{
		"org.cpgen.attempt":            string(execution.AttemptID),
		"org.cpgen.engine-digest":      string(execution.EngineIdentityDigest),
		"org.cpgen.execution-protocol": ExecutionProtocolDockerDirectV2,
		"org.cpgen.kind":               resource.Kind,
		"org.cpgen.logical-operation":  execution.LogicalOperationID,
		"org.cpgen.name":               resource.DeterministicName,
		"org.cpgen.ordinal":            strconv.Itoa(resource.PlanOrdinal),
		"org.cpgen.role":               resource.Role,
		"org.cpgen.run":                string(execution.RunID),
		"org.cpgen.slice":              "0",
		"org.cpgen.sandbox-execution":  string(execution.ID),
		"org.cpgen.plan-digest":        string(execution.PlanDigest),
	}
	if resource.Kind == "CONTAINER" {
		if resource.PhysicalCallID == nil || strings.TrimSpace(string(*resource.PhysicalCallID)) == "" {
			return nil, fmt.Errorf("persisted container resource %s has no physical call identity", resource.ID)
		}
		labels["org.cpgen.call"] = string(*resource.PhysicalCallID)
	} else {
		labels["org.cpgen.call"] = "none"
	}
	return labels, nil
}

func (r *sandboxReconciler) cleanupResource(ctx context.Context, execution domain.SandboxExecution, resource domain.SandboxResource) (domain.SandboxResource, bool, error) {
	proofs, ok := r.store.(port.SandboxCleanupRecorder)
	if !ok {
		return resource, false, errors.New("sandbox reconciler store lacks named cleanup proof methods")
	}
	stopAndRemove := func(proof StopProof, engineID string) (domain.SandboxResource, bool, error) {
		stopped, err := proofs.RecordResourceStopProof(ctx, domain.RecordResourceStopProofCommand{ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version, EngineResourceID: engineID, EngineIdentityDigest: resource.EngineIdentityDigest, LabelsDigest: resource.LabelsDigest, ProofDigest: proof.Digest(), ProofKind: "STOP_KILL_WAIT_INSPECT", At: stableLifecycleTime(resource.CreatedAt, "stop")})
		if err != nil {
			return resource, false, err
		}
		cleaned, err := proofs.RecordResourceCleaned(ctx, domain.RecordResourceCleanedCommand{ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: stopped.Version, EngineResourceID: engineID, EngineIdentityDigest: stopped.EngineIdentityDigest, EvidenceDigest: domain.SumBytes([]byte("cpgen.reconcile-remove/v1\n" + engineID)), At: stableLifecycleTime(resource.CreatedAt, "clean")})
		return cleaned, err == nil, err
	}
	switch resource.Kind {
	case "CONTAINER":
		inspected, err := r.engine.ContainerInspect(ctx, resource.EngineResourceID, moby.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			return stopAndRemove(StopProof{NotFound: true}, resource.EngineResourceID)
		}
		if err != nil {
			return resource, false, err
		}
		expectedLabels, labelErr := exactResourceLabels(execution, resource)
		if labelErr != nil {
			return resource, false, fmt.Errorf("%w: %v", errManualCleanup, labelErr)
		}
		if inspected.Container.Config == nil || inspected.Container.ID != resource.EngineResourceID || trimContainerName(inspected.Container.Name) != resource.DeterministicName {
			return resource, false, fmt.Errorf("%w: container identity or name mismatch", errManualCleanup)
		}
		if !maps.Equal(inspected.Container.Config.Labels, expectedLabels) || digestLabels(inspected.Container.Config.Labels) != resource.LabelsDigest {
			return resource, false, fmt.Errorf("%w: container labels mismatch", errManualCleanup)
		}
		proof, err := portableStop(ctx, r.engine, resource.EngineResourceID, func(result moby.ContainerInspectResult) error {
			if result.Container.ID != resource.EngineResourceID || trimContainerName(result.Container.Name) != resource.DeterministicName {
				return fmt.Errorf("container identity changed")
			}
			if result.Container.Config == nil || !maps.Equal(result.Container.Config.Labels, expectedLabels) || digestLabels(result.Container.Config.Labels) != resource.LabelsDigest {
				return fmt.Errorf("container labels changed")
			}
			return nil
		})
		if err != nil {
			return resource, false, err
		}
		if _, err := r.engine.ContainerRemove(ctx, resource.EngineResourceID, moby.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			return resource, false, err
		}
		return stopAndRemove(proof, resource.EngineResourceID)
	case "VOLUME":
		inspected, err := r.engine.VolumeInspect(ctx, resource.EngineResourceID, moby.VolumeInspectOptions{})
		if errdefs.IsNotFound(err) {
			return stopAndRemove(StopProof{NotFound: true}, resource.EngineResourceID)
		}
		if err != nil {
			return resource, false, err
		}
		expectedLabels, labelErr := exactResourceLabels(execution, resource)
		if labelErr != nil {
			return resource, false, fmt.Errorf("%w: %v", errManualCleanup, labelErr)
		}
		if inspected.Volume.Name != resource.EngineResourceID || !maps.Equal(inspected.Volume.Labels, expectedLabels) || digestLabels(inspected.Volume.Labels) != resource.LabelsDigest {
			return resource, false, fmt.Errorf("%w: volume identity or labels mismatch", errManualCleanup)
		}
		if _, err := r.engine.VolumeRemove(ctx, resource.EngineResourceID, moby.VolumeRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			return resource, false, err
		}
		return stopAndRemove(StopProof{InspectStopped: true}, resource.EngineResourceID)
	default:
		return resource, false, fmt.Errorf("%w: resource kind %q has no exact cleanup adapter", errManualCleanup, resource.Kind)
	}
}
