package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// SandboxReconcilerOptions binds a reconciler to one exact engine identity and
// the private lifecycle ledger. Engine calls are made outside SQLite writes.
type SandboxReconcilerOptions struct {
	Engine               Engine
	Store                SandboxReconcileStore
	EngineIdentityDigest domain.Digest
	CleanupTimeout       time.Duration
}

type sandboxReconciler struct {
	engine               Engine
	store                SandboxReconcileStore
	engineIdentityDigest domain.Digest
	cleanupTimeout       time.Duration
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
	return &sandboxReconciler{engine: options.Engine, store: options.Store, engineIdentityDigest: options.EngineIdentityDigest, cleanupTimeout: options.CleanupTimeout}, nil
}

func (r *sandboxReconciler) ReconcileRun(ctx context.Context, runID domain.RunID) (domain.SandboxReconcileReport, error) {
	if ctx == nil {
		return domain.SandboxReconcileReport{}, errors.New("reconciliation context is required")
	}
	if err := runID.Validate(); err != nil {
		return domain.SandboxReconcileReport{}, err
	}
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
	if len(report.ManualCleanup) == 0 {
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
		if inspected.Container.Config == nil || inspected.Container.ID != resource.EngineResourceID || trimContainerName(inspected.Container.Name) != resource.DeterministicName {
			return resource, false, fmt.Errorf("%w: container identity or name mismatch", errManualCleanup)
		}
		if resource.LabelsDigest != "" && digestLabels(inspected.Container.Config.Labels) != resource.LabelsDigest {
			return resource, false, fmt.Errorf("%w: container labels mismatch", errManualCleanup)
		}
		proof, err := portableStop(ctx, r.engine, resource.EngineResourceID, func(result moby.ContainerInspectResult) error {
			if result.Container.ID != resource.EngineResourceID || trimContainerName(result.Container.Name) != resource.DeterministicName {
				return fmt.Errorf("container identity changed")
			}
			if result.Container.Config == nil || (resource.LabelsDigest != "" && digestLabels(result.Container.Config.Labels) != resource.LabelsDigest) {
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
		if inspected.Volume.Name != resource.EngineResourceID || (resource.LabelsDigest != "" && digestLabels(inspected.Volume.Labels) != resource.LabelsDigest) {
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
