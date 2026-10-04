package docker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	watchdogprotocol "cpgen/internal/watchdog"
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

// The default lock is process-wide so two independently constructed
// reconcilers cannot concurrently mutate the same run. Callers which need a
// cross-process lock can inject an implementation backed by the run lock or
// database; the nil default still provides the required shared process
// serialization.
var defaultSandboxExecutionLocks = newRunExecutionLocks()

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
		lock = defaultSandboxExecutionLocks
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
	if controlReader, ok := r.store.(port.SandboxWatchdogReader); ok {
		control, controlErr := controlReader.GetSandboxWatchdogControl(ctx, execution.ID)
		if controlErr != nil {
			report.ManualCleanup = append(report.ManualCleanup, domain.SandboxCleanupBlocker{ExecutionID: execution.ID, Reason: fmt.Sprintf("watchdog control evidence unavailable: %v", controlErr), Manual: true})
			return nil, nil
		}
		if controlErr := verifyPersistedWatchdogControl(execution, control); controlErr != nil {
			report.ManualCleanup = append(report.ManualCleanup, domain.SandboxCleanupBlocker{ExecutionID: execution.ID, Reason: controlErr.Error(), Manual: true})
			return nil, nil
		}
	} else {
		report.ManualCleanup = append(report.ManualCleanup, domain.SandboxCleanupBlocker{ExecutionID: execution.ID, Reason: "sandbox reconciler store lacks watchdog control evidence reader", Manual: true})
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
			resource, err = recordResourceNoCreate(ctx, r.store, execution, resource)
			if err != nil {
				return nil, err
			}
			resources[index] = resource
			continue
		}
		// CREATING is durably before the dispatch boundary: the runner enters
		// this phase before recording the pre-create ACK and before authorizing
		// the Docker request.  A crash here therefore has a safe, deterministic
		// no-create outcome.  Never apply this shortcut to DISPATCHING/SENT,
		// where the external create may already have crossed the boundary.
		if resource.Phase == domain.SandboxResourceCreating && strings.TrimSpace(resource.EngineResourceID) == "" {
			resource, err = recordResourceNoCreate(ctx, r.store, execution, resource)
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
		if strings.TrimSpace(resource.EngineResourceID) == "" &&
			(resource.Phase == domain.SandboxResourceDispatching || resource.Phase == domain.SandboxResourceSent) {
			// The exact returned Engine ID was not durably recorded before the
			// process stopped.  Probe only the immutable deterministic name and
			// complete ownership labels; this is recovery, never a create or a
			// name-only delete.  If no exact object is present, retain a manual
			// cleanup blocker because the create boundary is ambiguous.
			recovered, recoverErr := r.recoverCreatedResource(ctx, execution, resource)
			if recoverErr != nil {
				if errors.Is(recoverErr, errManualCleanup) {
					report.ManualCleanup = append(report.ManualCleanup, domain.SandboxCleanupBlocker{
						ExecutionID: execution.ID, ResourceID: resource.ID,
						Reason: recoverErr.Error() + "; cleanup pending",
						Manual: true,
					})
					continue
				}
				return nil, recoverErr
			}
			resource = recovered
			resources[index] = resource
		}
		// STOPPED already carries an immutable stop proof. It is a cleanup
		// retry state, not new work; leave it in place so a crash between the
		// proof and the remove can be retried without a forbidden transition.
		if resource.Phase != domain.SandboxResourceCleanupPending && resource.Phase != domain.SandboxResourceStopped {
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
		// The independently verified envelope is no longer needed once the
		// ledger durably records every resource's cleanup proof.
		if err := cleanupWatchdogControl(filepath.Dir(execution.WatchdogControlRef), execution.WatchdogControlRef); err != nil {
			return nil, fmt.Errorf("release reconciled watchdog control: %w", err)
		}
	}
	return resources, nil
}

func recordResourceNoCreate(ctx context.Context, store any, execution domain.SandboxExecution, resource domain.SandboxResource) (domain.SandboxResource, error) {
	proofs, ok := store.(port.SandboxCleanupRecorder)
	if !ok {
		return domain.SandboxResource{}, errors.New("sandbox reconciler store lacks named cleanup proof methods")
	}
	return proofs.RecordResourceInterrupted(ctx, domain.RecordResourceInterruptedCommand{
		ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version,
		ReasonDigest: domain.SumBytes([]byte("cpgen.reconcile-no-create/v1\n" + string(resource.ID))),
		At:           stableLifecycleTime(resource.CreatedAt, "interrupt"),
	})
}

