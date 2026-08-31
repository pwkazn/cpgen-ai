//go:build cpgen_slice0_probe

package probe_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"testing"
	"time"

	"cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/probe"
	"cpgen/internal/toolchain"
	watchdogprotocol "cpgen/internal/watchdog"
)

func TestCapabilityProbeAggregatesExclusivePingAndContainerClaims(t *testing.T) {
	store := probe.NewMemoryArtifactStore()
	lock := capabilityLock(t)
	engine := domain.SumBytes([]byte("capability-engine"))
	sandbox := &capabilitySandbox{store: store, lock: lock, engine: engine}
	harness, err := probe.NewSlice0ProbeHarness(probe.Dependencies{
		Runner: sandbox, Lock: lock, Artifacts: store, EngineIdentityDigest: engine,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := harness.Probe(context.Background(), port.ProfileExecuteMVPV2)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Capabilities.Validate(port.ProfileExecuteMVPV2); err != nil {
		t.Fatal(err)
	}
	if err := result.CallTrace.Validate(); err != nil {
		t.Fatal(err)
	}
	if sandbox.pingCalls != 1 || sandbox.compileCalls != 1 || sandbox.runCalls != 4 {
		t.Fatalf("physical canaries ping=%d compile=%d run=%d", sandbox.pingCalls, sandbox.compileCalls, sandbox.runCalls)
	}
	if got := len(result.CallTrace.PhysicalAttemptCallIDs); got != 13 {
		t.Fatalf("aggregate physical calls = %d, want 13", got)
	}
	if !slices.Equal(sandbox.modes, []string{"I", "P", "C", "O"}) {
		t.Fatalf("execute modes = %#v", sandbox.modes)
	}
}

func TestCapabilityProbeFailsClosedOnIncompleteClaimsAndMandatoryCanary(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*capabilitySandbox)
	}{
		{name: "missing claim", mutate: func(s *capabilitySandbox) { s.omitLastCompileClaim = true }},
		{name: "mandatory canary", mutate: func(s *capabilitySandbox) { s.badChildOOM = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := probe.NewMemoryArtifactStore()
			lock := capabilityLock(t)
			engine := domain.SumBytes([]byte("capability-engine"))
			sandbox := &capabilitySandbox{store: store, lock: lock, engine: engine}
			test.mutate(sandbox)
			harness, err := probe.NewSlice0ProbeHarness(probe.Dependencies{Runner: sandbox, Lock: lock, Artifacts: store, EngineIdentityDigest: engine})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := harness.Probe(context.Background(), port.ProfileExecuteMVPV2); err == nil {
				t.Fatal("invalid capability probe succeeded")
			} else {
				var check *docker.CheckError
				if !errors.As(err, &check) || check.Failure.Code != domain.FailureCapabilityMissing || check.Failure.Class != domain.FailureBlocked {
					t.Fatalf("failure = %T %v", err, err)
				}
			}
		})
	}
}

func TestCapabilityProbePreservesDockerUnavailableClassification(t *testing.T) {
	store := probe.NewMemoryArtifactStore()
	lock := capabilityLock(t)
	engine := domain.SumBytes([]byte("capability-engine"))
	sandbox := &capabilitySandbox{store: store, lock: lock, engine: engine, unavailable: true}
	harness, err := probe.NewSlice0ProbeHarness(probe.Dependencies{Runner: sandbox, Lock: lock, Artifacts: store, EngineIdentityDigest: engine})
	if err != nil {
		t.Fatal(err)
	}
	result, err := harness.Probe(context.Background(), port.ProfileCompileV2)
	if err == nil {
		t.Fatal("unavailable Engine probe succeeded")
	}
	var check *docker.CheckError
	if !errors.As(err, &check) || check.Failure.Code != domain.FailureUnavailable || check.Failure.Class != domain.FailureBlocked {
		t.Fatalf("failure = %T %v", err, err)
	}
	if err := result.CallTrace.Validate(); err != nil || len(result.CallTrace.PhysicalAttemptCallIDs) != 1 {
		t.Fatalf("dispatched ping trace = %#v, %v", result.CallTrace, err)
	}
	if sandbox.compileCalls != 0 || sandbox.runCalls != 0 {
		t.Fatal("container canary dispatched after unavailable ping")
	}
}

