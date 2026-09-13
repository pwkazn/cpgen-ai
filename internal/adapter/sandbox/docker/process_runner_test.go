//go:build cpgen_slice0_probe

package docker_test

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestRunnerCapturesBoundedAttachAndHostExecutionEvidence(t *testing.T) {
	fixture := newRunnerFixture(t)
	fixture.engine.targetStdout = []byte("compiler stdout")
	fixture.engine.targetStderr = []byte("compiler stderr")
	result, err := fixture.runner.Compile(context.Background(), fixture.auth, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	if result.Stdout == nil || result.Stdout.Blob.Digest != domain.SumBytes(fixture.engine.targetStdout) || result.Stdout.CallID != "call_00000000000000000000000000000099" {
		t.Fatalf("stdout artifact = %#v", result.Stdout)
	}
	if result.Stderr == nil || result.Stderr.Blob.Digest != domain.SumBytes(fixture.engine.targetStderr) || result.Stderr.CallID != "call_00000000000000000000000000000099" {
		t.Fatalf("stderr artifact = %#v", result.Stderr)
	}
	if result.Execution == nil || result.Execution.Role != domain.ArtifactEvidence || result.Execution.CallID != "call_00000000000000000000000000000099" {
		t.Fatalf("execution artifact = %#v", result.Execution)
	}
	if result.Details["process_outcome"] != string(domain.ProcessExited) {
		t.Fatalf("compile details = %#v", result.Details)
	}
}

func TestRunnerOutcomePriorityAndLimitPlusOneStop(t *testing.T) {
	fixture := newRunnerFixture(t)
	fixture.engine.targetStdout = bytes.Repeat([]byte("x"), (1<<20)+8192)
	fixture.engine.targetOOMKilled = true
	result, err := fixture.runner.Compile(context.Background(), fixture.auth, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.CompileInfraError || result.Details["process_outcome"] != string(domain.ProcessMLE) {
		t.Fatalf("compile result = %#v", result)
	}
	if result.Stdout == nil || result.Stdout.Blob.Size != 1<<20 {
		t.Fatalf("bounded stdout = %#v", result.Stdout)
	}
	if result.Program != nil || len(result.CallTrace.PhysicalAttemptCallIDs) != 3 {
		t.Fatalf("OLE/MLE execution dispatched export: %#v", result)
	}
}

func TestRunnerOLEStopsAtPerStreamLimitPlusOne(t *testing.T) {
	fixture := newRunnerFixture(t)
	fixture.engine.targetStdout = bytes.Repeat([]byte("x"), (1<<20)+1)
	fixture.engine.targetWaitDelay = 30 * time.Millisecond
	result, err := fixture.runner.Compile(context.Background(), fixture.auth, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Details["process_outcome"] != string(domain.ProcessOLE) || result.Stdout == nil || result.Stdout.Blob.Size != 1<<20 {
		t.Fatalf("OLE result = %#v", result)
	}
	events := fixture.events.snapshot()
	if indexOf(events, "stop:TARGET") < 0 || indexOf(events, "kill:TARGET") < 0 {
		t.Fatalf("OLE did not trigger portable stop: %#v", events)
	}
}

func TestRunnerAcceptsExactTargetOOMEventAsMLEEvidence(t *testing.T) {
	fixture := newRunnerFixture(t)
	fixture.engine.targetOOMEvent = true
	result, err := fixture.runner.Compile(context.Background(), fixture.auth, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Details["process_outcome"] != string(domain.ProcessMLE) {
		t.Fatalf("OOM event result = %#v", result)
	}
}

func TestRunnerHardDeadlineUsesPortableStopProof(t *testing.T) {
	fixture := newRunnerFixture(t)
	fixture.request.Limits.Time = 5 * time.Millisecond
	fixture.engine.targetWaitDelay = 30 * time.Millisecond
	result, err := fixture.runner.Compile(context.Background(), fixture.auth, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.CompileInfraError || result.Details["process_outcome"] != string(domain.ProcessTLE) {
		t.Fatalf("deadline result = %#v", result)
	}
	events := fixture.events.snapshot()
	if indexOf(events, "stop:TARGET") < 0 || indexOf(events, "kill:TARGET") < 0 {
		t.Fatalf("portable stop was not used: %#v", events)
	}
}

func TestRunnerRunImportsExecutableAcceptsBoundedStdinAndReturnsRawExit(t *testing.T) {
	fixture := newRunFixture(t)
	result, err := fixture.runner.Run(context.Background(), fixture.auth, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.ProcessExited || result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("run result = %#v", result)
	}
	if !slices.Equal(fixture.engine.stdinBytes(), fixture.stdin) {
		t.Fatalf("stdin = %q, want %q", fixture.engine.stdinBytes(), fixture.stdin)
	}
	if result.Stdout == nil || result.Stdout.Blob.Digest != domain.SumBytes([]byte("run stdout")) || result.Execution == nil {
		t.Fatalf("run evidence = %#v", result)
	}
}

func TestWatchdogRunnerUsesProtocolACKsForEveryCreateAndTargetPhase(t *testing.T) {
	fixture := newRunnerFixture(t)
	lock := toolchainLock(t)
	identity := planIdentity()
	controller, err := docker.NewInProcessWatchdogController("runner-watchdog-token-0000000000000000000000001", fixture.engine)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := docker.NewRunner(docker.RunnerOptions{
		Engine: fixture.engine,
		Config: docker.Config{
			EngineEndpoint: "npipe:////./pipe/docker_engine", APIVersion: docker.RequiredAPIVersion,
			BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID),
			ExecutionProtocol: docker.ExecutionProtocolDockerDirectV2,
		},
		Lock: lock, EngineIdentityDigest: identity.EngineIdentityDigest,
		Blobs: fixture.blobs, Artifacts: &recordingArtifactSink{events: fixture.events}, Watchdog: controller,
		Lifecycle: newRecordingLifecycle(), CallLedger: recordingCallLedger{},
		Limits: docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Compile(context.Background(), fixture.auth, fixture.request)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != domain.CompileOK || fixture.engine.resourceCount() != 0 {
		t.Fatalf("watchdog-backed compile = %#v resources=%d", result, fixture.engine.resourceCount())
	}
}

func TestWatchdogCleanupAcknowledgesStoppedTargetAfterStartFailure(t *testing.T) {
	fixture := newRunnerFixture(t)
	fixture.engine.failStartRole = port.ContainerTarget
	if _, err := fixture.runner.Compile(context.Background(), fixture.auth, fixture.request); err == nil {
		t.Fatal("target Start failure was not reported")
	}
	events := fixture.events.snapshot()
	stopped := indexOf(events, "watchdog-stopped:TARGET")
	cleaned := indexOf(events, "watchdog-cleaned")
	if stopped < 0 || cleaned < 0 || stopped >= cleaned {
		t.Fatalf("cleanup watchdog acknowledgements are out of order: %#v", events)
	}
}

type runFixture struct {
	runner  *docker.Runner
	auth    port.SandboxDispatchAuthorization
	request port.RunRequest
	engine  *recordingDockerEngine
	stdin   []byte
}

func newRunFixture(t *testing.T) runFixture {
	t.Helper()
	events := &eventLog{}
	lock := toolchainLock(t)
	identity := planIdentity()
	identity.LogicalOperationID = "run-solution"
	program := []byte("fake executable")
	stdin := []byte("bounded stdin")
	stdinRef := domain.BlobRef{Digest: domain.SumBytes(stdin), Size: int64(len(stdin))}
	request := port.RunRequest{
		Role:    port.RoleSolution,
		Program: domain.BlobRef{Digest: domain.SumBytes(program), Size: int64(len(program))},
		Stdin:   &stdinRef,
		Limits:  port.RunLimits{Time: time.Second, MemoryBytes: 128 << 20, PIDs: 16, StdoutBytes: 1024, StderrBytes: 1024},
	}
	plan, err := docker.BuildRunPlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	calls := []domain.AttemptCallID{"call_00000000000000000000000000000031", "call_00000000000000000000000000000032"}
	probeIdentity := port.ProbeAuthorizationIdentity{
		LogicalOperationID: identity.LogicalOperationID, RunID: identity.RunID, AttemptID: identity.AttemptID,
		SandboxExecutionID: identity.SandboxExecutionID,
		ScopeDigest:        domain.SumBytes([]byte("run-scope")), PlanDigest: plan.PlanDigest, EngineIdentityDigest: identity.EngineIdentityDigest,
	}
	claims := newRecordingClaims(events, probeIdentity, calls)
	auth, err := port.NewSlice0ProbeAuthorization(probeIdentity, plan, claims)
	if err != nil {
		t.Fatal(err)
	}
	engine := newRecordingDockerEngine(events)
	engine.targetStdout = []byte("run stdout")
	engine.targetExitCode = 7
	engine.expectedStdin = stdin
	runner, err := docker.NewRunner(docker.RunnerOptions{
		Engine: engine,
		Config: docker.Config{
			EngineEndpoint: "npipe:////./pipe/docker_engine", APIVersion: docker.RequiredAPIVersion,
			BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID),
			ExecutionProtocol: docker.ExecutionProtocolDockerDirectV2,
		},
		Lock: lock, EngineIdentityDigest: identity.EngineIdentityDigest,
		Blobs:     &recordingBlobs{events: events, data: map[domain.Digest][]byte{request.Program.Digest: program, stdinRef.Digest: stdin}},
		Artifacts: &recordingArtifactSink{events: events}, Watchdog: &recordingWatchdog{events: events},
		Lifecycle: newRecordingLifecycle(), CallLedger: recordingCallLedger{},
		Limits: docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 5 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	return runFixture{runner: runner, auth: auth, request: request, engine: engine, stdin: stdin}
}
