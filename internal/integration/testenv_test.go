package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

// integrationEnvironment is intentionally assembled through the same config
// and application entry points used by the CLI.  Integration tests must not
// replace SQLite, the filesystem store, or the process lock with an in-memory
// fake.
type integrationEnvironment struct {
	root        string
	configPath  string
	requestPath string
	cfg         config.Config
}

func newIntegrationEnvironment(t *testing.T, scenario string) integrationEnvironment {
	t.Helper()
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	configPath := filepath.Join(root, "cpgen.yaml")
	requestPath := filepath.Join(root, "request.yaml")
	configText := fmt.Sprintf("storage:\n  state_root: %q\nsqlite:\n  busy_timeout: 2s\n  max_readers: 4\nruntime:\n  lock_poll_interval: 10ms\n  control_poll_interval: 25ms\n  accounting_heartbeat: 25ms\n  cleanup_wait: 2s\nfake_workflow:\n  scenario: %s\n", stateRoot, scenario)
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	requestText := "schema_version: cpgen.request/v1\nmode: offline\nbrief: integration A+B\nlanguage: cpp\ndifficulty: easy\ntime_limit_milliseconds: 1000\nmemory_limit_megabytes: 64\nsolution_language: go\nverification_profile: default\nbudget_limits:\n  max_active_time_milliseconds: 5000\n"
	if err := os.WriteFile(requestPath, []byte(requestText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return integrationEnvironment{root: root, configPath: configPath, requestPath: requestPath, cfg: cfg}
}

func openIntegrationApp(t *testing.T, env integrationEnvironment) *application.Application {
	t.Helper()
	app, err := application.Bootstrap(context.Background(), env.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

// openIntegrationRecoveryApp composes the same SQLite/blob/lock application
// as Bootstrap, but wires the durable adapter recovery hook into the public
// RunService.Resume path. The child process only leaves committed state; it
// never repairs that state itself before being killed.
func openIntegrationRecoveryApp(t *testing.T, env integrationEnvironment, boundary string) *application.Application {
	t.Helper()
	app := openIntegrationApp(t, env)
	recovery := integrationRecovery{runtime: app.Runtime, config: env.cfg, boundary: boundary}
	return replaceIntegrationRecovery(t, env, app, recovery)
}

func replaceIntegrationRecovery(t *testing.T, env integrationEnvironment, app *application.Application, recovery application.RunRecovery) *application.Application {
	t.Helper()
	pipeline, err := workflow.NewSlice1Pipeline(
		fake.NewPrepareStep(workflow.PrepareCapabilities{}),
		fake.NewExerciseStep(workflow.ExerciseCapabilities{}),
		fake.NewCheckpointStep(workflow.CheckpointCapabilities{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewRunService(application.RunServiceConfig{
		Runtime: app.Runtime, Reviews: app.Reviews, Locks: app.Locks, Clock: clock.Real{}, Pipeline: pipeline,
		ActiveTimeInterval: env.cfg.Runtime.AccountingHeartbeat, Recovery: recovery,
		Scenario: "review",
	})
	if err != nil {
		t.Fatal(err)
	}
	app.Runs = service
	return app
}

type integrationRecovery struct {
	runtime  port.RuntimeStore
	config   config.Config
	boundary string
}

func (r integrationRecovery) RecoverRun(ctx context.Context, runID domain.RunID) error {
	if isArtifactCrashBoundary(r.boundary) && r.boundary != "occurrence_commit" {
		return recoverArtifactAfterCrash(ctx, r.runtime, r.config, runID, r.boundary)
	}
	if r.boundary == "sandbox_resource" {
		return recoverSandboxAfterCrash(ctx, r.runtime, runID)
	}
	return nil
}

var _ application.RunRecovery = integrationRecovery{}

func integrationRequest() domain.RunRequest {
	return domain.RunRequest{
		SchemaVersion: "cpgen.request/v1", Mode: "offline", Brief: "integration A+B",
		Language: "cpp", Difficulty: "easy", TimeLimitMilliseconds: 1000,
		MemoryLimitMegabytes: 64, SolutionLanguage: "go", VerificationProfile: "default",
		BudgetLimits: domain.BudgetLimits{MaxSandboxCreates: 4, MaxArtifactBytes: 1 << 20, MaxActiveTimeMilliseconds: 5000},
	}
}

const (
	integrationArtifactCallRecordID  = domain.CallRecordID("callrec_00000000000000000000000000000002")
	integrationArtifactAttemptCallID = domain.AttemptCallID("call_00000000000000000000000000000002")
	integrationArtifactReservationID = domain.ReservationID("res_00000000000000000000000000000002")
	integrationArtifactDeclarationID = domain.ArtifactDeclarationID("decl_00000000000000000000000000000001")
)

type helperProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr synchronizedBuffer
}

// synchronizedBuffer keeps diagnostics safe while the child-process stderr
// copy goroutine is still draining output during readiness failures.
type synchronizedBuffer struct {
	mu sync.Mutex
	// Do not embed Buffer: promotion of ReadFrom lets io.Copy bypass Write's
	// mutex while a readiness failure reads live subprocess diagnostics.
	buffer bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// TestSlice1IntegrationHelper is the only test entry point executed in a
// child process.  It is deliberately opt-in: invoking `go test` normally can
// never accidentally turn a test into a lock holder.
func TestSlice1IntegrationHelper(t *testing.T) {
	if os.Getenv("CPGEN_SLICE1_HELPER") != "1" {
		return
	}
	env, err := config.Load(os.Getenv("CPGEN_SLICE1_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	action := os.Getenv("CPGEN_SLICE1_HELPER_ACTION")
	runID := domain.RunID(os.Getenv("CPGEN_SLICE1_RUN_ID"))
	if err := runID.Validate(); err != nil {
		t.Fatal(err)
	}
	if action == "hold-lock" {
		manager, err := runlock.NewManager(env.Paths.Locks, runlock.Options{PollInterval: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close()
		guard, err := manager.TryAcquireRun(runID, runlock.Exclusive)
		if err != nil {
			t.Fatal(err)
		}
		defer guard.Close()
		fmt.Fprintf(os.Stdout, "READY %s\n", action)
		select {}
	}
	if action == "live-executor" {
		if err := runLiveExecutorHelper(env); err != nil {
			t.Fatal(err)
		}
		return
	}
	app, err := application.Bootstrap(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	if strings.HasPrefix(action, "block-") {
		if err := runBoundaryUntil(context.Background(), app, env, runID, "dispatching"); err != nil {
			t.Fatal(err)
		}
		// The durable dispatch transaction is closed before the named adapter
		// is entered. Each adapter below is a real blocking call site in the
		// child process, rather than a sleep after READY; the parent can now
		// kill the owner while an external operation is genuinely in flight.
		if err := blockExternalAdapter(context.Background(), strings.TrimPrefix(action, "block-")); err != nil {
			t.Fatal(err)
		}
		return
	}

	if strings.HasPrefix(action, "crash-") {
		if err := runBoundaryUntil(context.Background(), app, env, runID, strings.TrimPrefix(action, "crash-")); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(os.Stdout, "READY %s\n", action)
		select {}
	}
	t.Fatalf("unknown integration helper action %q", action)
}

type blockingExternalAdapter struct {
	name string
}

// Block is the integration equivalent of the network/Docker/blob/watchdog
// and reconciliation calls. It is deliberately invoked only after
// runBoundaryUntil has committed the call, dispatch, and budget rows. A
// second process can therefore exercise SQLite while this external call is
// blocked, proving that no write transaction spans adapter I/O.
func (a blockingExternalAdapter) Block(ctx context.Context) error {
	fmt.Fprintf(os.Stdout, "READY block-%s\n", a.name)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-make(chan struct{}):
		return nil
	}
}

func blockExternalAdapter(ctx context.Context, name string) error {
	valid := map[string]bool{"network": true, "docker": true, "blob": true, "watchdog": true, "reconciler": true}
	if !valid[name] {
		return fmt.Errorf("unknown blocking external adapter %q", name)
	}
	return blockingExternalAdapter{name: name}.Block(ctx)
}

type blockingPrepareStep struct {
	started chan<- struct{}
}

func (s *blockingPrepareStep) Name() domain.StageName { return "prepare" }

func (s *blockingPrepareStep) Run(ctx context.Context, _ domain.RunView, _ domain.Slice1Input) (domain.AgentResult[domain.Slice1Prepared], error) {
	close(s.started)
	<-ctx.Done()
	return domain.Cancelled[domain.Slice1Prepared](domain.CancellationEvidence{Cause: domain.CauseUserCancel, Evidence: domain.SumBytes([]byte("integration live cancel"))}), nil
}

func runLiveExecutorHelper(cfg config.Config) error {
	app, err := application.Bootstrap(context.Background(), cfg)
	if err != nil {
		return err
	}
	defer app.Close()
	started := make(chan struct{})
	pipeline, err := workflow.NewSlice1Pipeline(
		&blockingPrepareStep{started: started},
		fake.NewExerciseStep(workflow.ExerciseCapabilities{}),
		fake.NewCheckpointStep(workflow.CheckpointCapabilities{}),
	)
	if err != nil {
		return err
	}
	service, err := application.NewRunService(application.RunServiceConfig{
		Runtime: app.Runtime, Locks: app.Locks, Clock: clock.Real{}, Pipeline: pipeline,
		ActiveTimeInterval: cfg.Runtime.AccountingHeartbeat,
	})
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() {
		_, runErr := service.Generate(context.Background(), integrationRequest())
		done <- runErr
	}()
	select {
	case <-started:
	case err := <-done:
		return fmt.Errorf("live executor exited before readiness: %w", err)
	case <-time.After(5 * time.Second):
		return errors.New("live executor did not enter its blocking stage")
	}
	var runID domain.RunID
	deadline := time.Now().Add(5 * time.Second)
	for runID == "" && time.Now().Before(deadline) {
		runs, listErr := app.Runtime.ListRuns(context.Background(), domain.RunFilter{Limit: 2})
		if listErr != nil {
			return listErr
		}
		if len(runs) == 1 && runs[0].State == domain.RunRunning && runs[0].CurrentStage == "prepare" {
			runID = runs[0].RunID
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if runID == "" {
		return errors.New("live executor did not persist a RUNNING prepare stage")
	}
	fmt.Fprintf(os.Stdout, "READY live %s\n", runID)
	if err := <-done; err != nil {
		return fmt.Errorf("live executor: %w", err)
	}
	return nil
}

func runBoundaryUntil(ctx context.Context, app *application.Application, cfg config.Config, runID domain.RunID, boundary string) error {
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := integrationRequest()
	submitted, err := canonicalIntegrationJSON(request)
	if err != nil {
		return err
	}
	effective, err := cfg.Effective()
	if err != nil {
		return err
	}
	create := domain.CreateRunRequest{
		RunID: runID, SubmittedRequestJSON: submitted, SubmittedRequestDigest: domain.SumBytes(submitted),
		EffectiveSeed: int64(len(request.Brief)), RedactedEffectiveConfigJSON: effective,
		RedactedEffectiveConfigDigest: domain.SumBytes(effective), WorkflowRevision: workflow.Slice1WorkflowRevision,
		SchemaVersion: domain.SchemaVersion(request.SchemaVersion), WorkflowDigest: domain.SumBytes([]byte(workflow.Slice1WorkflowRevision)),
		BudgetLimits: request.BudgetLimits, StageSequence: []domain.StageName{"prepare", "exercise", "checkpoint"}, CreatedAt: now,
		IdempotencyKey: "control_00000000000000000000000000000001",
	}
	created, err := app.Runtime.CreateRun(ctx, create)
	if err != nil {
		return err
	}
	if boundary == "run_create" {
		return nil
	}
	input := domain.SumBytes([]byte("integration prepare input"))
	attemptID := domain.AttemptID("attempt_00000000000000000000000000000001")
	attempt, err := app.Runtime.BeginStage(ctx, domain.BeginStageCommand{RunID: runID, ExpectedRunVersion: created.Version, StageName: "prepare", AttemptID: attemptID, InputDigest: input, IdempotencyKey: "begin_00000000000000000000000000000001", At: now.Add(time.Microsecond)})
	if err != nil {
		return err
	}
	if boundary == "stage_begin" {
		return nil
	}

	// The following records model every metered physical durable boundary.
	// The external operation is intentionally absent: the process is killed
	// between these short, committed calls and the later resume must not mint
	// another logical or physical identity.
	ledger, ok := app.Runtime.(port.CallLedger)
	if !ok {
		return errors.New("bootstrap runtime does not expose call ledger")
	}
	policyDigest := domain.SumBytes([]byte("integration policy"))
	callRecordID := domain.CallRecordID("callrec_00000000000000000000000000000001")
	call, err := ledger.OpenCall(ctx, domain.OpenCallRequest{ID: callRecordID, RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, LogicalOperationID: "integration-sandbox", Kind: domain.CallSandboxRun, Provider: "local-test", RequestDigest: domain.SumBytes([]byte("integration request")), PolicyDigest: policyDigest, RetryPolicy: domain.RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, JitterSeedDigest: policyDigest}, IdempotencyKey: "open_00000000000000000000000000000001", At: now.Add(2 * time.Microsecond)})
	if err != nil {
		return err
	}
	physicalID := domain.AttemptCallID("call_00000000000000000000000000000001")
	reservationID := domain.ReservationID("res_00000000000000000000000000000001")
	prepared, err := ledger.PrepareCalls(ctx, domain.PrepareCallsRequest{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, CallRecordID: call.ID, PlanDigest: domain.SumBytes([]byte("integration plan")), Calls: []domain.PhysicalCallPlan{{ID: physicalID, Ordinal: 1, RetryGroup: "integration", RetryOrdinal: 1, Kind: domain.PhysicalDockerContainerCreate, Provider: "local-test", RequestDigest: domain.SumBytes([]byte("physical request")), IdempotencyKey: "physical_00000000000000000000000000000001", Reservations: []domain.ReservationPlan{{ID: reservationID, Dimension: domain.BudgetDockerContainerCreates, Subkey: "create", UpperBound: 1}}}}, IdempotencyKey: "prepare_00000000000000000000000000000001", At: now.Add(3 * time.Microsecond)})
	if err != nil {
		return err
	}
	if boundary == "budget_reserve" {
		return nil
	}
	grant, err := ledger.BeginDispatch(ctx, domain.BeginDispatchRequest{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, CallRecordID: call.ID, AttemptCallID: prepared.PhysicalCalls[0].ID, IdempotencyKey: "dispatch_00000000000000000000000000000001", At: now.Add(4 * time.Microsecond)})
	if err != nil {
		return err
	}
	if boundary == "dispatching" {
		return nil
	}
	if err := ledger.MarkSent(ctx, grant, now.Add(5*time.Microsecond)); err != nil {
		return err
	}
	if boundary == "sent" {
		return nil
	}
	responseDigest := domain.SumBytes([]byte("integration response"))
	if err := ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, CallRecordID: call.ID, AttemptCallID: physicalID, State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ProviderRequestID: "provider-integration-1", ResponseDigest: &responseDigest, Usage: []domain.ReservationUsage{{ReservationID: reservationID, Dimension: domain.BudgetDockerContainerCreates, Subkey: "create", Value: 1, Verified: true}}, IdempotencyKey: "complete_00000000000000000000000000000001", At: now.Add(6 * time.Microsecond)}); err != nil {
		return err
	}
	if boundary == "physical_completion" {
		return nil
	}
	trace, err := ledger.FinishCall(ctx, domain.FinishCallRequest{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, CallRecordID: call.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &physicalID, IdempotencyKey: "finish_call_00000000000000000000000000000001", At: now.Add(7 * time.Microsecond)})
	if err != nil {
		return err
	}
	if err := trace.Validate(); err != nil {
		return err
	}
	if boundary == "sandbox_resource" {
		return runSandboxBoundary(ctx, cfg, app.Runtime, runID, attempt.AttemptID, created.Version+1, physicalID, now)
	}
	if isArtifactCrashBoundary(boundary) {
		_, err := runArtifactBoundary(ctx, cfg, ledger, runID, attempt.AttemptID, created.Version+1, now, boundary)
		return err
	}
	if boundary != "stage_finish" && boundary != "sandbox_resource" {
		return fmt.Errorf("unknown durable boundary %q", boundary)
	}
	output := domain.SumBytes([]byte("integration prepare output"))
	_, err = app.Runtime.FinishStage(ctx, domain.FinishStageCommand{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, IdempotencyKey: "finish_00000000000000000000000000000001", At: now.Add(8 * time.Microsecond)})
	return err
}

const (
	integrationSandboxExecutionID = domain.SandboxExecutionID("sandbox_00000000000000000000000000000001")
	integrationSandboxResourceID  = domain.SandboxResourceID("resource_00000000000000000000000000000001")
)

type sandboxLifecycleStore interface {
	port.SandboxLifecycleRecorder
	port.SandboxCleanupRecorder
	port.SandboxLifecycleReader
	port.SandboxWatchdogReader
	UnfinishedSandboxExecutions(context.Context, domain.RunID) ([]domain.SandboxExecution, error)
}

func runSandboxBoundary(ctx context.Context, cfg config.Config, runtimeStore port.RuntimeStore, runID domain.RunID, attemptID domain.AttemptID, expectedRunVersion int64, physicalID domain.AttemptCallID, now time.Time) error {
	store, ok := runtimeStore.(sandboxLifecycleStore)
	if !ok {
		return errors.New("bootstrap runtime does not expose sandbox lifecycle store")
	}
	engineDigest := domain.SumBytes([]byte("integration sandbox engine"))
	scopeDigest := domain.SumBytes([]byte("integration sandbox scope"))
	planDigest := domain.SumBytes([]byte("integration sandbox plan"))
	labelsDigest := domain.SumBytes([]byte("integration sandbox labels"))
	tokenDigest := domain.SumBytes([]byte("integration sandbox watchdog token"))
	created, err := store.PrepareExecution(ctx, domain.PrepareExecutionRequest{
		ExecutionID: integrationSandboxExecutionID, RunID: runID, AttemptID: attemptID, StageName: "prepare",
		LogicalOperationID: "integration-sandbox-resource", ScopeDigest: scopeDigest, PlanDigest: planDigest,
		EngineIdentityDigest: engineDigest, WatchdogControlRef: filepath.Join(cfg.Paths.Runtime, "integration-sandbox-watchdog"), WatchdogTokenDigest: tokenDigest,
		Resources:         []domain.SandboxResource{{ID: integrationSandboxResourceID, ExecutionID: integrationSandboxExecutionID, PlanOrdinal: 0, Kind: "CONTAINER", Role: "TARGET", PhysicalCallID: &physicalID, DeterministicName: "cpgen-integration-target", ExpectedLabelsDigest: labelsDigest, EngineIdentityDigest: engineDigest, CreationNonce: "integration-sandbox-nonce", Phase: domain.SandboxResourcePlanned, Version: 1, CreatedAt: now, UpdatedAt: now}},
		SafetyDeadlineUTC: now.Add(time.Minute), CleanupDeadlineUTC: now.Add(2 * time.Minute), IdempotencyKey: "sandbox_prepare_00000000000000000000000000000001", At: now.Add(17 * time.Microsecond),
	})
	if err != nil {
		return err
	}
	if err := store.RecordWatchdogArmed(ctx, domain.WatchdogArmed{ExecutionID: created.ID, ExpectedVersion: created.LifecycleVersion, ControlRecordRef: created.WatchdogControlRef, ControlFileDigest: domain.SumBytes([]byte("integration sandbox watchdog record")), TokenDigest: tokenDigest, IdempotencyKey: "sandbox_arm_00000000000000000000000000000001", At: now.Add(18 * time.Microsecond)}); err != nil {
		return err
	}
	preCreate, err := store.BeginResourceCreate(ctx, domain.BeginResourceCreate{ExecutionID: created.ID, ResourceID: integrationSandboxResourceID, ExpectedVersion: 1, PhysicalCallID: &physicalID, IdempotencyKey: "sandbox_create_00000000000000000000000000000001", At: now.Add(19 * time.Microsecond)})
	if err != nil {
		return err
	}
	if err := store.RecordPreCreateACK(ctx, domain.PreCreateACK{ExecutionID: created.ID, ResourceID: integrationSandboxResourceID, ResourceVersion: preCreate.Version, LabelsDigest: labelsDigest, WatchdogRecordRef: created.WatchdogControlRef, IdempotencyKey: "sandbox_ack_00000000000000000000000000000001", At: now.Add(20 * time.Microsecond)}); err != nil {
		return err
	}
	_, err = store.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: created.ID, ResourceID: integrationSandboxResourceID, ExpectedVersion: preCreate.Version, Phase: domain.SandboxResourceDispatching, PhysicalCallID: &physicalID, EngineIdentityDigest: engineDigest, LabelsDigest: labelsDigest, IdempotencyKey: "sandbox_dispatch_00000000000000000000000000000001", At: now.Add(21 * time.Microsecond)})
	return err
}

func recoverSandboxAfterCrash(ctx context.Context, runtimeStore port.RuntimeStore, runID domain.RunID) error {
	store, ok := runtimeStore.(sandboxLifecycleStore)
	if !ok {
		return errors.New("bootstrap runtime does not expose sandbox lifecycle store")
	}
	execution, err := store.GetSandboxExecution(ctx, integrationSandboxExecutionID)
	if err != nil {
		return err
	}
	if len(execution.Resources) != 1 {
		return fmt.Errorf("sandbox recovery loaded %d resources, want 1", len(execution.Resources))
	}
	resource := execution.Resources[0]
	engineDigest := execution.EngineIdentityDigest
	labelsDigest := resource.ExpectedLabelsDigest
	if _, err := store.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version, Phase: domain.SandboxResourceUnknown, PhysicalCallID: resource.PhysicalCallID, EngineIdentityDigest: engineDigest, LabelsDigest: labelsDigest, IdempotencyKey: "sandbox_unknown_00000000000000000000000000000001", At: time.Now().UTC()}); err != nil {
		return err
	}
	execution, err = store.GetSandboxExecution(ctx, execution.ID)
	if err != nil {
		return err
	}
	execution, err = store.MarkCleanupPending(ctx, domain.MarkCleanupPendingCommand{ExecutionID: execution.ID, ExpectedVersion: execution.LifecycleVersion, Reason: "integration crash recovery", IdempotencyKey: "sandbox_cleanup_00000000000000000000000000000001", At: time.Now().UTC()})
	if err != nil {
		return err
	}
	execution, err = store.GetSandboxExecution(ctx, execution.ID)
	if err != nil {
		return err
	}
	resource = execution.Resources[0]
	resource, err = store.RecordResourceStopProof(ctx, domain.RecordResourceStopProofCommand{ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version, EngineResourceID: "integration-engine-resource", EngineIdentityDigest: engineDigest, LabelsDigest: labelsDigest, ProofDigest: domain.SumBytes([]byte("integration stop proof")), ProofKind: "integration-test-stop", At: time.Now().UTC()})
	if err != nil {
		return err
	}
	resource, err = store.RecordResourceCleaned(ctx, domain.RecordResourceCleanedCommand{ExecutionID: execution.ID, ResourceID: resource.ID, ExpectedVersion: resource.Version, EngineResourceID: resource.EngineResourceID, EngineIdentityDigest: engineDigest, EvidenceDigest: domain.SumBytes([]byte("integration cleanup evidence")), At: time.Now().UTC()})
	if err != nil {
		return err
	}
	_, err = store.FinishCleanup(ctx, domain.FinishCleanupCommand{ExecutionID: execution.ID, ExpectedVersion: execution.LifecycleVersion, ReconciliationDigest: domain.SumBytes([]byte("integration reconciliation")), IdempotencyKey: "sandbox_finish_00000000000000000000000000000001", At: time.Now().UTC()})
	return err
}

func assertSandboxRecovered(t *testing.T, app *application.Application, runID domain.RunID) {
	t.Helper()
	store, ok := app.Runtime.(sandboxLifecycleStore)
	if !ok {
		t.Fatal("bootstrap runtime does not expose sandbox lifecycle store")
	}
	execution, err := store.GetSandboxExecution(context.Background(), integrationSandboxExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.RunID != runID || execution.State != domain.SandboxExecutionCleaned || len(execution.Resources) != 1 || execution.Resources[0].Phase != domain.SandboxResourceCleaned {
		t.Fatalf("sandbox recovery projection = %+v", execution)
	}
	unfinished, err := store.UnfinishedSandboxExecutions(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(unfinished) != 0 {
		t.Fatalf("unfinished sandbox executions after recovery = %+v", unfinished)
	}
}

func isArtifactCrashBoundary(boundary string) bool {
	switch boundary {
	case "blob_sealed", "blob_published", "blob_ready", "occurrence_commit":
		return true
	default:
		return false
	}
}

type artifactDeclarationWriter interface {
	CreateArtifactDeclaration(context.Context, domain.ArtifactDeclarationRecord) error
}

// runArtifactBoundary drives the real SQLite artifact ledger and private CAS
// writer through named, durable edges. The process helper is killed by the
// parent immediately after this function returns at the requested edge.
func runArtifactBoundary(ctx context.Context, cfg config.Config, ledger port.CallLedger, runID domain.RunID, attemptID domain.AttemptID, expectedVersion int64, now time.Time, boundary string) (domain.PendingArtifact, error) {
	artifactLedger, ok := ledger.(port.ArtifactLedger)
	if !ok {
		return domain.PendingArtifact{}, errors.New("bootstrap runtime does not expose artifact ledger")
	}
	declarations, ok := ledger.(artifactDeclarationWriter)
	if !ok {
		return domain.PendingArtifact{}, errors.New("bootstrap runtime does not expose artifact declarations")
	}
	digest := domain.SumBytes([]byte("integration artifact request\x00" + boundary))
	policyDigest := domain.SumBytes([]byte("integration artifact policy"))
	call, err := ledger.OpenCall(ctx, domain.OpenCallRequest{
		ID: integrationArtifactCallRecordID, RunID: runID, ExpectedRunVersion: expectedVersion,
		StageName: "prepare", AttemptID: attemptID, LogicalOperationID: "integration-artifact-" + boundary,
		Kind: domain.CallSandboxRun, Provider: "local-test", RequestDigest: digest, PolicyDigest: policyDigest,
		RetryPolicy:    domain.RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, JitterSeedDigest: policyDigest},
		IdempotencyKey: "artifact_open_00000000000000000000000000000001", At: now.Add(9 * time.Microsecond),
	})
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	_, err = ledger.PrepareCalls(ctx, domain.PrepareCallsRequest{
		RunID: runID, ExpectedRunVersion: expectedVersion, StageName: "prepare", AttemptID: attemptID,
		CallRecordID: call.ID, PlanDigest: digest, Calls: []domain.PhysicalCallPlan{{
			ID: integrationArtifactAttemptCallID, Ordinal: 1, RetryGroup: "integration-artifact", RetryOrdinal: 1,
			Kind: domain.PhysicalLocalArtifactWrite, Provider: "local-test", RequestDigest: digest,
			IdempotencyKey: "artifact_physical_00000000000000000000000000000001",
			Reservations:   []domain.ReservationPlan{{ID: integrationArtifactReservationID, Dimension: domain.BudgetArtifactPhysicalNewBytes, Subkey: "artifact", UpperBound: 256}},
		}}, IdempotencyKey: "artifact_prepare_00000000000000000000000000000001", At: now.Add(10 * time.Microsecond),
	})
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	grant, err := ledger.BeginDispatch(ctx, domain.BeginDispatchRequest{
		RunID: runID, ExpectedRunVersion: expectedVersion, StageName: "prepare", AttemptID: attemptID,
		CallRecordID: call.ID, AttemptCallID: integrationArtifactAttemptCallID,
		IdempotencyKey: "artifact_dispatch_00000000000000000000000000000001", At: now.Add(11 * time.Microsecond),
	})
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	if err := ledger.MarkSent(ctx, grant, now.Add(12*time.Microsecond)); err != nil {
		return domain.PendingArtifact{}, err
	}
	declaration := domain.ArtifactDeclarationRecord{
		ID: integrationArtifactDeclarationID, RunID: runID, StageName: "prepare", AttemptID: attemptID,
		CallRecordID: call.ID, AttemptCallID: integrationArtifactAttemptCallID, ReservationID: integrationArtifactReservationID,
		ReservationSubkey: "artifact", MediaType: "text/plain", Role: domain.ArtifactEvidence,
		LogicalPath: domain.SafeRelPath("evidence/" + boundary + ".txt"), MaxBytes: 256,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "slice1-integration"}, CreatedAt: now.Add(13 * time.Microsecond),
	}
	if err := declarations.CreateArtifactDeclaration(ctx, declaration); err != nil {
		return domain.PendingArtifact{}, err
	}
	store, err := blob.NewStore(cfg.Paths.Artifacts)
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	if corruption, ok := ledger.(blob.CorruptionLedger); ok {
		if err := store.AttachCorruptionLedger(corruption); err != nil {
			return domain.PendingArtifact{}, err
		}
	}
	_, token, err := artifactLedger.PrepareArtifact(ctx, declaration.ID)
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	if err := artifactLedger.OpenArtifactWriter(ctx, token.ID); err != nil {
		return domain.PendingArtifact{}, err
	}
	writer, err := store.Prepare(ctx, port.ArtifactDeclaration{MediaType: declaration.MediaType, Role: declaration.Role, LogicalPath: declaration.LogicalPath, MaxBytes: declaration.MaxBytes, Provenance: declaration.Provenance}, blob.WriterIdentity{CallID: declaration.AttemptCallID, ReservationID: declaration.ReservationID, WriterTokenID: token.ID, PinID: token.PinID})
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	content := integrationArtifactContent(boundary)
	if _, err := writer.Write(content); err != nil {
		_ = writer.Abort(context.Background())
		return domain.PendingArtifact{}, err
	}
	pending, err := blob.Stage(ctx, writer)
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	if err := artifactLedger.SealArtifact(ctx, token.ID, pending.Blob); err != nil {
		return domain.PendingArtifact{}, err
	}
	if boundary == "blob_sealed" {
		return pending, nil
	}
	physicalNew, err := blob.Publish(ctx, writer)
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	pending.PhysicalNewBytes = physicalNew
	if boundary == "blob_published" {
		return pending, nil
	}
	if err := artifactLedger.FinalizeArtifact(ctx, token.ID, pending.Blob); err != nil {
		return domain.PendingArtifact{}, err
	}
	if boundary == "blob_ready" {
		return pending, nil
	}
	output := domain.SumBytes([]byte("integration prepare output"))
	if boundary != "occurrence_commit" {
		return pending, nil
	}
	_, err = ledger.(port.RuntimeStore).FinishStage(ctx, domain.FinishStageCommand{RunID: runID, ExpectedRunVersion: expectedVersion, StageName: "prepare", AttemptID: attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending}}, IdempotencyKey: "artifact_occurrence_00000000000000000000000000000001", At: now.Add(16 * time.Microsecond)})
	return pending, err
}

func integrationArtifactContent(boundary string) []byte {
	return []byte("integration durable artifact evidence:" + boundary + "\n")
}

// recoverArtifactAfterCrash reopens the exact public adapter transitions left
// by a killed owner. It is called by integrationRecovery from RunService's
// public Resume path, never by the child before it is killed.
func recoverArtifactAfterCrash(ctx context.Context, runtimeStore port.RuntimeStore, cfg config.Config, runID domain.RunID, boundary string) error {
	ledger, ok := runtimeStore.(port.CallLedger)
	if !ok {
		return errors.New("bootstrap runtime does not expose call ledger")
	}
	artifactLedger, ok := runtimeStore.(port.ArtifactLedger)
	if !ok {
		return errors.New("bootstrap runtime does not expose artifact ledger")
	}
	store, err := blob.NewStore(cfg.Paths.Artifacts)
	if err != nil {
		return err
	}
	if corruption, ok := ledger.(blob.CorruptionLedger); ok {
		if err := store.AttachCorruptionLedger(corruption); err != nil {
			return err
		}
	}
	prepared, err := ledger.LoadCall(ctx, integrationArtifactCallRecordID)
	if err != nil {
		return err
	}
	declaration, token, err := artifactLedger.PrepareArtifact(ctx, integrationArtifactDeclarationID)
	if err != nil {
		return err
	}
	switch token.State {
	case domain.ArtifactWriterPrepared:
		if err := artifactLedger.OpenArtifactWriter(ctx, token.ID); err != nil {
			return err
		}
		writer, err := store.Prepare(ctx, port.ArtifactDeclaration{MediaType: declaration.MediaType, Role: declaration.Role, LogicalPath: declaration.LogicalPath, MaxBytes: declaration.MaxBytes, Provenance: declaration.Provenance}, blob.WriterIdentity{CallID: declaration.AttemptCallID, ReservationID: declaration.ReservationID, WriterTokenID: token.ID, PinID: token.PinID})
		if err != nil {
			return err
		}
		if _, err := writer.Write(integrationArtifactContent(boundary)); err != nil {
			return err
		}
		pending, err := blob.Stage(ctx, writer)
		if err != nil {
			return err
		}
		if err := artifactLedger.SealArtifact(ctx, token.ID, pending.Blob); err != nil {
			return err
		}
		if _, err := blob.Publish(ctx, writer); err != nil {
			return err
		}
		if err := artifactLedger.FinalizeArtifact(ctx, token.ID, pending.Blob); err != nil {
			return err
		}
	case domain.ArtifactWriterSealed:
		if token.Blob == nil {
			return errors.New("SEALED artifact token has no blob")
		}
		writer, err := store.ResumeStaged(ctx, port.ArtifactDeclaration{MediaType: declaration.MediaType, Role: declaration.Role, LogicalPath: declaration.LogicalPath, MaxBytes: declaration.MaxBytes, Provenance: declaration.Provenance}, blob.WriterIdentity{CallID: declaration.AttemptCallID, ReservationID: declaration.ReservationID, WriterTokenID: token.ID, PinID: token.PinID}, *token.Blob)
		if err != nil {
			return err
		}
		if _, err := blob.Publish(ctx, writer); err != nil {
			return err
		}
		if err := artifactLedger.FinalizeArtifact(ctx, token.ID, *token.Blob); err != nil {
			return err
		}
	case domain.ArtifactWriterFinalized:
		if token.Blob == nil {
			return errors.New("FINALIZED artifact token has no blob")
		}
	default:
		return fmt.Errorf("unexpected artifact token state %q after %s crash", token.State, boundary)
	}
	if len(prepared.PhysicalCalls) != 1 {
		return fmt.Errorf("artifact recovery loaded %d physical calls, want 1", len(prepared.PhysicalCalls))
	}
	physical := prepared.PhysicalCalls[0]
	if physical.State == domain.PhysicalDispatching || physical.State == domain.PhysicalSent {
		if err := ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{
			RunID: runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: prepared.Call.AttemptID,
			CallRecordID: prepared.Call.ID, AttemptCallID: physical.ID, State: domain.PhysicalUnknown,
			Outcome: domain.PhysicalOutcomeUnknown, Failure: &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown},
			IdempotencyKey: "artifact_unknown_00000000000000000000000000000001", At: time.Now().UTC(),
		}); err != nil {
			return err
		}
	}
	prepared, err = ledger.LoadCall(ctx, integrationArtifactCallRecordID)
	if err != nil {
		return err
	}
	if prepared.Call.State != domain.CallRecordTerminal {
		_, err = ledger.FinishCall(ctx, domain.FinishCallRequest{
			RunID: runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: prepared.Call.AttemptID,
			CallRecordID: prepared.Call.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &physical.ID,
			IdempotencyKey: "artifact_finish_00000000000000000000000000000001", At: time.Now().UTC(),
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func assertArtifactRecovered(t *testing.T, app *application.Application, cfg config.Config, runID domain.RunID, boundary string) {
	t.Helper()
	callLedger, ok := app.Runtime.(port.CallLedger)
	if !ok {
		t.Fatal("bootstrap runtime does not expose call ledger")
	}
	prepared, err := callLedger.LoadCall(context.Background(), integrationArtifactCallRecordID)
	if err != nil {
		t.Fatalf("read recovered artifact call: %v", err)
	}
	if len(prepared.PhysicalCalls) != 1 || prepared.PhysicalCalls[0].ID != integrationArtifactAttemptCallID {
		t.Fatalf("recovered artifact physical calls = %+v, want one immutable call", prepared.PhysicalCalls)
	}
	if len(prepared.Reservations) != 1 {
		t.Fatalf("recovered artifact reservations = %+v, want one reservation", prepared.Reservations)
	}
	if boundary == "occurrence_commit" {
		if prepared.PhysicalCalls[0].State != domain.PhysicalSent || prepared.Reservations[0].State != domain.ReservationSettled || prepared.Reservations[0].SettledValue == nil {
			t.Fatalf("committed artifact physical state = %s reservations = %+v", prepared.PhysicalCalls[0].State, prepared.Reservations)
		}
	} else if prepared.PhysicalCalls[0].State != domain.PhysicalUnknown || prepared.Reservations[0].State != domain.ReservationSettled || prepared.Reservations[0].SettledValue == nil || *prepared.Reservations[0].SettledValue != prepared.Reservations[0].UpperBound {
		t.Fatalf("recovered artifact physical state = %s reservations = %+v, want UNKNOWN/upper-bound SETTLED", prepared.PhysicalCalls[0].State, prepared.Reservations)
	}
	artifactLedger, ok := app.Runtime.(port.ArtifactLedger)
	if !ok {
		t.Fatal("bootstrap runtime does not expose artifact ledger")
	}
	declaration, token, err := artifactLedger.PrepareArtifact(context.Background(), integrationArtifactDeclarationID)
	if err != nil {
		t.Fatalf("read recovered artifact token: %v", err)
	}
	if token.Blob == nil {
		t.Fatalf("recovered artifact token = %+v, want a blob", token)
	}
	if boundary == "occurrence_commit" {
		if token.State != domain.ArtifactWriterFinalized {
			t.Fatalf("committed artifact token = %+v, want FINALIZED", token)
		}
	} else if token.State != domain.ArtifactWriterReleased {
		t.Fatalf("recovered artifact token = %+v, want RELEASED", token)
	}
	store, err := blob.NewStore(cfg.Paths.Artifacts)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenVerified(context.Background(), *token.Blob)
	if err != nil {
		t.Fatalf("open recovered canonical blob: %v", err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	projection, ok := app.Runtime.(interface {
		CommittedArtifactReferences(context.Context, domain.RunID) ([]domain.CommittedArtifactRef, error)
	})
	if !ok {
		t.Fatal("bootstrap runtime does not expose committed artifact references")
	}
	occurrences, err := projection.CommittedArtifactReferences(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if boundary == "occurrence_commit" {
		if len(occurrences) != 1 {
			t.Fatalf("recovered artifact occurrences = %d, want 1", len(occurrences))
		}
		if occurrences[0].Blob != *token.Blob || occurrences[0].LogicalPath != declaration.LogicalPath {
			t.Fatalf("recovered artifact occurrence = %+v, token = %+v declaration = %+v", occurrences[0], *token.Blob, declaration)
		}
	} else if len(occurrences) != 0 {
		t.Fatalf("uncommitted artifact occurrences = %d, want 0", len(occurrences))
	}
	budgetReader, ok := app.Runtime.(interface {
		BudgetSnapshot(context.Context, domain.RunID) (domain.BudgetSnapshot, error)
	})
	if !ok {
		t.Fatal("bootstrap runtime does not expose budget projection")
	}
	budget, err := budgetReader.BudgetSnapshot(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	wantRemaining := integrationRequest().BudgetLimits.MaxArtifactBytes
	if boundary == "occurrence_commit" {
		wantRemaining -= token.Blob.Size
	} else {
		wantRemaining -= prepared.Reservations[0].UpperBound
	}
	if got := budget.Remaining[domain.BudgetArtifactPhysicalNewBytes]; got != wantRemaining {
		t.Fatalf("artifact budget remaining = %d, want %d after one settlement", got, wantRemaining)
	}
}

func canonicalIntegrationJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

// startIntegrationHelper runs this package's own test binary.  The helper
// only uses exported application/run-lock APIs and emits a readiness record
// before it blocks, so tests never synchronize with an arbitrary sleep.
func startIntegrationHelper(t *testing.T, env integrationEnvironment, action string, runID domain.RunID) *helperProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSlice1IntegrationHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		"CPGEN_SLICE1_HELPER=1",
		"CPGEN_SLICE1_HELPER_ACTION="+action,
		"CPGEN_SLICE1_CONFIG="+env.configPath,
		"CPGEN_SLICE1_RUN_ID="+string(runID),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	helper := &helperProcess{cmd: cmd, stdout: stdout}
	cmd.Stderr = &helper.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Register before waiting so readiness failures cannot orphan the helper.
	t.Cleanup(func() { helper.kill(t) })
	ready := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(stdout)
		var output []string
		for {
			line, readErr := reader.ReadString('\n')
			if strings.HasPrefix(line, "READY ") {
				ready <- nil
				return
			}
			if line != "" {
				output = append(output, line)
			}
			if readErr != nil {
				ready <- fmt.Errorf("%w (output=%q)", readErr, strings.Join(output, ""))
				return
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("helper %s did not become ready: %v; stderr=%q", action, err, helper.stderr.String())
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("helper %s readiness timed out; stderr=%q", action, helper.stderr.String())
	}
	if cmd.ProcessState != nil {
		t.Fatalf("helper %s exited immediately after readiness", action)
	}
	return helper
}

func (h *helperProcess) kill(t *testing.T) {
	t.Helper()
	if h.cmd.ProcessState == nil {
		if err := h.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) && !strings.Contains(strings.ToLower(err.Error()), "access is denied") {
			t.Fatalf("kill helper: %v", err)
		}
	}
	if h.cmd.ProcessState == nil {
		if err := h.cmd.Wait(); err != nil && h.cmd.ProcessState == nil {
			t.Fatalf("wait helper: %v", err)
		}
	}
	_ = h.stdout.Close()
}

func runCLI(t *testing.T, args ...string) (int, []byte, []byte) {
	t.Helper()
	command := exec.Command("go", append([]string{"run", "./cmd/cpgen", "--config"}, args...)...)
	command.Dir = filepath.Join("..", "..")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run cpgen: %v", err)
		}
	}
	return command.ProcessState.ExitCode(), stdout.Bytes(), stderr.Bytes()
}

func decodeEnvelope(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode CLI envelope: %v; output=%q", err, data)
	}
	return envelope
}
