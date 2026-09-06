//go:build cpgen_slice0_probe

package probe

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

type Dependencies struct {
	Runner               port.DockerSandbox
	Lock                 toolchain.Lock
	Artifacts            *MemoryArtifactStore
	EngineIdentityDigest domain.Digest
}

type Harness struct {
	mu        sync.Mutex
	runner    port.DockerSandbox
	lock      toolchain.Lock
	artifacts *MemoryArtifactStore
	engine    domain.Digest
	runID     domain.RunID
	attemptID domain.AttemptID
	scope     domain.Digest
	ordinal   uint64
}

func NewSlice0ProbeHarness(dependencies Dependencies) (*Harness, error) {
	if dependencies.Runner == nil || dependencies.Artifacts == nil {
		return nil, fmt.Errorf("Slice 0 probe Runner and artifact store are required")
	}
	if err := dependencies.Lock.Validate(); err != nil {
		return nil, err
	}
	if err := dependencies.EngineIdentityDigest.Validate(); err != nil {
		return nil, err
	}
	runID, err := domain.NewID("run")
	if err != nil {
		return nil, err
	}
	attemptID, err := domain.NewID("attempt")
	if err != nil {
		return nil, err
	}
	return &Harness{
		runner: dependencies.Runner, lock: dependencies.Lock, artifacts: dependencies.Artifacts,
		engine: dependencies.EngineIdentityDigest, runID: domain.RunID(runID), attemptID: domain.AttemptID(attemptID),
		scope: domain.SumBytes([]byte(runID + ":slice0-probe")),
	}, nil
}

func (h *Harness) Compile(ctx context.Context, request port.CompileRequest) (port.CompileResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.compile(ctx, h.nextLogical("compile"), request)
}

func (h *Harness) Run(ctx context.Context, request port.RunRequest) (port.RunResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.run(ctx, h.nextLogical("run"), request)
}

func (h *Harness) Probe(ctx context.Context, profile port.SandboxProfile) (port.DockerProbeResult, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !profile.Valid() {
		return port.DockerProbeResult{}, fmt.Errorf("invalid capability profile %q", profile)
	}
	logical := h.nextLogical("capability")
	ping, err := h.enginePing(ctx, logical, profile)
	if err != nil {
		return ping, err
	}
	aggregate := ping.CallTrace
	if profile == port.ProfileExecuteReleaseV2 {
		ping.Capabilities.Profile = profile
		ping.Capabilities.Flags = map[string]bool{}
		return ping, &docker.CheckError{
			Failure: domain.PortFailure{Code: domain.FailureCapabilityMissing, Class: domain.FailureIncompatible},
			Cause:   fmt.Errorf("execute-release-v2 requires a separately verified rootful Linux cgroup parent implementation; Docker Desktop is not allowlisted"),
		}
	}

	compileRequest, err := h.capabilityCompileRequest()
	if err != nil {
		return ping, capabilityFailure(err)
	}
	compiled, err := h.compile(ctx, logical, compileRequest)
	if traceErr := appendTrace(&aggregate, compiled.CallTrace); traceErr != nil && err == nil {
		err = traceErr
	}
	if err != nil {
		ping.CallTrace = aggregate
		return ping, capabilityFailure(err)
	}
	if compiled.Outcome != domain.CompileOK || compiled.Program == nil {
		ping.CallTrace = aggregate
		return ping, capabilityFailure(fmt.Errorf("capability compile outcome is %q", compiled.Outcome))
	}

	flags := port.RequiredCapabilityFlags(profile)
	if profile != port.ProfileCompileV2 {
		for _, canary := range []struct {
			mode string
			want domain.ProcessOutcome
		}{
			{mode: "I", want: domain.ProcessExited},
			{mode: "P", want: domain.ProcessMLE},
			{mode: "C", want: domain.ProcessMLE},
			{mode: "O", want: domain.ProcessOLE},
		} {
			runRequest := h.capabilityRunRequest(compiled.Program.Blob, canary.mode)
			runResult, runErr := h.run(ctx, logical, runRequest)
			if traceErr := appendTrace(&aggregate, runResult.CallTrace); traceErr != nil && runErr == nil {
				runErr = traceErr
			}
			if runErr != nil {
				ping.CallTrace = aggregate
				return ping, capabilityFailure(runErr)
			}
			if runResult.Outcome != canary.want {
				ping.CallTrace = aggregate
				return ping, capabilityFailure(fmt.Errorf("capability mode %s outcome is %q, want %q", canary.mode, runResult.Outcome, canary.want))
			}
			if canary.mode == "I" {
				if runResult.ExitCode == nil || *runResult.ExitCode != 0 || runResult.Stdout == nil {
					ping.CallTrace = aggregate
					return ping, capabilityFailure(fmt.Errorf("introspection canary omitted successful stdout evidence"))
				}
				stdout, readErr := h.artifacts.ReadBlob(runResult.Stdout.Blob)
				if readErr != nil || string(stdout) != "cpgen-capability-ok\n" {
					ping.CallTrace = aggregate
					return ping, capabilityFailure(errors.Join(readErr, fmt.Errorf("introspection canary evidence mismatch")))
				}
			}
		}
	}

	ping.Capabilities.Profile = profile
	ping.Capabilities.Flags = flags
	ping.CallTrace = aggregate
	if err := ping.Capabilities.Validate(profile); err != nil {
		return ping, capabilityFailure(err)
	}
	if err := ping.CallTrace.Validate(); err != nil {
		return ping, capabilityFailure(err)
	}
	return ping, nil
}

