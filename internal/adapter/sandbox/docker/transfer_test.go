//go:build cpgen_slice0_probe

package docker_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"runtime"
	"slices"
	"testing"
	"time"

	"cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

func TestTransferHelpersAndOutputVolumeUseClosedSpecifications(t *testing.T) {
	fixture := newRunnerFixture(t)
	if _, err := fixture.runner.Compile(context.Background(), fixture.auth, fixture.request); err != nil {
		t.Fatal(err)
	}

	helpers := make(map[port.ContainerRole][]string)
	for _, options := range fixture.engine.creationHistory() {
		if options.Config.Labels["org.cpgen.image-role"] != "transfer" {
			continue
		}
		role := port.ContainerRole(options.Config.Labels["org.cpgen.role"])
		helpers[role] = append(helpers[role], options.Config.User)
		if options.HostConfig == nil || !options.HostConfig.ReadonlyRootfs || !options.HostConfig.NetworkMode.IsNone() ||
			!slices.Equal(options.HostConfig.CapDrop, []string{"ALL"}) || len(options.HostConfig.CapAdd) != 0 ||
			!slices.Equal(options.HostConfig.SecurityOpt, []string{"no-new-privileges"}) || options.HostConfig.Runtime != "runc" ||
			options.HostConfig.LogConfig.Type != "none" || options.HostConfig.Memory != 128<<20 ||
			options.HostConfig.PidsLimit == nil || *options.HostConfig.PidsLimit != 16 || len(options.HostConfig.Mounts) != 1 ||
			options.HostConfig.Mounts[0].VolumeOptions == nil || !options.HostConfig.Mounts[0].VolumeOptions.NoCopy {
			t.Fatalf("helper %s escaped the closed specification: %#v", role, options)
		}
	}
	if !slices.Equal(helpers[port.ContainerImport], []string{"0:0"}) ||
		!slices.Equal(helpers[port.ContainerKeeper], []string{"65532:65532"}) ||
		!slices.Equal(helpers[port.ContainerExport], []string{"65532:65532"}) {
		t.Fatalf("helper users = %#v", helpers)
	}

	wantOutputOptions := map[string]string{
		"type": "tmpfs", "device": "tmpfs",
		"o": fmt.Sprintf("size=%d,uid=65532,gid=65532,nosuid,nodev,noexec", fixture.request.Limits.OutputBytes),
	}
	foundOutput := false
	for _, options := range fixture.engine.volumeCreationHistory() {
		if options.Labels["org.cpgen.role"] != string(port.ResourceOutput) {
			continue
		}
		foundOutput = true
		if options.Driver != "local" || !maps.Equal(options.DriverOpts, wantOutputOptions) {
			t.Fatalf("output volume options = %#v", options)
		}
	}
	if !foundOutput {
		t.Fatal("output volume was not created")
	}
}