// verifyPersistedWatchdogControl validates both the immutable database row
// and the owner-only control file. Reconciliation must never trust a path or
// digest merely because it was persisted by a prior process: the file is an
// independently readable, fsynced envelope whose full execution identity and
// deadlines must still match the ledger.
func verifyPersistedWatchdogControl(execution domain.SandboxExecution, control domain.SandboxWatchdogControl) error {
	if control.ExecutionID != execution.ID || control.ControlID != execution.WatchdogControlRef || control.ProcessRecordRef != execution.WatchdogControlRef {
		return fmt.Errorf("persisted watchdog control reference differs from sandbox execution")
	}
	if control.TokenDigest != execution.WatchdogTokenDigest {
		return fmt.Errorf("persisted watchdog token digest differs from sandbox execution")
	}
	if !filepath.IsAbs(execution.WatchdogControlRef) {
		return fmt.Errorf("persisted watchdog control reference is not an absolute owner-only path")
	}
	data, err := secureReadWatchdogControl(execution.WatchdogControlRef, 1<<20)
	if err != nil {
		return fmt.Errorf("read persisted watchdog control: %w", err)
	}
	if domain.SumBytes(data) != control.ControlFileDigest {
		return fmt.Errorf("persisted watchdog control file digest differs from ledger")
	}
	envelope, err := watchdogprotocol.ParseEnvelope(data)
	if err != nil {
		return fmt.Errorf("parse persisted watchdog control: %w", err)
	}
	record := envelope.Record
	if record.RunID != execution.RunID || record.AttemptID != execution.AttemptID || record.SandboxExecutionID != execution.ID ||
		record.LogicalOperationID != execution.LogicalOperationID || record.ScopeDigest != execution.ScopeDigest || record.Plan.PlanDigest != execution.PlanDigest ||
		record.EngineIdentityDigest != execution.EngineIdentityDigest || record.TokenDigest != execution.WatchdogTokenDigest ||
		!record.SafetyDeadlineUTC.Equal(execution.SafetyDeadlineUTC) ||
		!record.CleanupDeadlineUTC.Equal(execution.CleanupDeadlineUTC) {
		return fmt.Errorf("persisted watchdog envelope identity or deadline differs from sandbox execution")
	}
	if envelope.RecordDigest != domainDigestRecord(record) {
		return fmt.Errorf("persisted watchdog envelope record digest is invalid")
	}
	return nil
}