func (h *Harness) enginePing(ctx context.Context, logical string, profile port.SandboxProfile) (port.DockerProbeResult, error) {
	plan, err := port.NewContainerPlan(h.engine, nil, 0)
	if err != nil {
		return port.DockerProbeResult{}, err
	}
	identity := h.probeIdentity(logical, plan.PlanDigest)
	callID, err := newAttemptCallID()
	if err != nil {
		return port.DockerProbeResult{}, err
	}
	ledger, err := NewEnginePingLedger(identity, plan, callID)
	if err != nil {
		return port.DockerProbeResult{}, err
	}
	auth, err := port.NewSlice0ProbeAuthorization(identity, plan, ledger)
	if err != nil {
		return port.DockerProbeResult{}, err
	}
	result, probeErr := h.runner.Probe(ctx, auth, port.DockerProbeRequest{Profile: string(profile)})
	if err := validatePingDispatch(ledger.Snapshot(), result.CallTrace, logical, callID); err != nil {
		if probeErr == nil {
			probeErr = err
		} else {
			probeErr = errors.Join(probeErr, err)
		}
	}
	if probeErr == nil && result.Capabilities.EngineIdentityDigest != h.engine {
		probeErr = fmt.Errorf("probe Engine identity does not match the Harness")
	}
	return result, probeErr
}

func (h *Harness) compile(ctx context.Context, logical string, request port.CompileRequest) (port.CompileResult, error) {
	identity, err := h.planIdentity(logical)
	if err != nil {
		return port.CompileResult{}, err
	}
	plan, err := docker.BuildCompilePlan(request, h.lock, identity)
	if err != nil {
		return port.CompileResult{}, err
	}
	auth, ledger, calls, err := h.containerAuthorization(logical, plan)
	if err != nil {
		return port.CompileResult{}, err
	}
	if err := h.artifacts.configureCalls(plan, calls); err != nil {
		return port.CompileResult{}, err
	}
	result, runErr := h.runner.Compile(ctx, auth, request)
	if validateErr := validateContainerDispatch(ledger.Snapshot(), result.CallTrace, logical, calls); validateErr != nil {
		runErr = errors.Join(runErr, validateErr)
	}
	if runErr == nil {
		runErr = result.Validate()
	}
	return result, runErr
}

func (h *Harness) run(ctx context.Context, logical string, request port.RunRequest) (port.RunResult, error) {
	identity, err := h.planIdentity(logical)
	if err != nil {
		return port.RunResult{}, err
	}
	plan, err := docker.BuildRunPlan(request, h.lock, identity)
	if err != nil {
		return port.RunResult{}, err
	}
	auth, ledger, calls, err := h.containerAuthorization(logical, plan)
	if err != nil {
		return port.RunResult{}, err
	}
	if err := h.artifacts.configureCalls(plan, calls); err != nil {
		return port.RunResult{}, err
	}
	result, runErr := h.runner.Run(ctx, auth, request)
	if validateErr := validateContainerDispatch(ledger.Snapshot(), result.CallTrace, logical, calls); validateErr != nil {
		runErr = errors.Join(runErr, validateErr)
	}
	if runErr == nil {
		runErr = result.Validate()
	}
	return result, runErr
}

