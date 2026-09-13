//go:build cpgen_slice0_probe

package probe_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/probe"
	"cpgen/internal/toolchain"
)

func TestSlice0DockerCapabilities(t *testing.T) {
	if os.Getenv("CPGEN_RUN_DOCKER_CANARY") != "1" {
		t.Skip("set CPGEN_RUN_DOCKER_CANARY=1 to run the real Docker capability canary")
	}
	lockFile, err := os.Open("../../config/toolchains/docker-v1.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := toolchain.LoadLock(lockFile)
	closeErr := lockFile.Close()
	if err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
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
	report, err := docker.CheckStatic(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := docker.NewEngineClient(config)
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	watchdog, err := docker.NewDetachedWatchdogController(docker.DetachedWatchdogOptions{
		Config: config, EngineIdentity: report.EngineIdentityDigest, ControlDirectory: t.TempDir(),
		Executable: executable, ArmTimeout: 15 * time.Second,
		CommandFactory: func(executable, controlPath string) *exec.Cmd {
			command := exec.Command(executable, "-test.run=^TestSlice0WatchdogServiceChild$")
			command.Env = append(os.Environ(), "CPGEN_SLICE0_WATCHDOG_CHILD=1", "CPGEN_SLICE0_WATCHDOG_CONTROL="+controlPath)
			return command
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := probe.NewMemoryArtifactStore()
	runner, err := docker.NewRunner(docker.RunnerOptions{
		Engine: engine, Config: config, Lock: lock, EngineIdentityDigest: report.EngineIdentityDigest,
		Blobs: store, Artifacts: store, Watchdog: watchdog,
		Limits: docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 15 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	harness, err := probe.NewSlice0ProbeHarness(probe.Dependencies{
		Runner: runner, Lock: lock, Artifacts: store, EngineIdentityDigest: report.EngineIdentityDigest,
	})
	if err != nil {
		t.Fatal(err)
	}

	compile, err := harness.Probe(context.Background(), port.ProfileCompileV2)
	if err != nil {
		t.Fatal(err)
	}
	if err := compile.Capabilities.Validate(port.ProfileCompileV2); err != nil {
		t.Fatal(err)
	}

	mvp, err := harness.Probe(context.Background(), port.ProfileExecuteMVPV2)
	if err != nil {
		t.Fatal(err)
	}
	if err := mvp.Capabilities.Validate(port.ProfileExecuteMVPV2); err != nil {
		t.Fatal(err)
	}
	if len(mvp.CallTrace.PhysicalAttemptCallIDs) != 13 {
		t.Fatalf("MVP capability trace has %d physical calls, want 13", len(mvp.CallTrace.PhysicalAttemptCallIDs))
	}

	release, err := harness.Probe(context.Background(), port.ProfileExecuteReleaseV2)
	if err == nil {
		t.Fatal("release capability unexpectedly succeeded without a release cgroup implementation")
	}
	var check *docker.CheckError
	if !errors.As(err, &check) || check.Failure.Code != domain.FailureCapabilityMissing || check.Failure.Class != domain.FailureIncompatible {
		t.Fatalf("release failure = %T %v", err, err)
	}
	if err := release.CallTrace.Validate(); err != nil || len(release.CallTrace.PhysicalAttemptCallIDs) != 1 {
		t.Fatalf("release probe trace = %#v, %v", release.CallTrace, err)
	}
}

func TestSlice0WatchdogServiceChild(t *testing.T) {
	if os.Getenv("CPGEN_SLICE0_WATCHDOG_CHILD") != "1" {
		return
	}
	if err := docker.RunWatchdogService(context.Background(), os.Getenv("CPGEN_SLICE0_WATCHDOG_CONTROL")); err != nil {
		t.Fatal(err)
	}
}
