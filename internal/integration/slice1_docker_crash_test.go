//go:build cpgen_slice0_probe

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dockersandbox "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/probe"
	"cpgen/internal/watchdog"
	"cpgen/internal/workflow"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
)

const (
	dockerProbeRunID        = domain.RunID("run_00000000000000000000000000000081")
	dockerProbeCallRecordID = domain.CallRecordID("callrec_00000000000000000000000000000081")
	dockerProbeAttemptID    = domain.AttemptID("attempt_00000000000000000000000000000081")
	dockerProbeAttemptCall  = domain.AttemptCallID("call_00000000000000000000000000000081")
)

func TestSlice1DockerCrash(t *testing.T) {
	// Perform capability probing in the parent so a missing daemon is an
	// explicit canary skip rather than a child that silently exits before READY.
	_, _, _ = loadSlice1DockerCanary(t)
	env := newIntegrationEnvironment(t, "review")
	first := startSlice1DockerProcess(t, env, true)
	first.kill(t)
	firstOutput := first.output()
	second := startSlice1DockerProcess(t, env, false)
	secondOutput := waitSlice1DockerProcess(t, second)
	firstCall := dockerReadyCallLine(firstOutput)
	secondCall := dockerRecoveryCallLine(secondOutput)
	if firstCall == "" || secondCall == "" {
		t.Fatalf("Docker crash canary omitted durable evidence: first=%q second=%q", firstOutput, secondOutput)
	}
	if firstCall != secondCall {
		t.Fatalf("Docker recovery changed the durable physical identity: first=%q second=%q", firstCall, secondCall)
	}
}