// recoverCreatedResource closes the crash window between an Engine Create
// response and the lifecycle row recording its exact ID.  It deliberately
// probes the immutable deterministic name and verifies the complete ownership
// label set before writing COMPLETED.  An absent or mismatched object is not
// treated as a no-create result because DISPATCHING/SENT means the external
// boundary may already have been crossed.
func (r *sandboxReconciler) recoverCreatedResource(ctx context.Context, execution domain.SandboxExecution, resource domain.SandboxResource) (domain.SandboxResource, error) {
	expectedLabels, err := exactResourceLabels(execution, resource)
	if err != nil {
		return resource, fmt.Errorf("%w: cannot recover resource identity: %v", errManualCleanup, err)
	}
	if resource.ExpectedLabelsDigest != digestLabels(expectedResourceBaseLabels(execution, resource)) {
		return resource, fmt.Errorf("%w: persisted expected ownership labels differ from immutable plan", errManualCleanup)
	}
	if resource.LabelsDigest != "" && resource.LabelsDigest != digestLabels(expectedLabels) {
		return resource, fmt.Errorf("%w: persisted ownership labels differ from immutable plan", errManualCleanup)
	}
	engineID := ""
	switch resource.Kind {
	case "CONTAINER":
		inspected, inspectErr := r.engine.ContainerInspect(ctx, resource.DeterministicName, moby.ContainerInspectOptions{})
		if errdefs.IsNotFound(inspectErr) {
			return resource, fmt.Errorf("%w: %s %s was not found after dispatch; external create outcome is unknown", errManualCleanup, resource.Kind, resource.DeterministicName)
		}
		if inspectErr != nil {
			return resource, inspectErr
		}
		if inspected.Container.Config == nil || inspected.Container.ID == "" || trimContainerName(inspected.Container.Name) != resource.DeterministicName || !maps.Equal(inspected.Container.Config.Labels, expectedLabels) {
			return resource, fmt.Errorf("%w: recovered container identity or labels do not match the immutable plan", errManualCleanup)
		}
		engineID = inspected.Container.ID
	case "VOLUME":
		inspected, inspectErr := r.engine.VolumeInspect(ctx, resource.DeterministicName, moby.VolumeInspectOptions{})
		if errdefs.IsNotFound(inspectErr) {
			return resource, fmt.Errorf("%w: %s %s was not found after dispatch; external create outcome is unknown", errManualCleanup, resource.Kind, resource.DeterministicName)
		}
		if inspectErr != nil {
			return resource, inspectErr
		}
		if inspected.Volume.Name != resource.DeterministicName || !maps.Equal(inspected.Volume.Labels, expectedLabels) {
			return resource, fmt.Errorf("%w: recovered volume identity or labels do not match the immutable plan", errManualCleanup)
		}
		engineID = inspected.Volume.Name
	default:
		return resource, fmt.Errorf("%w: resource kind %q cannot be recovered", errManualCleanup, resource.Kind)
	}

	labelsDigest := digestLabels(expectedLabels)
	if execution.State == domain.SandboxExecutionCleanupPending || execution.State == domain.SandboxExecutionInterrupted {
		// Startup recovery first settles the execution into CLEANUP_PENDING.
		// Once cancellation is durable, COMPLETED/SENT are new-work edges and
		// are rejected by the storage cancel guard. Persist the exact engine ID
		// directly on the cleanup-only edge instead.
		return r.store.AdvanceResource(ctx, domain.AdvanceResourceRequest{
			ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version,
			Phase: domain.SandboxResourceCleanupPending, PhysicalCallID: resource.PhysicalCallID,
			EngineResourceID: engineID, EngineIdentityDigest: resource.EngineIdentityDigest,
			LabelsDigest: labelsDigest, IdempotencyKey: stableSandboxKey("reconcile_recovered_cleanup", string(resource.ID)),
			At: stableLifecycleTime(resource.CreatedAt, "recovered_cleanup"),
		})
	}
	if resource.Phase == domain.SandboxResourceDispatching {
		// The schema's monotone transition requires SENT before COMPLETED.
		resource, err = r.store.AdvanceResource(ctx, domain.AdvanceResourceRequest{
			ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version,
			Phase: domain.SandboxResourceSent, PhysicalCallID: resource.PhysicalCallID,
			EngineResourceID: engineID, EngineIdentityDigest: resource.EngineIdentityDigest,
			LabelsDigest: labelsDigest, IdempotencyKey: stableSandboxKey("reconcile_recovered_sent", string(resource.ID)),
			At: stableLifecycleTime(resource.CreatedAt, "recovered_sent"),
		})
		if err != nil {
			return resource, err
		}
	}
	return r.store.AdvanceResource(ctx, domain.AdvanceResourceRequest{
		ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version,
		Phase: domain.SandboxResourceCompleted, PhysicalCallID: resource.PhysicalCallID,
		EngineResourceID: engineID, EngineIdentityDigest: resource.EngineIdentityDigest,
		LabelsDigest: labelsDigest, IdempotencyKey: stableSandboxKey("reconcile_recovered_complete", string(resource.ID)),
		At: stableLifecycleTime(resource.CreatedAt, "recovered_complete"),
	})
}

