package docker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	watchdogprotocol "cpgen/internal/watchdog"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/events"
	moby "github.com/moby/moby/client"
)

func TestWatchdogOwnerEOFPreservesRecoveryEvidence(t *testing.T) {
	op, store := recoveryEvidenceOperation(t, false)
	if err := op.armWatchdog(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := op.watchdog.Close(); err != nil {
		t.Fatal(err)
	}
	assertRecoveryEvidence(t, store)

	// Even after all resources have been reconciled, a failed final ledger
	// transaction must leave the independent evidence available for retry.
	store.finishErr = errors.New("injected cleanup commit failure")
	reconciler := recoveryEvidenceReconciler(t, store)
	if _, err := reconciler.ReconcileRun(context.Background(), store.execution.RunID); !errors.Is(err, store.finishErr) {
		t.Fatalf("failed reconciliation = %v", err)
	}
	assertRecoveryEvidence(t, store)
	store.finishErr = nil
	assertRecoveryCompletes(t, store)
}

func TestWatchdogStartupFailurePreservesRecoveryEvidence(t *testing.T) {
	op, store := recoveryEvidenceOperation(t, true)
	if err := op.armWatchdog(context.Background(), time.Second); err == nil {
		t.Fatal("missing child executable unexpectedly started")
	}
	assertRecoveryEvidence(t, store)

	store.finishErr = errors.New("injected foreground cleanup commit failure")
	if err := op.finish(); !errors.Is(err, store.finishErr) {
		t.Fatalf("foreground cleanup = %v", err)
	}
	assertRecoveryEvidence(t, store)
	store.finishErr = nil
	assertRecoveryCompletes(t, store)
}

func TestForegroundCleanupRetiresWatchdogEvidenceAfterCommit(t *testing.T) {
	for _, detached := range []bool{false, true} {
		name := "armed session"
		if detached {
			name = "failed child startup"
		}
		t.Run(name, func(t *testing.T) {
			op, store := recoveryEvidenceOperation(t, detached)
			err := op.armWatchdog(context.Background(), time.Second)
			if (err != nil) != detached {
				t.Fatalf("arm result = %v, detached=%v", err, detached)
			}
			assertRecoveryEvidence(t, store)
			if err := op.finish(); err != nil {
				t.Fatal(err)
			}
			assertRecoveryEvidenceRetired(t, store)
		})
	}
}

func TestRunWatchdogServiceFailurePreservesRecoveryEvidence(t *testing.T) {
	op, store := recoveryEvidenceOperation(t, false)
	if err := op.armWatchdog(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	if err := op.watchdog.Close(); err != nil {
		t.Fatal(err)
	}
	// In-process envelopes omit the detached Engine configuration. A child
	// failure while reading a persisted envelope must not destroy that file.
	if err := RunWatchdogService(context.Background(), store.control.ProcessRecordRef); err == nil {
		t.Fatal("service accepted an envelope without Engine configuration")
	}
	assertRecoveryEvidence(t, store)
	assertRecoveryCompletes(t, store)
}

func TestPrepareDoesNotOverwriteRecoveryEvidence(t *testing.T) {
	op, store := recoveryEvidenceOperation(t, true)
	if err := op.armWatchdog(context.Background(), time.Second); err == nil {
		t.Fatal("missing child executable unexpectedly started")
	}
	data, err := secureReadWatchdogControl(store.control.ProcessRecordRef, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := watchdogprotocol.ParseEnvelope(data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := op.runner.watchdog.(WatchdogPreparer).Prepare(context.Background(), envelope.Record); err == nil {
		t.Fatal("repreparation replaced unreconciled control evidence")
	}
	assertRecoveryEvidence(t, store)
	assertRecoveryCompletes(t, store)
}

func TestFailedWatchdogPersistenceReleasesUnreferencedPreparation(t *testing.T) {
	for _, detached := range []bool{false, true} {
		for _, boundary := range []string{"read", "prepare", "cancel"} {
			t.Run(fmt.Sprintf("detached=%v/%s", detached, boundary), func(t *testing.T) {
				op, store := recoveryEvidenceOperation(t, detached)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				injected := errors.New("injected lifecycle persistence failure")
				switch boundary {
				case "read":
					store.readErrorAt, store.readErr = 2, injected
				case "prepare":
					store.prepareErr = injected
				case "cancel":
					store.prepareHook = cancel
					store.prepareErr, injected = context.Canceled, context.Canceled
				}
				if err := op.armWatchdog(ctx, time.Second); !errors.Is(err, injected) {
					t.Fatalf("arm result = %v, want %v", err, injected)
				}
				if store.execution.ID != "" {
					t.Fatal("failure unexpectedly committed an execution")
				}
				evidence := op.watchdogControl.(WatchdogSessionEvidence)
				if _, err := os.Stat(filepath.Dir(evidence.ControlRecordRef())); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unreferenced preparation blocks a retry: %v", err)
				}
				// A retry with the same deterministic identity must prepare a
				// fresh envelope after the independent no-row check, even when
				// the failed caller's context was canceled.
				store.prepareErr, store.prepareHook = nil, nil
				op.watchdog = nil
				err := op.armWatchdog(context.Background(), time.Second)
				if (err != nil) != detached {
					t.Fatalf("retry arm result = %v, detached=%v", err, detached)
				}
				assertRecoveryEvidence(t, store)
				if err := op.finish(); err != nil {
					t.Fatal(err)
				}
				assertRecoveryEvidenceRetired(t, store)
			})
		}
	}
}

func TestFailedWatchdogPersistenceRetainsUncertainPreparation(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprintf("committed=%v", committed), func(t *testing.T) {
			op, store := recoveryEvidenceOperation(t, true)
			store.prepareErr = errors.New("injected uncertain commit result")
			store.commitOnPrepareError = committed
			if !committed {
				// An error mentioning absence is not a typed proof that it is
				// safe to delete recovery evidence.
				store.readErrorAt = 3
				store.readErr = errors.New("database not found during lookup")
			}
			if err := op.armWatchdog(context.Background(), time.Second); !errors.Is(err, store.prepareErr) {
				t.Fatalf("arm result = %v", err)
			}
			evidence := op.watchdogControl.(WatchdogSessionEvidence)
			raw, err := secureReadWatchdogControl(evidence.ControlRecordRef(), 1<<20)
			if err != nil || domain.SumBytes(raw) != evidence.ControlFileDigest() {
				t.Fatalf("uncertain preparation lost its exact evidence: %v", err)
			}
			envelope, err := watchdogprotocol.ParseEnvelope(raw)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := op.runner.watchdog.(WatchdogPreparer).Prepare(context.Background(), envelope.Record); err == nil {
				t.Fatal("uncertain preparation was overwritten")
			}
		})
	}
}

func recoveryEvidenceOperation(t *testing.T, detached bool) (*operation, *recoveryEvidenceStore) {
	t.Helper()
	identity := PlanIdentity{
		RunID: "run_00000000000000000000000000000001", AttemptID: "attempt_00000000000000000000000000000001",
		SandboxExecutionID: "sandbox_00000000000000000000000000000001", LogicalOperationID: "watchdog-recovery",
		OperationNonce: "0123456789abcdef0123456789abcdef", EngineIdentityDigest: domain.SumBytes([]byte("engine")),
	}
	ordinal := 0
	resource := port.PlannedResource{Ordinal: 0, Kind: port.ResourceContainer, Role: port.ResourceTarget,
		DeterministicName: "cpgen-s0-0123456789abcdef0123456789abcdef-00-ctr-target", CreateCallOrdinal: &ordinal}
	resource.ExpectedLabelsDigest = digestLabels(baseResourceLabels(identity, resource))
	plan, err := port.NewContainerPlan(identity.EngineIdentityDigest, []port.PlannedResource{resource}, 0)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "unix:///var/run/docker.sock"
	if runtime.GOOS == "windows" {
		endpoint = "npipe:////./pipe/docker_engine"
	}
	image := string(domain.SumBytes([]byte("image")))
	config := Config{EngineEndpoint: endpoint, APIVersion: RequiredAPIVersion, BuilderImage: image, RuntimeImage: image,
		TransferImage: image, ExecutionProtocol: ExecutionProtocolDockerDirectV2}
	engine := &recoveryEvidenceEngine{}
	var controller WatchdogController
	if detached {
		controlRoot, rootErr := os.MkdirTemp("", "cpgen-recovery-")
		if rootErr != nil {
			t.Fatal(rootErr)
		}
		t.Cleanup(func() { _ = os.RemoveAll(controlRoot) })
		controller, err = NewDetachedWatchdogController(DetachedWatchdogOptions{Config: config, EngineIdentity: identity.EngineIdentityDigest,
			ControlDirectory: controlRoot, Executable: filepath.Join(t.TempDir(), "missing-executable"), ArmTimeout: time.Second})
	} else {
		controller, err = NewInProcessWatchdogController("recovery-watchdog-token-00000000000000000001", engine)
	}
	if err != nil {
		t.Fatal(err)
	}
	store := &recoveryEvidenceStore{}
	op := &operation{runner: &Runner{engine: engine, watchdog: controller, lifecycle: store, clock: clock.Real{}, config: config,
		limits: ControlLimits{CleanupTimeout: time.Second}}, identity: identity, plan: plan,
		auth: recoveryEvidenceAuthorization{scope: domain.SumBytes([]byte("scope"))}}
	t.Cleanup(func() {
		if op.watchdog != nil {
			_ = op.watchdog.Close()
		}
		if op.watchdogControl != nil {
			_ = op.watchdogControl.ReleaseControl()
		}
	})
	return op, store
}

func recoveryEvidenceReconciler(t *testing.T, store *recoveryEvidenceStore) SandboxReconciler {
	t.Helper()
	// Any Engine call would panic: the persisted resources were never
	// dispatched, and the startup path must only settle their no-create proof.
	r, err := NewSandboxReconciler(SandboxReconcilerOptions{Engine: &recoveryEvidenceEngine{}, Store: store,
		EngineIdentityDigest: store.execution.EngineIdentityDigest, CleanupTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func assertRecoveryEvidence(t *testing.T, store *recoveryEvidenceStore) {
	t.Helper()
	if err := verifyPersistedWatchdogControl(store.execution, store.control); err != nil {
		t.Fatalf("durable recovery evidence was lost: %v", err)
	}
}

func assertRecoveryCompletes(t *testing.T, store *recoveryEvidenceStore) {
	t.Helper()
	report, err := recoveryEvidenceReconciler(t, store).ReconcileRun(context.Background(), store.execution.RunID)
	if err != nil || !report.Completed || len(report.ManualCleanup) != 0 {
		t.Fatalf("startup reconciliation = %+v, %v", report, err)
	}
	assertRecoveryEvidenceRetired(t, store)
}

func assertRecoveryEvidenceRetired(t *testing.T, store *recoveryEvidenceStore) {
	t.Helper()
	if store.execution.State != domain.SandboxExecutionCleaned || !store.evidenceAtCommit {
		t.Fatalf("cleanup was not committed with evidence intact: %+v", store.execution)
	}
	if _, err := os.Stat(filepath.Dir(store.control.ProcessRecordRef)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed cleanup retained control credentials: %v", err)
	}
}

type recoveryEvidenceAuthorization struct {
	port.SandboxDispatchAuthorization
	scope domain.Digest
}

func (a recoveryEvidenceAuthorization) ScopeDigest() domain.Digest         { return a.scope }
func (recoveryEvidenceAuthorization) AbortRemaining(context.Context) error { return nil }

type recoveryEvidenceEngine struct{ Engine }

func (*recoveryEvidenceEngine) Events(context.Context, moby.EventsListOptions) moby.EventsResult {
	return moby.EventsResult{Messages: make(chan events.Message), Err: make(chan error)}
}
func (*recoveryEvidenceEngine) ContainerInspect(context.Context, string, moby.ContainerInspectOptions) (moby.ContainerInspectResult, error) {
	return moby.ContainerInspectResult{}, errdefs.ErrNotFound
}

type recoveryEvidenceStore struct {
	port.SandboxLifecycleRecorder
	port.SandboxCleanupRecorder
	execution            domain.SandboxExecution
	control              domain.SandboxWatchdogControl
	finishErr            error
	evidenceAtCommit     bool
	prepareErr           error
	prepareHook          func()
	commitOnPrepareError bool
	readCount            int
	readErrorAt          int
	readErr              error
}

func (s *recoveryEvidenceStore) GetSandboxExecution(ctx context.Context, _ domain.SandboxExecutionID) (domain.SandboxExecution, error) {
	if err := ctx.Err(); err != nil {
		return domain.SandboxExecution{}, err
	}
	s.readCount++
	if s.readCount == s.readErrorAt {
		return domain.SandboxExecution{}, s.readErr
	}
	if s.execution.ID == "" {
		return domain.SandboxExecution{}, fmt.Errorf("sandbox execution not found: %w", sql.ErrNoRows)
	}
	return s.execution, nil
}
func (s *recoveryEvidenceStore) PrepareExecution(_ context.Context, req domain.PrepareExecutionRequest) (domain.SandboxExecution, error) {
	if s.prepareHook != nil {
		s.prepareHook()
	}
	if s.prepareErr != nil && !s.commitOnPrepareError {
		return domain.SandboxExecution{}, s.prepareErr
	}
	s.execution = domain.SandboxExecution{ID: req.ExecutionID, RunID: req.RunID, AttemptID: req.AttemptID, StageName: req.StageName,
		LogicalOperationID: req.LogicalOperationID, ScopeDigest: req.ScopeDigest, PlanDigest: req.PlanDigest,
		EngineIdentityDigest: req.EngineIdentityDigest, WatchdogControlRef: req.WatchdogControlRef, WatchdogTokenDigest: req.WatchdogTokenDigest,
		SafetyDeadlineUTC: req.SafetyDeadlineUTC, CleanupDeadlineUTC: req.CleanupDeadlineUTC, CreatedAt: req.At, UpdatedAt: req.At,
		State: domain.SandboxExecutionPlanned, LifecycleVersion: 1, Resources: req.Resources}
	return s.execution, errors.Join(s.execution.Validate(), s.prepareErr)
}
func (s *recoveryEvidenceStore) RecordWatchdogArmed(_ context.Context, req domain.WatchdogArmed) error {
	s.control = domain.SandboxWatchdogControl{ExecutionID: req.ExecutionID, ControlID: req.ControlRecordRef,
		ProcessRecordRef: req.ControlRecordRef, ControlFileDigest: req.ControlFileDigest, TokenDigest: req.TokenDigest}
	s.execution.State = domain.SandboxExecutionArmed
	s.execution.LifecycleVersion++
	return nil
}
func (s *recoveryEvidenceStore) GetSandboxWatchdogControl(context.Context, domain.SandboxExecutionID) (domain.SandboxWatchdogControl, error) {
	return s.control, nil
}
func (s *recoveryEvidenceStore) UnfinishedSandboxExecutions(context.Context, domain.RunID) ([]domain.SandboxExecution, error) {
	if s.execution.State == domain.SandboxExecutionCleaned {
		return nil, nil
	}
	return []domain.SandboxExecution{s.execution}, nil
}
func (s *recoveryEvidenceStore) SandboxResources(context.Context, domain.SandboxExecutionID) ([]domain.SandboxResource, error) {
	return append([]domain.SandboxResource(nil), s.execution.Resources...), nil
}
func (s *recoveryEvidenceStore) MarkCleanupPending(_ context.Context, req domain.MarkCleanupPendingCommand) (domain.SandboxExecution, error) {
	s.execution.State = domain.SandboxExecutionCleanupPending
	s.execution.LifecycleVersion++
	return s.execution, nil
}
func (s *recoveryEvidenceStore) FinishCleanup(context.Context, domain.FinishCleanupCommand) (domain.SandboxExecution, error) {
	if s.finishErr != nil {
		return domain.SandboxExecution{}, s.finishErr
	}
	if err := verifyPersistedWatchdogControl(s.execution, s.control); err != nil {
		return domain.SandboxExecution{}, err
	}
	s.evidenceAtCommit = true
	s.execution.State = domain.SandboxExecutionCleaned
	s.execution.LifecycleVersion++
	return s.execution, nil
}
func (s *recoveryEvidenceStore) RecordResourceInterrupted(_ context.Context, req domain.RecordResourceInterruptedCommand) (domain.SandboxResource, error) {
	for i, resource := range s.execution.Resources {
		if resource.ID == req.ResourceID {
			resource.Phase = domain.SandboxResourceInterrupted
			resource.Version++
			s.execution.Resources[i] = resource
			return resource, nil
		}
	}
	return domain.SandboxResource{}, errors.New("resource not found")
}