func (h *Harness) containerAuthorization(logical string, plan port.ContainerPlan) (port.SandboxDispatchAuthorization, *Ledger, []domain.AttemptCallID, error) {
	count := 0
	for _, resource := range plan.Resources {
		if resource.Kind == port.ResourceContainer {
			count++
		}
	}
	calls := make([]domain.AttemptCallID, count)
	for index := range calls {
		callID, err := newAttemptCallID()
		if err != nil {
			return nil, nil, nil, err
		}
		calls[index] = callID
	}
	identity := h.probeIdentity(logical, plan.PlanDigest)
	ledger, err := NewContainerLedger(identity, plan, calls)
	if err != nil {
		return nil, nil, nil, err
	}
	auth, err := port.NewSlice0ProbeAuthorization(identity, plan, ledger)
	return auth, ledger, calls, err
}

func (h *Harness) planIdentity(logical string) (docker.PlanIdentity, error) {
	suffix := stableLogicalSuffix(h.runID, h.attemptID, logical)
	sandboxID := domain.SandboxExecutionID("sandbox_" + suffix)
	return docker.PlanIdentity{
		RunID: h.runID, AttemptID: h.attemptID, SandboxExecutionID: sandboxID, LogicalOperationID: logical,
		OperationNonce: suffix, EngineIdentityDigest: h.engine,
	}, nil
}

func (h *Harness) probeIdentity(logical string, plan domain.Digest) port.ProbeAuthorizationIdentity {
	return port.ProbeAuthorizationIdentity{
		LogicalOperationID: logical, RunID: h.runID, AttemptID: h.attemptID, SandboxExecutionID: domain.SandboxExecutionID("sandbox_" + stableLogicalSuffix(h.runID, h.attemptID, logical)),
		EngineIdentityDigest: h.engine, ScopeDigest: h.scope, PlanDigest: plan,
	}
}

func stableLogicalSuffix(runID domain.RunID, attemptID domain.AttemptID, logical string) string {
	value := string(domain.SumBytes([]byte(string(runID) + "\x00" + string(attemptID) + "\x00" + logical)))
	value = strings.TrimPrefix(value, "sha256:")
	return value[:32]
}

func (h *Harness) nextLogical(kind string) string {
	h.ordinal++
	return fmt.Sprintf("slice0-probe-%s-%06d", kind, h.ordinal)
}

func (h *Harness) capabilityCompileRequest() (port.CompileRequest, error) {
	source := h.artifacts.PutBlob([]byte(capabilitySource))
	bundle := port.SourceBundleManifest{
		SchemaVersion: domain.DomainSchemaVersion,
		Files:         []port.SourceFile{{Path: "main.cpp", Blob: source}},
		EntryPoint:    "main.cpp",
	}
	digest, err := port.ComputeSourceBundleDigest(bundle)
	if err != nil {
		return port.CompileRequest{}, err
	}
	bundle.Digest = digest
	return port.CompileRequest{
		Language: port.LanguageCPP20, Role: port.RoleSolution, SourceBundle: bundle,
		Toolchain:      toolchain.CPP20ToolchainID,
		Limits:         port.CompileLimits{Time: 45 * time.Second, MemoryBytes: 512 << 20, PIDs: 64, OutputBytes: 8 << 20},
		ExpectedOutput: "result/files/main",
	}, nil
}

func (h *Harness) capabilityRunRequest(program domain.BlobRef, mode string) port.RunRequest {
	stdin := h.artifacts.PutBlob([]byte(mode))
	limits := port.RunLimits{Time: 8 * time.Second, MemoryBytes: 64 << 20, PIDs: 16, StdoutBytes: 8 << 10, StderrBytes: 8 << 10}
	if mode == "P" || mode == "C" {
		limits.MemoryBytes = 32 << 20
	}
	if mode == "O" {
		limits.StdoutBytes = 1024
	}
	return port.RunRequest{Role: port.RoleSolution, Program: program, Stdin: &stdin, Limits: limits}
}

func validatePingDispatch(snapshot LedgerSnapshot, trace domain.CallTrace, logical string, callID domain.AttemptCallID) error {
	if snapshot.EnginePing == nil || len(snapshot.Containers) != 0 || snapshot.EnginePing.CallID != callID || snapshot.EnginePing.State != ClaimDispatching {
		return fmt.Errorf("Engine ping did not consume exactly one exclusive claim")
	}
	if err := trace.Validate(); err != nil {
		return err
	}
	if trace.LogicalOperationID != logical || !slices.Equal(trace.PhysicalAttemptCallIDs, []domain.AttemptCallID{callID}) || trace.ResultAttemptCallID == nil || *trace.ResultAttemptCallID != callID {
		return fmt.Errorf("Engine ping CallTrace does not match its claim")
	}
	return nil
}

