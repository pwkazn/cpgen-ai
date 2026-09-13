//go:build cpgen_slice0_probe

package probe

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

func TestVerticalRoleOutcomeBoundaries(t *testing.T) {
	zero, one, three, four, nine := 0, 1, 3, 4, 9
	signal := "SIGSEGV"
	for _, test := range []struct {
		name    string
		result  port.RunResult
		verdict domain.SolutionVerdict
	}{
		{name: "nonzero", result: verticalRun(domain.ProcessExited, &one, nil), verdict: domain.SolutionRE},
		{name: "signal", result: verticalRun(domain.ProcessSignaled, nil, &signal), verdict: domain.SolutionRE},
		{name: "TLE", result: verticalRun(domain.ProcessTLE, nil, nil), verdict: domain.SolutionTLE},
		{name: "MLE", result: verticalRun(domain.ProcessMLE, nil, nil), verdict: domain.SolutionMLE},
		{name: "OLE", result: verticalRun(domain.ProcessOLE, nil, nil), verdict: domain.SolutionOLE},
		{name: "success", result: verticalRun(domain.ProcessExited, &zero, nil), verdict: domain.SolutionOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, blocked, err := solutionOutcome(test.result)
			if err != nil || blocked || got != test.verdict {
				t.Fatalf("solution outcome = %q blocked=%v err=%v", got, blocked, err)
			}
		})
	}
	validator, blocked, err := validatorOutcome(verticalRun(domain.ProcessExited, &nine, nil))
	if err != nil || blocked || validator != domain.ValidatorError {
		t.Fatalf("validator unknown = %q blocked=%v err=%v", validator, blocked, err)
	}
	for _, test := range []struct {
		exit int
		want domain.CheckerOutcome
	}{
		{exit: one, want: domain.CheckerWA}, {exit: 2, want: domain.CheckerPE},
		{exit: four, want: domain.CheckerPE}, {exit: three, want: domain.CheckerError},
		{exit: nine, want: domain.CheckerError},
	} {
		got, blocked, err := checkerOutcome(verticalRun(domain.ProcessExited, &test.exit, nil))
		if err != nil || blocked || got != test.want {
			t.Fatalf("checker exit %d = %q blocked=%v err=%v", test.exit, got, blocked, err)
		}
	}
}

func TestVerticalReportRejectsIncompleteDifferentialEvidence(t *testing.T) {
	report := VerticalReport{SchemaVersion: VerticalReportSchemaVersion, Status: VerticalPassed, Reason: "raw strings happened to match"}
	if err := report.Validate(); err == nil {
		t.Fatal("passed report without typed checker evidence was accepted")
	}
}

func TestStructuralPackageRemainsProbeOnly(t *testing.T) {
	if StructuralPackageReportKind != "PROBE_STRUCTURAL_ONLY" {
		t.Fatalf("structural report kind = %q", StructuralPackageReportKind)
	}
	for _, root := range []string{"package.go", "../packageprobe"} {
		info, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		paths := []string{root}
		if info.IsDir() {
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			paths = paths[:0]
			for _, entry := range entries {
				if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
					paths = append(paths, filepath.Join(root, entry.Name()))
				}
			}
		}
		for _, path := range paths {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range [][]byte{
				[]byte("database/sql"), []byte("internal/workflow"), []byte("internal/persistence"),
				[]byte("PackageVerificationReceipt"), []byte(`"READY"`),
			} {
				if bytes.Contains(source, forbidden) {
					t.Fatalf("probe-only boundary %q appears in %s", forbidden, path)
				}
			}
		}
	}
}

