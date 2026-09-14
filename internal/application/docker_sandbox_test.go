package application_test

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	sandboxexec "cpgen/internal/adapter/sandbox"
	docker "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

func TestDockerSandboxSessionCompilesRunsAndReplaysWithSQLite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	base := newDockerSandboxTestConfig(t, ctx)
	root := t.TempDir()
	f := newCoordinatorFixtureAt(t, "f6", domain.BudgetLimits{MaxArtifactBytes: 64 << 20, MaxSandboxCreates: 20, MaxActiveTimeMilliseconds: 180000}, []domain.StageName{"prepare", "exercise"}, domain.SumBytes([]byte("Docker canary")), time.Now().UTC())
	blobs, err := blob.NewStore(filepath.Join(root, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000f6", LogicalOperationID: "verified-solution", Kind: domain.CallSandboxCompile, ScopeDigest: domain.SumBytes([]byte("source content")), ExpectedRunVersion: 2}
	sources, err := sandboxexec.NewArtifactSink(f.store, blobs, clock.Real{}, identity)
	if err != nil {
		t.Fatal(err)
	}
	decl := port.ArtifactDeclaration{MediaType: "text/x-c++src", Role: domain.ArtifactSource, LogicalPath: "solution/reference/main.cpp", MaxBytes: 4096, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.solution-source/v1", Producer: "solution"}}
	source, err := sources.Publish(ctx, decl, []byte("#include <iostream>\nint main(){long long a,b;if(std::cin>>a>>b)std::cout<<a+b<<'\\n';}\n"))
	if err != nil {
		t.Fatal(err)
	}
	manifest := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", Files: []port.SourceFile{{Path: "main.cpp", Blob: source.Blob}}, EntryPoint: "main.cpp"}
	manifest.Digest, err = port.ComputeSourceBundleDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	newSession := func() *sandboxexec.Session {
		config := base
		config.Store, config.Blobs, config.Clock, config.Identity = f.store, blobs, clock.Real{}, identity
		session, err := sandboxexec.NewSession(config)
		if err != nil {
			t.Fatal(err)
		}
		return session
	}
	runDockerSandboxSessionCanary(t, ctx, f, blobs, sources, decl, source, manifest, newSession)
}

func newDockerSandboxTestConfig(t *testing.T, ctx context.Context) sandboxexec.Config {
	t.Helper()
	if os.Getenv("CPGEN_RUN_DOCKER_CANARY") != "1" {
		t.Skip("real Docker canary is opt-in")
	}
	lockPath := os.Getenv("CPGEN_DOCKER_TOOLCHAIN_LOCK")
	if !filepath.IsAbs(lockPath) {
		t.Fatal("select an absolute CPGEN_DOCKER_TOOLCHAIN_LOCK")
	}
	file, err := os.Open(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := toolchain.LoadLock(file)
	_ = file.Close()
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "unix:///var/run/docker.sock"
	if runtime.GOOS == "windows" {
		endpoint = "npipe:////./pipe/docker_engine"
	}
	config := docker.Config{EngineEndpoint: endpoint, APIVersion: docker.RequiredAPIVersion, BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID), ExecutionProtocol: docker.ExecutionProtocolDockerDirectV2}
	static, err := docker.CheckStatic(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := docker.NewEngineClient(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	root := t.TempDir()
	binary := filepath.Join(root, "cpgen")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/cpgen")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build watchdog executable: %v: %s", err, output)
	}
	watchdog, err := docker.NewDetachedWatchdogController(docker.DetachedWatchdogOptions{Config: config, EngineIdentity: static.EngineIdentityDigest, ControlDirectory: filepath.Join(root, "watchdog"), Executable: binary, ArmTimeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return sandboxexec.Config{Engine: engine, Config: config, Lock: lock, EngineIdentity: static.EngineIdentityDigest, Watchdog: watchdog, Limits: docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 15 * time.Second}}
}

func runDockerSandboxSessionCanary(t *testing.T, ctx context.Context, f coordinatorFixture, blobs *blob.Store, sources *sandboxexec.ArtifactSink, decl port.ArtifactDeclaration, source domain.PendingArtifact, manifest port.SourceBundleManifest, newSession func() *sandboxexec.Session) {
	t.Helper()
	request := port.CompileRequest{Language: port.LanguageCPP20, Role: port.RoleSolution, SourceBundle: manifest, Toolchain: "cpp20-gcc-bookworm-v1", Limits: port.CompileLimits{Time: 30 * time.Second, MemoryBytes: 512 << 20, PIDs: 64, OutputBytes: 8 << 20}, ExpectedOutput: "result/files/main"}
	worker := newSession()
	compiled, err := worker.Compile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if compiled.Value == nil || compiled.Value.Outcome != domain.CompileOK || compiled.Value.Program == nil {
		t.Fatalf("compile failed: %+v", compiled)
	}
	replayed, err := newSession().Compile(ctx, request)
	if err != nil || replayed.Value == nil || !compiled.CallTrace.Equal(replayed.CallTrace) {
		t.Fatalf("compile replay: %+v %v", replayed, err)
	}
	decl.Role, decl.MediaType, decl.LogicalPath = domain.ArtifactInput, "text/plain", "samples/01.in"
	input, err := sources.Publish(ctx, decl, []byte("7 11\n"))
	if err != nil {
		t.Fatal(err)
	}
	run := port.RunRequest{Role: port.RoleSolution, Program: compiled.Value.Program.Blob, Stdin: &input.Blob, Limits: port.RunLimits{Time: 2 * time.Second, MemoryBytes: 128 << 20, PIDs: 16, StdoutBytes: 4096, StderrBytes: 4096}}
	result, err := worker.Run(ctx, run)
	if err != nil {
		t.Fatal(err)
	}
	if result.Value == nil || result.Value.Outcome != domain.ProcessExited || result.Value.ExitCode == nil || *result.Value.ExitCode != 0 || result.Value.Stdout == nil {
		t.Fatalf("run failed: %+v", result)
	}
	reader, err := blobs.OpenVerified(ctx, result.Value.Stdout.Blob)
	if err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(output) != "18\n" {
		t.Fatalf("sample output=%q err=%v", output, err)
	}
	again, err := newSession().Run(ctx, run)
	if err != nil || !again.CallTrace.Equal(result.CallTrace) {
		t.Fatalf("run replay: %+v %v", again, err)
	}
	artifacts := append(worker.Artifacts(), source, input)
	if len(artifacts) != 11 {
		t.Fatalf("compile/run receipt set has %d artifacts, want 11", len(artifacts))
	}
	occurrences := make([]domain.PendingOccurrence, len(artifacts))
	for index := range artifacts {
		occurrences[index] = domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &artifacts[index]}
	}
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	stageOutput := domain.SumBytes([]byte("compiled and sample checked"))
	if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &stageOutput, NextStage: "exercise", NextInputDigest: &stageOutput, Occurrences: occurrences, IdempotencyKey: coordinatorID("finish", "real sandbox artifacts"), At: time.Now().UTC()}); err != nil {
		t.Fatalf("atomic source/program/stream/result attachment: %v", err)
	}
	budget, err := f.store.BudgetSnapshot(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if budget.Remaining[domain.BudgetDockerContainerCreates] != 14 {
		t.Fatalf("compile/run/replay container usage differs: %+v", budget)
	}
	t.Logf("real compile and sample passed; compile physical=%d run physical=%d; replay identities unchanged", len(compiled.CallTrace.PhysicalAttemptCallIDs), len(result.CallTrace.PhysicalAttemptCallIDs))
}
