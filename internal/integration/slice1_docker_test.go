//go:build cpgen_slice0_probe

package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	dockersandbox "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/probe"
	"cpgen/internal/toolchain"
)

func TestSlice1DockerAB(t *testing.T) {
	harness := newSlice1DockerHarness(t)
	vertical, err := harness.RunVertical(context.Background())
	if err != nil {
		t.Fatalf("A+B vertical canary: %v", err)
	}
	if err := vertical.Validate(); err != nil {
		t.Fatalf("vertical report: %v", err)
	}
	seenCalls := make(map[string]struct{})
	for index, trace := range vertical.CallTraces {
		if trace.DispatchKind != domain.DispatchDispatched || len(trace.PhysicalAttemptCallIDs) == 0 {
			t.Fatalf("vertical trace %d omitted dispatched physical lifecycle: %+v", index, trace)
		}
		if trace.ResultAttemptCallID == nil || !slices.Contains(trace.PhysicalAttemptCallIDs, *trace.ResultAttemptCallID) {
			t.Fatalf("vertical trace %d has no terminal result identity: %+v", index, trace)
		}
		for _, callID := range trace.PhysicalAttemptCallIDs {
			if _, exists := seenCalls[string(callID)]; exists {
				t.Fatalf("vertical report reused physical call identity %q", callID)
			}
			seenCalls[string(callID)] = struct{}{}
		}
	}
	if len(seenCalls) != 42 {
		t.Fatalf("vertical report has %d unique physical calls, want 42", len(seenCalls))
	}
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	packageReport, err := harness.RunStructuralPackage(context.Background(), root, vertical)
	if err != nil {
		t.Fatalf("structural package canary: %v", err)
	}
	if err := packageReport.Validate(); err != nil {
		t.Fatalf("package report: %v", err)
	}
	if packageReport.FileCount < 10 || len(packageReport.Artifacts) != packageReport.FileCount+1 {
		t.Fatalf("package report has incomplete artifact evidence: files=%d artifacts=%d", packageReport.FileCount, len(packageReport.Artifacts))
	}
	if _, ok := packageReport.Artifact("manifest.json"); !ok {
		t.Fatal("package report omitted manifest artifact")
	}
}

func newSlice1DockerHarness(t *testing.T) *probe.Harness {
	t.Helper()
	config, lock, static := loadSlice1DockerCanary(t)
	engine, err := dockersandbox.NewEngineClient(config)
	if err != nil {
		t.Fatalf("create Docker client: %v", err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	artifacts := probe.NewMemoryArtifactStore()
	watchdog, err := dockersandbox.NewInProcessWatchdogController("slice1-integration-watchdog-token-00000000000000000001", engine)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := dockersandbox.NewRunner(dockersandbox.RunnerOptions{
		Engine: engine, Config: config, Lock: lock, EngineIdentityDigest: static.EngineIdentityDigest,
		Blobs: artifacts, Artifacts: artifacts, Watchdog: watchdog,
		Limits: dockersandbox.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 15 * time.Second},
	})
	if err != nil {
		t.Fatalf("create Docker runner: %v", err)
	}
	harness, err := probe.NewSlice0ProbeHarness(probe.Dependencies{Runner: runner, Lock: lock, Artifacts: artifacts, EngineIdentityDigest: static.EngineIdentityDigest})
	if err != nil {
		t.Fatal(err)
	}
	return harness
}

func loadSlice1DockerCanary(t *testing.T) (dockersandbox.Config, toolchain.Lock, dockersandbox.StaticReport) {
	t.Helper()
	if os.Getenv("CPGEN_RUN_DOCKER_CANARY") != "1" {
		t.Skip("set CPGEN_RUN_DOCKER_CANARY=1 to run the real Docker canary")
	}
	lockPath := filepath.Join("..", "..", "config", "toolchains", "docker-v1.lock.json")
	lockFile, err := os.Open(lockPath)
	if err != nil {
		t.Fatalf("open Docker toolchain lock: %v", err)
	}
	lock, loadErr := toolchain.LoadLock(lockFile)
	closeErr := lockFile.Close()
	if loadErr != nil || closeErr != nil {
		t.Fatal(errors.Join(loadErr, closeErr))
	}
	endpoint := "unix:///var/run/docker.sock"
	if runtime.GOOS == "windows" {
		endpoint = "npipe:////./pipe/docker_engine"
	}
	config := dockersandbox.Config{
		EngineEndpoint: endpoint, APIVersion: dockersandbox.RequiredAPIVersion,
		BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID),
		ExecutionProtocol: dockersandbox.ExecutionProtocolDockerDirectV2,
	}
	if _, err := config.Validate(runtime.GOOS); err != nil {
		t.Fatalf("validate Docker canary config: %v", err)
	}
	static, err := dockersandbox.CheckStatic(context.Background(), config)
	if err != nil {
		// Docker is an explicit opt-in capability. A missing local daemon is an
		// expected blocked capability, not an integration test failure.
		t.Skipf("Docker canary unavailable: %v", err)
	}
	return config, lock, static
}