func TestCapabilityProbeUnusedLocalEndpointReturnsUnavailable(t *testing.T) {
	lock := capabilityLock(t)
	endpoint := "unix:///tmp/cpgen-definitely-unused-docker.sock"
	if runtime.GOOS == "windows" {
		endpoint = "npipe:////./pipe/cpgen-definitely-unused-docker"
	}
	config := docker.Config{
		EngineEndpoint: endpoint, APIVersion: docker.RequiredAPIVersion,
		BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID),
		ExecutionProtocol: docker.ExecutionProtocolDockerDirectV2,
	}
	engine, err := docker.NewEngineClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	engineIdentity := domain.SumBytes([]byte("unused-local-engine"))
	store := probe.NewMemoryArtifactStore()
	runner, err := docker.NewRunner(docker.RunnerOptions{
		Engine: engine, Config: config, Lock: lock, EngineIdentityDigest: engineIdentity,
		Blobs: store, Artifacts: store, Watchdog: unusedWatchdog{},
		Limits: docker.ControlLimits{HelperMemoryBytes: 64 << 20, HelperPIDs: 8, MaxTransferBytes: 16 << 20, CleanupTimeout: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	harness, err := probe.NewSlice0ProbeHarness(probe.Dependencies{Runner: runner, Lock: lock, Artifacts: store, EngineIdentityDigest: engineIdentity})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := harness.Probe(ctx, port.ProfileCompileV2)
	var check *docker.CheckError
	if !errors.As(err, &check) || check.Failure.Code != domain.FailureUnavailable || check.Failure.Class != domain.FailureBlocked {
		t.Fatalf("unused endpoint failure = %T %v", err, err)
	}
	if err := result.CallTrace.Validate(); err != nil || len(result.CallTrace.PhysicalAttemptCallIDs) != 1 {
		t.Fatalf("unused endpoint trace = %#v, %v", result.CallTrace, err)
	}
}

type capabilitySandbox struct {
	store                *probe.MemoryArtifactStore
	lock                 toolchain.Lock
	engine               domain.Digest
	pingCalls            int
	compileCalls         int
	runCalls             int
	modes                []string
	omitLastCompileClaim bool
	badChildOOM          bool
	unavailable          bool
}

func (s *capabilitySandbox) Probe(ctx context.Context, auth port.SandboxDispatchAuthorization, request port.DockerProbeRequest) (port.DockerProbeResult, error) {
	s.pingCalls++
	grant, err := auth.ClaimEnginePing(ctx)
	if err != nil {
		return port.DockerProbeResult{}, err
	}
	call := grant.CallID()
	result := port.DockerProbeResult{CallTrace: dispatchedTrace(auth.LogicalOperationID(), []domain.AttemptCallID{call}, call)}
	if s.unavailable {
		return result, &docker.CheckError{Failure: domain.PortFailure{Code: domain.FailureUnavailable, Class: domain.FailureBlocked}, Cause: errors.New("simulated unavailable Engine")}
	}
	result.Capabilities = port.CapabilitySnapshot{
		Profile: port.SandboxProfile(request.Profile), EngineIdentityDigest: s.engine,
		EndpointDigest: domain.SumBytes([]byte("endpoint")), ServerOS: "linux", APIVersion: docker.RequiredAPIVersion,
		CgroupVersion: 2, Flags: map[string]bool{}, BuilderImageDigest: s.lock.Builder.ImageID,
		RuntimeImageDigest: s.lock.Runtime.ImageID, TransferImageDigest: s.lock.Transfer.ImageID,
		ExecutionProtocol: docker.ExecutionProtocolDockerDirectV2,
	}
	return result, nil
}

func (s *capabilitySandbox) Compile(ctx context.Context, auth port.SandboxDispatchAuthorization, _ port.CompileRequest) (port.CompileResult, error) {
	s.compileCalls++
	calls, roles, err := claimPlan(ctx, auth, s.omitLastCompileClaim)
	if err != nil {
		return port.CompileResult{}, err
	}
	if len(calls) == 0 {
		return port.CompileResult{}, errors.New("compile plan had no calls")
	}
	target, export := roles[port.ContainerTarget], roles[port.ContainerExport]
	program := pendingArtifact(s.store.PutBlob([]byte("fake-capability-program")), domain.ArtifactProgram, "program/main", export)
	return port.CompileResult{CallTrace: dispatchedTrace(auth.LogicalOperationID(), calls, target), Outcome: domain.CompileOK, Program: &program}, nil
}

func (s *capabilitySandbox) Run(ctx context.Context, auth port.SandboxDispatchAuthorization, request port.RunRequest) (port.RunResult, error) {
	s.runCalls++
	calls, roles, err := claimPlan(ctx, auth, false)
	if err != nil {
		return port.RunResult{}, err
	}
	modeBytes, err := s.store.ReadBlob(*request.Stdin)
	if err != nil {
		return port.RunResult{}, err
	}
	mode := string(modeBytes)
	s.modes = append(s.modes, mode)
	outcome := domain.ProcessExited
	exit := 0
	var stdout *domain.PendingArtifact
	switch mode {
	case "I":
		artifact := pendingArtifact(s.store.PutBlob([]byte("cpgen-capability-ok\n")), domain.ArtifactStdout, "process/stdout", roles[port.ContainerTarget])
		stdout = &artifact
	case "P":
		outcome = domain.ProcessMLE
	case "C":
		if s.badChildOOM {
			outcome = domain.ProcessExited
		} else {
			outcome = domain.ProcessMLE
		}
	case "O":
		outcome = domain.ProcessOLE
	default:
		return port.RunResult{}, errors.New("unexpected capability mode")
	}
	result := port.RunResult{
		CallTrace: dispatchedTrace(auth.LogicalOperationID(), calls, roles[port.ContainerTarget]),
		Outcome:   outcome, Metrics: port.ProcessMetrics{WallTime: time.Millisecond}, Stdout: stdout,
	}
	if outcome == domain.ProcessExited {
		result.ExitCode = &exit
	}
	return result, nil
}

func claimPlan(ctx context.Context, auth port.SandboxDispatchAuthorization, omitLast bool) ([]domain.AttemptCallID, map[port.ContainerRole]domain.AttemptCallID, error) {
	plan := auth.ContainerPlan()
	containerCount := 0
	for _, resource := range plan.Resources {
		if resource.Kind == port.ResourceContainer {
			containerCount++
		}
	}
	if omitLast {
		containerCount--
	}
	calls := make([]domain.AttemptCallID, 0, containerCount)
	roles := map[port.ContainerRole]domain.AttemptCallID{}
	for _, resource := range plan.Resources {
		if resource.Kind != port.ResourceContainer || len(calls) == containerCount {
			continue
		}
		role := port.ContainerRole(resource.Role)
		grant, err := auth.ClaimNextContainer(ctx, role)
		if err != nil {
			return nil, nil, err
		}
		calls = append(calls, grant.Auth.CallID())
		roles[role] = grant.Auth.CallID()
	}
	return calls, roles, nil
}

func dispatchedTrace(logical string, calls []domain.AttemptCallID, result domain.AttemptCallID) domain.CallTrace {
	return domain.CallTrace{LogicalOperationID: logical, DispatchKind: domain.DispatchDispatched, PhysicalAttemptCallIDs: slices.Clone(calls), ResultAttemptCallID: &result}
}

func pendingArtifact(blob domain.BlobRef, role domain.ArtifactRole, path domain.SafeRelPath, call domain.AttemptCallID) domain.PendingArtifact {
	return domain.PendingArtifact{
		Blob: blob, MediaType: "application/octet-stream", Role: role, LogicalPath: path, CallID: call,
		ReservationID: "res_00000000000000000000000000000001", WriterTokenID: "writer_00000000000000000000000000000001",
		PinID: "pin_00000000000000000000000000000001", PhysicalNewBytes: blob.Size,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: docker.ExecutionProtocolDockerDirectV2},
	}
}

func capabilityLock(t *testing.T) toolchain.Lock {
	t.Helper()
	lock, err := toolchain.NewDockerV1Lock(domain.SumBytes([]byte("builder")), domain.SumBytes([]byte("runtime")), domain.SumBytes([]byte("transfer")))
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

var _ port.DockerSandbox = (*capabilitySandbox)(nil)

type unusedWatchdog struct{}

func (unusedWatchdog) TokenDigest() domain.Digest { return domain.SumBytes([]byte("unused-watchdog")) }
func (unusedWatchdog) Arm(context.Context, watchdogprotocol.ControlRecord) (docker.WatchdogSession, error) {
	return nil, fmt.Errorf("unused endpoint watchdog must not arm")
}

var _ docker.WatchdogController = unusedWatchdog{}