func TestSlice0ABVertical(t *testing.T) {
	if os.Getenv("CPGEN_RUN_DOCKER_CANARY") != "1" {
		t.Skip("set CPGEN_RUN_DOCKER_CANARY=1 to run the real A+B vertical probe")
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
	watchdog, err := docker.NewInProcessWatchdogController("slice0-vertical-watchdog-token-00000000000000000001", engine)
	if err != nil {
		t.Fatal(err)
	}
	store := NewMemoryArtifactStore()
	runner, err := docker.NewRunner(docker.RunnerOptions{
		Engine: engine, Config: config, Lock: lock, EngineIdentityDigest: report.EngineIdentityDigest,
		Blobs: store, Artifacts: store, Watchdog: watchdog,
		Limits: docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 15 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	harness, err := NewSlice0ProbeHarness(Dependencies{Runner: runner, Lock: lock, Artifacts: store, EngineIdentityDigest: report.EngineIdentityDigest})
	if err != nil {
		t.Fatal(err)
	}
	vertical, err := harness.RunVertical(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := vertical.Validate(); err != nil {
		t.Fatalf("vertical report = %#v: %v", vertical, err)
	}
	if vertical.Status != VerticalPassed || len(vertical.CallTraces) != 42 {
		t.Fatalf("vertical status=%s traces=%d reason=%s failure=%v", vertical.Status, len(vertical.CallTraces), vertical.Reason, vertical.Failure)
	}
	wantPrograms := map[string]domain.Digest{
		"brute":     "sha256:8ed1de2cdafb1650b1695d2dcb10333fd443690e6a03ece6b77d71523e10e7c1",
		"checker":   "sha256:557483a66576c44a98e3ccc53b258519fcad81506e28b77f7b922f38635cd441",
		"generator": "sha256:48786b254ff33ed340a877005c6040e3dfc3ebcb6118d5cd91a458db27689fb7",
		"reference": "sha256:8ed1de2cdafb1650b1695d2dcb10333fd443690e6a03ece6b77d71523e10e7c1",
		"validator": "sha256:fc1d431b643d790821aabe6742856485905fd4e23886e1af14a55fe2a3aff578",
	}
	for name, want := range wantPrograms {
		if got := vertical.Programs[name].Digest; got != want {
			t.Fatalf("%s program digest = %s, want %s", name, got, want)
		}
	}
	packageDirectory := t.TempDir()
	packageRoot, err := os.OpenRoot(packageDirectory)
	if err != nil {
		t.Fatal(err)
	}
	defer packageRoot.Close()
	packageReport, err := harness.RunStructuralPackage(context.Background(), packageRoot, vertical)
	if err != nil {
		t.Fatal(err)
	}
	if err := packageReport.Validate(); err != nil {
		t.Fatalf("structural package report = %#v: %v", packageReport, err)
	}
	const wantPackageID domain.Digest = "sha256:e680b9bd726499a1946c1fa3ae5508a14cc832f4fb526a4e80650c8ab435e4a3"
	const wantManifestDigest domain.Digest = "sha256:c5666c6aba707e49948bab25d691e1e4192ea4527ef1f78ee7eae04f0538cfd1"
	if packageReport.PackageID != wantPackageID || packageReport.ManifestDigest != wantManifestDigest {
		t.Fatalf("structural package identity = (%s, %s), want (%s, %s)", packageReport.PackageID, packageReport.ManifestDigest, wantPackageID, wantManifestDigest)
	}
	if packageReport.FileCount != 23 || len(packageReport.Artifacts) != 24 {
		t.Fatalf("structural package files=%d artifacts=%d", packageReport.FileCount, len(packageReport.Artifacts))
	}
}

func verticalRun(outcome domain.ProcessOutcome, exit *int, signal *string) port.RunResult {
	call := domain.AttemptCallID("call_00000000000000000000000000000077")
	return port.RunResult{
		CallTrace: domain.CallTrace{LogicalOperationID: "vertical-vector", DispatchKind: domain.DispatchDispatched,
			PhysicalAttemptCallIDs: []domain.AttemptCallID{call}, ResultAttemptCallID: &call},
		Outcome: outcome, ExitCode: exit, Signal: signal,
	}
}