func TestSlice1DockerWatchdogFailure(t *testing.T) {
	dockerConfig, _, _ := loadSlice1DockerCanary(t)
	marker := filepath.Join(t.TempDir(), "watchdog-result")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSlice1DockerWatchdogOwnerHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "CPGEN_RUN_DOCKER_CANARY=1", "CPGEN_SLICE1_DOCKER_WATCHDOG_OWNER=1", "CPGEN_SLICE1_DOCKER_WATCHDOG_MARKER="+marker)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	var output bytes.Buffer
	go func() {
		scanner := bufio.NewScanner(stdout)
		seenReady := false
		for scanner.Scan() {
			line := scanner.Text()
			output.WriteString(line)
			output.WriteByte('\n')
			if strings.HasPrefix(line, "READY watchdog ") && !seenReady {
				seenReady = true
				ready <- nil
			}
		}
		if !seenReady {
			ready <- errors.Join(scanner.Err(), fmt.Errorf("watchdog owner exited before READY"))
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("watchdog owner readiness: %v; stderr=%q", err, stderr.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("watchdog owner readiness timed out; stderr=%q", stderr.String())
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill watchdog owner: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("watchdog owner unexpectedly exited cleanly")
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(marker); readErr == nil {
			if !strings.HasPrefix(string(data), "ok resource=") {
				t.Fatalf("watchdog EOF result = %q; owner output=%q stderr=%q", data, output.String(), stderr.String())
			}
			resourceName := strings.TrimPrefix(strings.SplitN(string(data), " id=", 2)[0], "ok resource=")
			engine, engineErr := dockersandbox.NewEngineClient(dockerConfig)
			if engineErr != nil {
				t.Fatal(engineErr)
			}
			_, inspectErr := engine.ContainerInspect(context.Background(), resourceName, moby.ContainerInspectOptions{})
			_ = engine.Close()
			if !errdefs.IsNotFound(inspectErr) {
				t.Fatalf("watchdog cleanup left container %q: %v", resourceName, inspectErr)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("watchdog child did not observe owner EOF; output=%q stderr=%q", output.String(), stderr.String())
}

// TestSlice1DockerCrashHelper is a real owner process. Its first invocation
// commits the run/stage/call/DISPATCHING rows and then enters the real Docker
// Runner's Ping call through a gate. The parent kills it while that external
// call is in flight. A second process reopens the same state root and invokes
// the public RunService.Resume recovery path against the exact call identity.
func TestSlice1DockerCrashHelper(t *testing.T) {
	if os.Getenv("CPGEN_SLICE1_DOCKER_HELPER") != "1" {
		return
	}
	cfg, err := config.Load(os.Getenv("CPGEN_SLICE1_DOCKER_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CPGEN_SLICE1_DOCKER_HELPER_RECOVER") == "1" {
		app := openIntegrationApp(t, integrationEnvironment{cfg: cfg})
		recovery := dockerProbeRecovery{runtime: app.Runtime}
		replaceIntegrationRecovery(t, integrationEnvironment{cfg: cfg}, app, recovery)
		runID := domain.RunID(os.Getenv("CPGEN_SLICE1_DOCKER_RUN_ID"))
		resumed, err := app.Runs.Resume(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		ledger, ok := app.Runtime.(port.CallLedger)
		if !ok {
			t.Fatal("Docker recovery runtime does not expose call ledger")
		}
		prepared, err := ledger.LoadCall(context.Background(), dockerProbeCallRecordID)
		if err != nil {
			t.Fatal(err)
		}
		if len(prepared.PhysicalCalls) != 1 || prepared.PhysicalCalls[0].State != domain.PhysicalUnknown {
			t.Fatalf("Docker recovery call projection = %+v", prepared)
		}
		if resumed.State != domain.RunNeedsReview {
			t.Fatalf("Docker recovery run = %+v, want NEEDS_REVIEW", resumed)
		}
		fmt.Fprintf(os.Stdout, "DONE recovery call=%s state=%s\n", prepared.PhysicalCalls[0].ID, prepared.PhysicalCalls[0].State)
		return
	}

	callID, auth, runner, closeRunner, err := prepareDurableDockerProbe(t, cfg, domain.RunID(os.Getenv("CPGEN_SLICE1_DOCKER_RUN_ID")), true)
	if err != nil {
		t.Fatal(err)
	}
	defer closeRunner()
	// READY is emitted by the gate immediately before the real Docker client
	// receives Ping. No completion/trace is printed before the owner is killed.
	result, err := runner.Probe(context.Background(), auth, port.DockerProbeRequest{Profile: string(port.ProfileCompileV2)})
	if err != nil {
		t.Fatal(err)
	}
	if err := result.CallTrace.Validate(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(os.Stdout, "DONE unexpected-completion call=%s\n", callID)
}

type slice1DockerProcess struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan struct{}
	mu     sync.Mutex
}

func startSlice1DockerProcess(t *testing.T, env integrationEnvironment, hold bool) *slice1DockerProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSlice1DockerCrashHelper$", "-test.v")
	processEnv := append(os.Environ(), "CPGEN_RUN_DOCKER_CANARY=1", "CPGEN_SLICE1_DOCKER_HELPER=1", "CPGEN_SLICE1_DOCKER_CONFIG="+env.configPath, "CPGEN_SLICE1_DOCKER_RUN_ID="+string(dockerProbeRunID))
	if !hold {
		processEnv = append(processEnv, "CPGEN_SLICE1_DOCKER_HELPER_RECOVER=1")
	}
	cmd.Env = processEnv
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	process := &slice1DockerProcess{cmd: cmd, done: make(chan struct{})}
	cmd.Stderr = &process.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		defer close(process.done)
		scanner := bufio.NewScanner(stdout)
		seenReady := false
		for scanner.Scan() {
			line := scanner.Text()
			process.mu.Lock()
			process.stdout.WriteString(line)
			process.stdout.WriteByte('\n')
			process.mu.Unlock()
			if strings.HasPrefix(line, "READY ") && !seenReady {
				seenReady = true
				ready <- nil
			}
		}
		if !seenReady {
			ready <- errors.Join(scanner.Err(), fmt.Errorf("Docker helper exited before READY"))
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("Docker helper readiness: %v; stderr=%q", err, process.stderr.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("Docker helper readiness timed out; stderr=%q", process.stderr.String())
	}
	return process
}

func (p *slice1DockerProcess) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdout.String()
}

func (p *slice1DockerProcess) kill(t *testing.T) {
	t.Helper()
	if p.cmd.ProcessState == nil {
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("kill Docker helper: %v", err)
		}
	}
	if p.cmd.ProcessState == nil {
		if err := p.cmd.Wait(); err != nil && p.cmd.ProcessState == nil {
			t.Fatalf("wait Docker helper: %v", err)
		}
	}
	<-p.done
}

func waitSlice1DockerProcess(t *testing.T, p *slice1DockerProcess) string {
	t.Helper()
	if err := p.cmd.Wait(); err != nil {
		t.Fatalf("Docker helper resume: %v; stderr=%q", err, p.stderr.String())
	}
	<-p.done
	return p.output()
}

func dockerTraceLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "DONE trace=") {
			return strings.TrimPrefix(line, "DONE trace=")
		}
	}
	return ""
}

func dockerReadyCallLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "READY docker-probe call=") {
			return strings.TrimPrefix(line, "READY docker-probe call=")
		}
	}
	return ""
}

func dockerRecoveryCallLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "DONE recovery call=") {
			value := strings.TrimPrefix(line, "DONE recovery call=")
			return strings.TrimSuffix(strings.SplitN(value, " state=", 2)[0], "\r")
		}
	}
	return ""
}

type durableEnginePingClaims struct {
	identity port.ProbeAuthorizationIdentity
	callID   domain.AttemptCallID
}

func (c durableEnginePingClaims) ClaimEnginePing(_ context.Context, identity port.ProbeAuthorizationIdentity) (domain.AttemptCallID, error) {
	if identity != c.identity {
		return "", errors.New("durable Docker probe identity changed during recovery")
	}
	return c.callID, nil
}
func (durableEnginePingClaims) ClaimContainer(context.Context, port.ProbeAuthorizationIdentity, int, port.ContainerRole) (domain.AttemptCallID, error) {
	return "", errors.New("durable Docker probe does not authorize container claims")
}
func (durableEnginePingClaims) AbortRemaining(context.Context, port.ProbeAuthorizationIdentity) error {
	return nil
}

type gatedDockerEngine struct {
	dockersandbox.Engine
	hold  bool
	ready sync.Once
}

func (e *gatedDockerEngine) Ping(ctx context.Context, options moby.PingOptions) (moby.PingResult, error) {
	e.ready.Do(func() { fmt.Fprintf(os.Stdout, "READY docker-probe call=%s\n", dockerProbeAttemptCall) })
	if e.hold {
		select {
		case <-ctx.Done():
			return moby.PingResult{}, ctx.Err()
		case <-make(chan struct{}):
		}
	}
	return e.Engine.Ping(ctx, options)
}