func validateContainerDispatch(snapshot LedgerSnapshot, trace domain.CallTrace, logical string, calls []domain.AttemptCallID) error {
	if snapshot.EnginePing != nil || len(snapshot.Containers) != len(calls) {
		return fmt.Errorf("container canary used an invalid claim ledger")
	}
	for index, claim := range snapshot.Containers {
		if claim.Ordinal != index || claim.CallID != calls[index] || claim.State != ClaimDispatching {
			return fmt.Errorf("container claim %d is incomplete or out of order", index)
		}
	}
	if err := trace.Validate(); err != nil {
		return err
	}
	if trace.LogicalOperationID != logical || !slices.Equal(trace.PhysicalAttemptCallIDs, calls) {
		return fmt.Errorf("container canary CallTrace is incomplete or out of order")
	}
	return nil
}

func appendTrace(aggregate *domain.CallTrace, next domain.CallTrace) error {
	if aggregate == nil {
		return fmt.Errorf("aggregate CallTrace is required")
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if aggregate.LogicalOperationID != next.LogicalOperationID || aggregate.DispatchKind != domain.DispatchDispatched || next.DispatchKind != domain.DispatchDispatched {
		return fmt.Errorf("capability CallTrace logical operation mismatch")
	}
	aggregate.PhysicalAttemptCallIDs = append(aggregate.PhysicalAttemptCallIDs, next.PhysicalAttemptCallIDs...)
	aggregate.ResultAttemptCallID = next.ResultAttemptCallID
	return aggregate.Validate()
}

func newAttemptCallID() (domain.AttemptCallID, error) {
	value, err := domain.NewID("call")
	return domain.AttemptCallID(value), err
}

func capabilityFailure(cause error) error {
	if cause == nil {
		cause = fmt.Errorf("mandatory Docker capability canary failed")
	}
	var check *docker.CheckError
	if errors.As(cause, &check) {
		return cause
	}
	return &docker.CheckError{Failure: domain.PortFailure{Code: domain.FailureCapabilityMissing, Class: domain.FailureBlocked}, Cause: cause}
}

const capabilitySource = `#include <cerrno>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <fstream>
#include <iostream>
#include <string>
#include <sys/types.h>
#include <sys/wait.h>
#include <unistd.h>
#include <vector>

static std::string read_file(const char* path) {
    std::ifstream input(path);
    std::string value;
    std::getline(input, value);
    return value;
}

[[noreturn]] static void exhaust_memory() {
    std::vector<void*> pages;
    for (;;) {
        void* page = std::malloc(1 << 20);
        if (page == nullptr) _exit(91);
        std::memset(page, 0x5a, 1 << 20);
        pages.push_back(page);
    }
}

int main() {
    char mode = 0;
    if (!std::cin.get(mode)) return 2;
    if (mode == 'P') exhaust_memory();
    if (mode == 'C') {
        pid_t child = fork();
        if (child < 0) return 31;
        if (child == 0) exhaust_memory();
        int status = 0;
        if (waitpid(child, &status, 0) != child) return 32;
        return 0;
    }
    if (mode == 'O') {
        std::string block(4096, 'x');
        for (;;) std::cout << block;
    }
    if (mode != 'I') return 3;
    if (getpid() != 1 || getuid() != 65532 || getgid() != 65532) return 41;
    if (read_file("/sys/fs/cgroup/memory.max") != "67108864") return 42;
    if (read_file("/sys/fs/cgroup/memory.swap.max") != "0") return 43;
    if (read_file("/sys/fs/cgroup/pids.max") != "16") return 44;
    if (std::getenv("DOCKER_HOST") != nullptr || access("/var/run/docker.sock", F_OK) == 0) return 45;
    int fd = open("/cpgen-rootfs-write", O_CREAT | O_WRONLY, 0600);
    if (fd >= 0) return 46;
    std::ifstream status("/proc/self/status");
    std::string line;
    bool cap_zero = false, nnp = false;
    while (std::getline(status, line)) {
        if (line == "CapEff:\t0000000000000000") cap_zero = true;
        if (line == "NoNewPrivs:\t1") nnp = true;
    }
    if (!cap_zero || !nnp) return 47;
    std::cout << "cpgen-capability-ok\n";
    return 0;
}
`