// domainDigestRecord mirrors watchdog's private canonical record digest. The
// envelope parser already validates it; this small canonical check only
// rejects JSON that was altered after the persisted file digest was recorded.
func domainDigestRecord(record watchdogprotocol.ControlRecord) domain.Digest {
	encoded, err := json.Marshal(record)
	if err != nil {
		return ""
	}
	return domain.SumBytes(encoded)
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
	if resource.Kind == "VOLUME" && strings.TrimSpace(resource.EngineResourceID) != "" && resource.EngineResourceID != resource.DeterministicName {
		return fmt.Errorf("persisted volume resource %s Engine identity does not match sealed deterministic name", resource.ID)
	}
	if resource.ExpectedLabelsDigest != digestLabels(expectedResourceBaseLabels(execution, resource)) {
		return fmt.Errorf("persisted resource %s expected ownership labels digest does not match sealed identity", resource.ID)
	}
	// CREATING is persisted before the external create boundary. It may not
	// have a complete ownership-label record yet, but it is safe to settle as
	// no-create when no engine ID was persisted.
	if resource.Phase == domain.SandboxResourcePlanned || resource.Phase == domain.SandboxResourceCreating || resource.Phase == domain.SandboxResourceInterrupted {
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
	labels := expectedResourceBaseLabels(execution, resource)
	labels["org.cpgen.plan-digest"] = string(execution.PlanDigest)
	if resource.Kind == "CONTAINER" {
		if resource.PhysicalCallID == nil || strings.TrimSpace(string(*resource.PhysicalCallID)) == "" {
			return nil, fmt.Errorf("persisted container resource %s has no physical call identity", resource.ID)
		}
		labels["org.cpgen.call"] = string(*resource.PhysicalCallID)
	} else if resource.Kind == "VOLUME" {
		if resource.PhysicalCallID == nil || strings.TrimSpace(string(*resource.PhysicalCallID)) == "" {
			return nil, fmt.Errorf("persisted volume resource %s has no physical call identity", resource.ID)
		}
		labels["org.cpgen.call"] = string(*resource.PhysicalCallID)
	} else {
		labels["org.cpgen.call"] = "none"
	}
	return labels, nil
}

func expectedResourceBaseLabels(execution domain.SandboxExecution, resource domain.SandboxResource) map[string]string {
	return map[string]string{
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
	}

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
	recordCleanedAfterExistingStopProof := func(engineID string) (domain.SandboxResource, bool, error) {
		cleaned, err := proofs.RecordResourceCleaned(ctx, domain.RecordResourceCleanedCommand{
			ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version,
			EngineResourceID: engineID, EngineIdentityDigest: resource.EngineIdentityDigest,
			EvidenceDigest: domain.SumBytes([]byte("cpgen.reconcile-remove/v1\n" + engineID)),
			At:             stableLifecycleTime(resource.CreatedAt, "clean"),
		})
		return cleaned, err == nil, err
	}
	existingStopProof := resource.Phase == domain.SandboxResourceStopped
	switch resource.Kind {
	case "CONTAINER":
		inspected, err := r.engine.ContainerInspect(ctx, resource.EngineResourceID, moby.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			if existingStopProof {
				return recordCleanedAfterExistingStopProof(resource.EngineResourceID)
			}
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
		if existingStopProof {
			if _, err := r.engine.ContainerRemove(ctx, resource.EngineResourceID, moby.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
				return resource, false, err
			}
			return recordCleanedAfterExistingStopProof(resource.EngineResourceID)
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
			if existingStopProof {
				return recordCleanedAfterExistingStopProof(resource.EngineResourceID)
			}
			return stopAndRemove(StopProof{NotFound: true}, resource.EngineResourceID)
		}
		if err != nil {
			return resource, false, err
		}
		expectedLabels, labelErr := exactResourceLabels(execution, resource)
		if labelErr != nil {
			return resource, false, fmt.Errorf("%w: %v", errManualCleanup, labelErr)
		}
		if inspected.Volume.Name != resource.EngineResourceID || inspected.Volume.Name != resource.DeterministicName || !maps.Equal(inspected.Volume.Labels, expectedLabels) || digestLabels(inspected.Volume.Labels) != resource.LabelsDigest {
			return resource, false, fmt.Errorf("%w: volume identity or labels mismatch", errManualCleanup)
		}
		if _, err := r.engine.VolumeRemove(ctx, resource.EngineResourceID, moby.VolumeRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			return resource, false, err
		}
		if existingStopProof {
			return recordCleanedAfterExistingStopProof(resource.EngineResourceID)
		}
		return stopAndRemove(StopProof{InspectStopped: true}, resource.EngineResourceID)
	default:
		return resource, false, fmt.Errorf("%w: resource kind %q has no exact cleanup adapter", errManualCleanup, resource.Kind)
	}
}