func TestDockerTransferLifecycleCanary(t *testing.T) {
	if os.Getenv("CPGEN_DOCKER_TRANSFER_CANARY") != "1" {
		t.Skip("set CPGEN_DOCKER_TRANSFER_CANARY=1 to run the real Engine transfer lifecycle")
	}
	lockFile, err := os.Open("../../../../config/toolchains/docker-v1.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	lock, err := toolchain.LoadLock(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "unix:///var/run/docker.sock"
	if runtime.GOOS == "windows" {
		endpoint = "npipe:////./pipe/docker_engine"
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

	source := []byte("#include <iostream>\n#include <string>\nint main() { std::string s; std::getline(std::cin, s); std::cout << s; std::cerr << \"err\"; return 7; }\n")
	request := compileRequest()
	request.SourceBundle.Files = []port.SourceFile{{Path: "main.cpp", Blob: domain.BlobRef{Digest: domain.SumBytes(source), Size: int64(len(source))}}}
	request.SourceBundle.Digest, err = port.ComputeSourceBundleDigest(request.SourceBundle)
	if err != nil {
		t.Fatal(err)
	}
	identity := planIdentity()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	identity.OperationNonce = hex.EncodeToString(nonce[:])
	plan, err := docker.BuildCompilePlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	calls := []domain.AttemptCallID{
		"call_00000000000000000000000000000021", "call_00000000000000000000000000000022",
		"call_00000000000000000000000000000023", "call_00000000000000000000000000000024",
	}
	events := &eventLog{}
	probeIdentity := port.ProbeAuthorizationIdentity{
		LogicalOperationID: identity.LogicalOperationID, RunID: identity.RunID, AttemptID: identity.AttemptID,
		OwnerID: "owner_00000000000000000000000000000003", LeaseEpoch: identity.LeaseEpoch,
		ScopeDigest: domain.SumBytes([]byte("canary-scope")), PlanDigest: plan.PlanDigest,
	}
	claims := newRecordingClaims(events, probeIdentity, calls)
	auth, err := port.NewSlice0ProbeAuthorization(probeIdentity, plan, claims)
	if err != nil {
		t.Fatal(err)
	}
	artifactSink := &recordingArtifactSink{events: events}
	runner, err := docker.NewRunner(docker.RunnerOptions{
		Engine: engine, Config: config, Lock: lock, EngineIdentityDigest: identity.EngineIdentityDigest,
		Blobs:     &recordingBlobs{events: events, data: map[domain.Digest][]byte{request.SourceBundle.Files[0].Blob.Digest: source}},
		Artifacts: artifactSink, Watchdog: &recordingWatchdog{events: events},
		Limits: docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 15 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	result, err := runner.Compile(ctx, auth, request)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	if result.Program == nil || result.Program.Blob.Size == 0 || result.Program.CallID != calls[3] {
		t.Fatalf("canary program = %#v", result.Program)
	}
	program := artifactSink.finalizedBytes("program/main")
	if int64(len(program)) != result.Program.Blob.Size || domain.SumBytes(program) != result.Program.Blob.Digest {
		t.Fatal("compiled program bytes do not match the promoted artifact")
	}

	runIdentity := identity
	runIdentity.LogicalOperationID = "run-solution-canary"
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	runIdentity.OperationNonce = hex.EncodeToString(nonce[:])
	stdin := []byte("canary input\n")
	stdinRef := domain.BlobRef{Digest: domain.SumBytes(stdin), Size: int64(len(stdin))}
	runRequest := port.RunRequest{
		Role: port.RoleSolution, Program: result.Program.Blob, Stdin: &stdinRef,
		Limits: port.RunLimits{Time: time.Second, MemoryBytes: 128 << 20, PIDs: 16, StdoutBytes: 1024, StderrBytes: 1024},
	}
	runPlan, err := docker.BuildRunPlan(runRequest, lock, runIdentity)
	if err != nil {
		t.Fatal(err)
	}
	runCalls := []domain.AttemptCallID{"call_00000000000000000000000000000025", "call_00000000000000000000000000000026"}
	runProbeIdentity := port.ProbeAuthorizationIdentity{
		LogicalOperationID: runIdentity.LogicalOperationID, RunID: runIdentity.RunID, AttemptID: runIdentity.AttemptID,
		OwnerID: "owner_00000000000000000000000000000003", LeaseEpoch: runIdentity.LeaseEpoch,
		ScopeDigest: domain.SumBytes([]byte("run-canary-scope")), PlanDigest: runPlan.PlanDigest,
	}
	runClaims := newRecordingClaims(events, runProbeIdentity, runCalls)
	runAuth, err := port.NewSlice0ProbeAuthorization(runProbeIdentity, runPlan, runClaims)
	if err != nil {
		t.Fatal(err)
	}
	runRunner, err := docker.NewRunner(docker.RunnerOptions{
		Engine: engine, Config: config, Lock: lock, EngineIdentityDigest: runIdentity.EngineIdentityDigest,
		Blobs:     &recordingBlobs{events: events, data: map[domain.Digest][]byte{result.Program.Blob.Digest: program, stdinRef.Digest: stdin}},
		Artifacts: &recordingArtifactSink{events: events}, Watchdog: &recordingWatchdog{events: events},
		Limits: docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 15 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	runResult, err := runRunner.Run(ctx, runAuth, runRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := runResult.Validate(); err != nil {
		t.Fatal(err)
	}
	if runResult.Outcome != domain.ProcessExited || runResult.ExitCode == nil || *runResult.ExitCode != 7 ||
		runResult.Stdout == nil || runResult.Stdout.Blob.Digest != domain.SumBytes([]byte("canary input")) ||
		runResult.Stderr == nil || runResult.Stderr.Blob.Digest != domain.SumBytes([]byte("err")) {
		t.Fatalf("run canary result = %#v", runResult)
	}
}