// prepareDurableDockerProbe commits the complete local preparation through
// SQLite before constructing the real Docker Runner. The only in-memory part
// is the claim facade, which returns the already-persisted physical key.
func prepareDurableDockerProbe(t *testing.T, cfg config.Config, runID domain.RunID, hold bool) (domain.AttemptCallID, port.SandboxDispatchAuthorization, *dockersandbox.Runner, func(), error) {
	t.Helper()
	app, err := application.Bootstrap(context.Background(), cfg)
	if err != nil {
		return "", nil, nil, func() {}, err
	}
	ledger, ok := app.Runtime.(port.CallLedger)
	if !ok {
		_ = app.Close()
		return "", nil, nil, func() {}, errors.New("Docker probe runtime does not expose call ledger")
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := integrationRequest()
	submitted, err := canonicalIntegrationJSON(request)
	if err != nil {
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	effective, err := cfg.Effective()
	if err != nil {
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	create := domain.CreateRunRequest{RunID: runID, SubmittedRequestJSON: submitted, SubmittedRequestDigest: domain.SumBytes(submitted), EffectiveSeed: int64(len(request.Brief)), RedactedEffectiveConfigJSON: effective, RedactedEffectiveConfigDigest: domain.SumBytes(effective), WorkflowRevision: workflow.Slice1WorkflowRevision, SchemaVersion: domain.SchemaVersion(request.SchemaVersion), WorkflowDigest: domain.SumBytes([]byte(workflow.Slice1WorkflowRevision)), BudgetLimits: request.BudgetLimits, StageSequence: []domain.StageName{"prepare", "exercise", "checkpoint"}, CreatedAt: now, IdempotencyKey: "control_00000000000000000000000000000081"}
	created, err := app.Runtime.CreateRun(context.Background(), create)
	if err != nil {
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	attempt, err := app.Runtime.BeginStage(context.Background(), domain.BeginStageCommand{RunID: runID, ExpectedRunVersion: created.Version, StageName: "prepare", AttemptID: dockerProbeAttemptID, InputDigest: domain.SumBytes([]byte("docker-probe-input")), IdempotencyKey: "begin_00000000000000000000000000000081", At: now.Add(time.Microsecond)})
	if err != nil {
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	policy := domain.SumBytes([]byte("docker-probe-policy"))
	requestDigest := domain.SumBytes([]byte("docker-probe-request"))
	call, err := ledger.OpenCall(context.Background(), domain.OpenCallRequest{ID: dockerProbeCallRecordID, RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, LogicalOperationID: "integration-docker-probe", Kind: domain.CallSandboxProbe, Provider: "docker", RequestDigest: requestDigest, PolicyDigest: policy, RetryPolicy: domain.RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, JitterSeedDigest: policy}, IdempotencyKey: "open_00000000000000000000000000000081", At: now.Add(2 * time.Microsecond)})
	if err != nil {
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	planDigest := domain.SumBytes([]byte("docker-probe-plan"))
	prepared, err := ledger.PrepareCalls(context.Background(), domain.PrepareCallsRequest{
		RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID,
		CallRecordID: call.ID, PlanDigest: planDigest,
		Calls: []domain.PhysicalCallPlan{{
			ID: dockerProbeAttemptCall, Ordinal: 1, RetryGroup: "docker-probe", RetryOrdinal: 1,
			Kind: domain.PhysicalDockerEnginePing, Provider: "docker", RequestDigest: requestDigest,
			IdempotencyKey: "physical_00000000000000000000000000000081",
		}},
		IdempotencyKey: "prepare_00000000000000000000000000000081", At: now.Add(3 * time.Microsecond),
	})
	if err != nil {
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	if _, err := ledger.BeginDispatch(context.Background(), domain.BeginDispatchRequest{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, CallRecordID: call.ID, AttemptCallID: prepared.PhysicalCalls[0].ID, IdempotencyKey: "dispatch_00000000000000000000000000000081", At: now.Add(4 * time.Microsecond)}); err != nil {
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	dockerConfig, lock, static := loadSlice1DockerCanary(t)
	engine, err := dockersandbox.NewEngineClient(dockerConfig)
	if err != nil {
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	artifacts := probe.NewMemoryArtifactStore()
	watchdog, err := dockersandbox.NewInProcessWatchdogController("slice1-docker-probe-watchdog-token-00000000000000000001", engine)
	if err != nil {
		_ = engine.Close()
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	plan, err := port.NewContainerPlan(static.EngineIdentityDigest, nil, 0)
	if err != nil {
		_ = engine.Close()
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	identity := port.ProbeAuthorizationIdentity{LogicalOperationID: "integration-docker-probe", RunID: runID, AttemptID: attempt.AttemptID, SandboxExecutionID: "sandbox_00000000000000000000000000000081", EngineIdentityDigest: static.EngineIdentityDigest, ScopeDigest: domain.SumBytes([]byte("docker-probe-scope")), PlanDigest: plan.PlanDigest}
	auth, err := port.NewSlice0ProbeAuthorization(identity, plan, durableEnginePingClaims{identity: identity, callID: dockerProbeAttemptCall})
	if err != nil {
		_ = engine.Close()
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	runner, err := dockersandbox.NewRunner(dockersandbox.RunnerOptions{Engine: &gatedDockerEngine{Engine: engine, hold: hold}, Config: dockerConfig, Lock: lock, EngineIdentityDigest: static.EngineIdentityDigest, Blobs: artifacts, Artifacts: artifacts, Watchdog: watchdog, Limits: dockersandbox.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 15 * time.Second}, Lifecycle: app.Runtime.(port.SandboxLifecycleRecorder), CallLedger: ledger, Clock: clock.Real{}})
	if err != nil {
		_ = engine.Close()
		_ = app.Close()
		return "", nil, nil, func() {}, err
	}
	return dockerProbeAttemptCall, auth, runner, func() { _ = engine.Close(); _ = app.Close() }, nil
}

type dockerProbeRecovery struct{ runtime port.RuntimeStore }

func (r dockerProbeRecovery) RecoverRun(ctx context.Context, runID domain.RunID) error {
	ledger, ok := r.runtime.(port.CallLedger)
	if !ok {
		return errors.New("Docker recovery runtime does not expose call ledger")
	}
	current, err := r.runtime.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	prepared, err := ledger.LoadCall(ctx, dockerProbeCallRecordID)
	if err != nil {
		return err
	}
	if len(prepared.PhysicalCalls) != 1 {
		return fmt.Errorf("Docker recovery loaded %d physical calls", len(prepared.PhysicalCalls))
	}
	physical := prepared.PhysicalCalls[0]
	if physical.State == domain.PhysicalDispatching || physical.State == domain.PhysicalSent {
		if err := ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: runID, ExpectedRunVersion: current.Version, StageName: "prepare", AttemptID: prepared.Call.AttemptID, CallRecordID: prepared.Call.ID, AttemptCallID: physical.ID, State: domain.PhysicalUnknown, Outcome: domain.PhysicalOutcomeUnknown, Failure: &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}, IdempotencyKey: "complete_00000000000000000000000000000081", At: time.Now().UTC()}); err != nil {
			return err
		}
	}
	prepared, err = ledger.LoadCall(ctx, dockerProbeCallRecordID)
	if err != nil {
		return err
	}
	if prepared.Call.State != domain.CallRecordTerminal {
		_, err = ledger.FinishCall(ctx, domain.FinishCallRequest{RunID: runID, ExpectedRunVersion: current.Version, StageName: "prepare", AttemptID: prepared.Call.AttemptID, CallRecordID: prepared.Call.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &physical.ID, IdempotencyKey: "finish_00000000000000000000000000000081", At: time.Now().UTC()})
	}
	return err
}

var _ application.RunRecovery = dockerProbeRecovery{}

// TestSlice1DockerWatchdogOwnerHelper keeps a detached watchdog session open.
// The parent kills this owner, causing the watchdog's real control connection
// to receive EOF and run its reconciler before exiting.
func TestSlice1DockerWatchdogOwnerHelper(t *testing.T) {
	if os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_OWNER") != "1" {
		return
	}
	config, _, static := loadSlice1DockerCanary(t)
	marker := os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_MARKER")
	if marker == "" {
		t.Fatal("watchdog marker is required")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	var resourceName string
	controller, err := dockersandbox.NewDetachedWatchdogController(dockersandbox.DetachedWatchdogOptions{
		Config: config, EngineIdentity: static.EngineIdentityDigest, ControlDirectory: filepath.Join(filepath.Dir(marker), "watchdog-control"),
		Executable: executable, ArmTimeout: 20 * time.Second,
		CommandFactory: func(executable, controlPath string) *exec.Cmd {
			command := exec.Command(executable, "-test.run=^TestSlice1DockerWatchdogServiceChild$", "-test.v")
			command.Env = append(os.Environ(), "CPGEN_SLICE1_DOCKER_WATCHDOG_CHILD=1", "CPGEN_SLICE1_DOCKER_WATCHDOG_CONTROL="+controlPath, "CPGEN_SLICE1_DOCKER_WATCHDOG_MARKER="+marker, "CPGEN_SLICE1_DOCKER_WATCHDOG_RESOURCE="+resourceName)
			return command
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := dockersandbox.PlanIdentity{
		RunID: dockerProbeRunID, AttemptID: dockerProbeAttemptID, SandboxExecutionID: "sandbox_00000000000000000000000000000082",
		LogicalOperationID: "slice1-watchdog-eof", OperationNonce: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", EngineIdentityDigest: static.EngineIdentityDigest,
	}
	callOrdinal := 0
	resource := port.PlannedResource{
		Ordinal: 0, Kind: port.ResourceContainer, Role: port.ResourceTarget,
		DeterministicName: "cpgen-s0-bbbbbbbbbbbbbbbbbbbbbbbbbbbb-00-ctr-target", CreateCallOrdinal: &callOrdinal,
	}
	resourceName = resource.DeterministicName
	resource.ExpectedLabelsDigest = watchdogCanaryLabelsDigest(identity, resource)
	plan, err := port.NewContainerPlan(static.EngineIdentityDigest, []port.PlannedResource{resource}, 0)
	if err != nil {
		t.Fatal(err)
	}
	callID := dockerProbeAttemptCall
	labels, err := dockersandbox.ResourceLabels(identity, plan, resource, &callID)
	if err != nil {
		t.Fatal(err)
	}
	record := watchdog.ControlRecord{
		SchemaVersion: watchdog.ControlRecordSchemaVersion, TokenDigest: controller.TokenDigest(), RunID: identity.RunID, AttemptID: identity.AttemptID,
		SandboxExecutionID: identity.SandboxExecutionID, EngineEndpoint: config.EngineEndpoint, EngineIdentityDigest: static.EngineIdentityDigest,
		LogicalOperationID: identity.LogicalOperationID, ScopeDigest: domain.SumBytes([]byte("slice1-watchdog-eof-scope")), Plan: plan,
		SafetyDeadlineUTC: time.Now().Add(20 * time.Second).UTC(), CleanupDeadlineUTC: time.Now().Add(40 * time.Second).UTC(),
	}
	session, err := controller.Arm(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	evidence, ok := session.(dockersandbox.WatchdogSessionEvidence)
	if !ok {
		t.Fatal("detached watchdog omitted control-file evidence")
	}
	engine, err := dockersandbox.NewEngineClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if err := session.PreCreate(context.Background(), resource, labels); err != nil {
		t.Fatal(err)
	}
	created, err := engine.ContainerCreate(context.Background(), moby.ContainerCreateOptions{
		Name:       resource.DeterministicName,
		Config:     &container.Config{Image: config.RuntimeImage, Cmd: []string{"sleep", "300"}, Labels: labels},
		HostConfig: &container.HostConfig{AutoRemove: false},
	})
	if err != nil {
		_ = session.Close()
		t.Fatal(err)
	}
	if created.ID == "" {
		_ = session.Close()
		t.Fatal("Docker watchdog canary returned empty container ID")
	}
	if _, err := engine.ContainerStart(context.Background(), created.ID, moby.ContainerStartOptions{}); err != nil {
		_ = session.Close()
		t.Fatal(err)
	}
	if err := session.ResourceCreated(context.Background(), resource, created.ID); err != nil {
		t.Fatal(err)
	}
	if err := session.TargetPhase(context.Background(), resource, created.ID, 20*time.Second); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(os.Stdout, "READY watchdog %s\n", evidence.ControlRecordRef())
	select {}
}

func TestSlice1DockerWatchdogServiceChild(t *testing.T) {
	if os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_CHILD") != "1" {
		return
	}
	control := os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_CONTROL")
	marker := os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_MARKER")
	err := dockersandbox.RunWatchdogService(context.Background(), control)
	if err == nil {
		if _, statErr := os.Stat(control); !errors.Is(statErr, os.ErrNotExist) {
			err = fmt.Errorf("watchdog control file was not cleaned: %v", statErr)
		} else if _, statErr := os.Stat(filepath.Dir(control)); !errors.Is(statErr, os.ErrNotExist) {
			err = fmt.Errorf("watchdog control directory was not cleaned: %v", statErr)
		}
	}
	value := "ok"
	if resourceName := os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_RESOURCE"); resourceName != "" && err == nil {
		value += " resource=" + resourceName
	}
	if err != nil {
		value = "error: " + err.Error()
	}
	if writeErr := os.WriteFile(marker, []byte(value), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func watchdogCanaryLabelsDigest(identity dockersandbox.PlanIdentity, resource port.PlannedResource) domain.Digest {
	labels := map[string]string{
		"org.cpgen.attempt":            string(identity.AttemptID),
		"org.cpgen.engine-digest":      string(identity.EngineIdentityDigest),
		"org.cpgen.execution-protocol": dockersandbox.ExecutionProtocolDockerDirectV2,
		"org.cpgen.kind":               string(resource.Kind),
		"org.cpgen.logical-operation":  identity.LogicalOperationID,
		"org.cpgen.name":               resource.DeterministicName,
		"org.cpgen.ordinal":            "0",
		"org.cpgen.role":               string(resource.Role),
		"org.cpgen.run":                string(identity.RunID),
		"org.cpgen.slice":              "0",
		"org.cpgen.sandbox-execution":  string(identity.SandboxExecutionID),
	}
	encoded, err := json.Marshal(labels)
	if err != nil {
		panic(err)
	}
	return domain.SumBytes(encoded)
}
