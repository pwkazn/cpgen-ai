package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/agent"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/packageprobe"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

// Generation uses real durable calls against local HTTP fixtures, with the
// exact production policy identities. Then a separately built, unmodified CLI
// resumes the committed Solution and executes Docker plus its own watchdog.
// The fixture transport is test-only; no CLI/provider security option changes.
func TestSolutionPublicCLIResumesCommittedDraftThroughDocker(t *testing.T) {
	verifyPublicGenerationCLI(t, false)
}

func TestMVPPublicCLIResumesDataDraftAndExportsVerifiedPackage(t *testing.T) {
	verifyPublicGenerationCLI(t, true)
}

func verifyPublicGenerationCLI(t *testing.T, mvp bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	base := newDockerSandboxTestConfig(t, ctx)
	root := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "solution.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if mvp {
		raw = []byte(strings.Replace(string(raw), workflow.LegacySolutionCheckpointRevision, workflow.GenerationRevision, 1))
	}
	lockDigest, err := base.Lock.Digest()
	if err != nil {
		t.Fatal(err)
	}
	lockSource := os.Getenv("CPGEN_DOCKER_TOOLCHAIN_LOCK")
	lockCopy := filepath.Join(root, "bound-toolchain.lock.json")
	lockRaw, err := os.ReadFile(lockSource)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockCopy, lockRaw, 0600); err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.NewReplacer(
		"D:/cpgen-private/solution", filepath.ToSlash(filepath.Join(root, "state")),
		"D:/cpgen-private/toolchains/docker-v1.lock.json", filepath.ToSlash(lockCopy),
		"sha256:"+strings.Repeat("0", 64), string(lockDigest),
		"npipe:////./pipe/docker_engine", base.Config.EngineEndpoint,
		"https://provider.example.com/v1", "https://93.184.216.34/v1",
		"https://similarity.example.com/search", "https://93.184.216.34/search",
		"replace-with-provider-identity", "fixture",
	).Replace(string(raw)))
	cfg, err := config.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if mvp {
		snapshot, err := base.Lock.MarshalIndent()
		if err != nil {
			t.Fatal(err)
		}
		sandbox := *cfg.Sandbox
		sandbox.ToolchainLockSnapshot = snapshot
		cfg.Sandbox = &sandbox
	}
	base.Limits.CleanupTimeout = cfg.Runtime.CleanupWait
	configPath := filepath.Join(root, "cpgen.yaml")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "cpgen")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/cpgen")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v %s", err, output)
	}
	runCLI := func(wantCode int, args ...string) domain.RunSnapshot {
		t.Helper()
		command := exec.CommandContext(ctx, binary, append([]string{"--config", configPath}, args...)...)
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		code := 0
		if err != nil {
			var exit *exec.ExitError
			if !errors.As(err, &exit) {
				t.Fatal(err)
			}
			code = exit.ExitCode()
		}
		if code != wantCode {
			t.Fatalf("CLI %v: code=%d %s %s", args, code, stdout.String(), stderr.String())
		}
		var result struct {
			Data  domain.RunSnapshot `json:"data"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
			t.Fatalf("CLI JSON: %v %s", err, stdout.String())
		}
		if mvp && wantCode == 9 && len(args) >= 2 && args[0] == "run" && args[1] == "resume" {
			if result.Error == nil || !strings.Contains(result.Error.Message, "test pause before package commit") {
				t.Fatalf("CLI failed before the intended package transaction pause: %s", stdout.String())
			}
		}
		return result.Data
	}
	runCLI(0, "config", "validate")
	requestPath, err := filepath.Abs(filepath.Join("..", "..", "config", "slice2-zero-budget.request.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	zero := runCLI(6, "generate", "--request", requestPath)
	if zero.CurrentStage != "idea" || zero.State != domain.RunNeedsReview || zero.WorkflowRevision != cfg.Workflow.Revision {
		t.Fatalf("CLI zero-budget admission: %+v", zero)
	}
	if again := runCLI(6, "run", "resume", string(zero.RunID)); again.Version != zero.Version {
		t.Fatal("review resume changed zero-budget run")
	}

	app, err := application.Bootstrap(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	store := app.Runtime.(*sqlite.Store)
	pause := &solutionCLIPauseStore{Store: store}
	if mvp {
		pause.stage = "data_verify"
	}
	blobs, err := blob.NewStore(cfg.Paths.Artifacts)
	if err != nil {
		t.Fatal(err)
	}
	outputs := solutionDockerOutputs(t, "pass")
	if mvp {
		outputs = dataDockerOutputs(t, "pass")
	}
	var calls, searches atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/search" {
			searches.Add(1)
			similarityExecutorSuccess(w, r)
			return
		}
		ordinal := calls.Add(1)
		steps := []string{"idea.draft", "statement.draft", "solution.draft"}
		if mvp {
			steps = append(steps, "data.draft")
		}
		if ordinal > int32(len(steps)) {
			t.Error("unexpected model dispatch")
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "cli-fixture", "choices": []any{map[string]any{"message": map[string]string{"content": string(outputs[steps[ordinal-1]])}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "93.184.216.34:443" {
			return nil, errors.New("unexpected fixture endpoint")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	llmConfig, _, err := application.BuildLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	llmConfig.HTTPClient = client
	model, err := agent.NewLangChain(llmConfig)
	if err != nil {
		t.Fatal(err)
	}
	content, retry, err := application.BuildGenerationExecutionSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	generationConfig := application.GenerationExecutorConfig{Store: pause, Blobs: blobs, LLM: model, Clock: clock.Real{}, Locks: app.Locks, Content: content, RetryPolicy: retry, CostUpperBoundMicroUSD: cfg.Workflow.LLMCostUpperBoundMicroUSD}
	for step, target := range map[string]*application.FormatRepairPolicy{"idea.draft": &generationConfig.IdeaRepair, "statement.draft": &generationConfig.StatementRepair, "solution.draft": &generationConfig.SolutionRepair, "data.draft": &generationConfig.DataRepair} {
		*target, err = application.BuildFormatRepairPolicy(cfg, step)
		if err != nil {
			t.Fatal(err)
		}
	}
	generation, err := application.NewGenerationExecutor(generationConfig)
	if err != nil {
		t.Fatal(err)
	}
	simConfig, policy, err := application.BuildSimilarityConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	simConfig.HTTPClient = client
	provider, err := similarity.New(simConfig)
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := application.NewSimilarityExecutor(application.SimilarityExecutorConfig{Generation: generation, Provider: provider, Policy: policy, Limit: cfg.Similarity.Limit, RetryPolicy: retry, CostUpperBoundMicroUSD: cfg.Workflow.SimilarityCostUpperBoundMicroUSD, WorkflowRevision: cfg.Workflow.Revision})
	if err != nil {
		t.Fatal(err)
	}
	effective, err := cfg.Effective()
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewSlice2RunService(application.Slice2RunServiceConfig{Generation: generation, Similarity: evidence, Reviews: store, ActiveTimeInterval: cfg.Runtime.AccountingHeartbeat, EffectiveConfigJSON: effective, SolutionSandbox: &base})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(cfg.LLM.APIKeyEnv, "fixture-only")
	t.Setenv(cfg.Similarity.APIKeyEnv, "fixture-only")
	seed := int64(9007199254740993)
	request := domain.RunRequest{SchemaVersion: domain.RequestSchemaV1, Mode: domain.RequestModeManual, Brief: "Graphs", Language: "en", Difficulty: "hard", TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"}, Seed: &seed, BudgetLimits: domain.BudgetLimits{MaxLLMCalls: 6, MaxLLMInputTokens: 500000, MaxLLMOutputTokens: 30000, MaxLLMCostMicroUSD: 600000, MaxSimilarityCalls: 2, MaxSimilarityCostMicroUSD: 200000, MaxArtifactBytes: 64 << 20, MaxSandboxCreates: 100, MaxActiveTimeMilliseconds: 180000}}
	wantCalls, wantCode, wantCreates := int32(3), 6, int64(16)
	wantState, wantStage := domain.RunNeedsReview, domain.StageName("solution_checkpoint")
	if mvp {
		request.BudgetLimits.MaxPackageBytes, request.BudgetLimits.MaxActiveTimeMilliseconds = 16<<20, 600000
		wantCalls, wantCode, wantCreates = 4, 0, 96
		wantState, wantStage = domain.RunReady, "package"
	}
	if result, err := service.Generate(ctx, request); !errors.Is(err, errSolutionCLIPause) {
		t.Fatalf("did not stop after committed Solution: %+v %v, calls=%d/%d", result, err, calls.Load(), searches.Load())
	}
	if calls.Load() != wantCalls || searches.Load() != 1 || pause.runID == "" {
		t.Fatalf("fixture chain: LLM=%d similarity=%d", calls.Load(), searches.Load())
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if mvp {
		if err := os.Remove(lockCopy); err != nil {
			t.Fatal(err)
		}
	}
	// The child has no keys or injected transport. Its read-only reconstruction
	// must accept exactly the stored policy and then use real local Docker.
	t.Setenv(cfg.LLM.APIKeyEnv, "")
	t.Setenv(cfg.Similarity.APIKeyEnv, "")
	if mvp {
		pausePublicPackageCommit(t, cfg.Paths.Database)
		runCLI(9, "run", "resume", string(pause.runID))
		crashPublicPackageCommit(t, ctx, configPath, cfg.Paths.Database, pause.runID)
	}
	result := runCLI(wantCode, "run", "resume", string(pause.runID))
	if result.State != wantState || result.CurrentStage != wantStage || result.WorkflowRevision != cfg.Workflow.Revision {
		t.Fatalf("public resume: %+v", result)
	}
	if again := runCLI(wantCode, "run", "resume", string(result.RunID)); again.Version != result.Version {
		t.Fatal("public replay changed committed result")
	}
	if calls.Load() != wantCalls || searches.Load() != 1 {
		t.Fatal("CLI replay dispatched fixture providers")
	}
	readApp, err := application.BootstrapLocal(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer readApp.Close()
	readStore := readApp.Runtime.(*sqlite.Store)
	stage, err := readStore.ReadCommittedSandboxStage(ctx, result.RunID, "solution_verify")
	if err != nil || len(stage.Artifacts) != 33 {
		t.Fatalf("CLI did not commit complete real verification: artifacts=%d %v", len(stage.Artifacts), err)
	}
	budget, err := readStore.BudgetSnapshot(ctx, result.RunID)
	if err != nil || budget.Remaining[domain.BudgetDockerContainerCreates] != 100-wantCreates {
		t.Fatalf("CLI Docker accounting: %+v %v", budget, err)
	}
	if mvp {
		// Export uses only local resources and the run's frozen policy. Make
		// the current execution configuration unusable without touching the
		// original frozen lock needed to reconstruct historical proof.
		offlineConfig := strings.NewReplacer(cfg.Sandbox.EngineEndpoint, cfg.Sandbox.EngineEndpoint+"-offline-test", filepath.ToSlash(cfg.Sandbox.ToolchainLockPath), filepath.ToSlash(filepath.Join(root, "missing-current-lock.json"))).Replace(string(raw))
		if _, err := config.Decode([]byte(offlineConfig)); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, []byte(offlineConfig), 0600); err != nil {
			t.Fatal(err)
		}
		destination := filepath.Join(root, "problem.zip")
		runCLI(0, "run", "export", string(result.RunID), "--output", destination)
		raw, err := os.ReadFile(destination)
		if err != nil {
			t.Fatal(err)
		}
		verified, err := packageprobe.ReadArchive(ctx, raw)
		if err != nil || verified.Manifest.RunID != result.RunID {
			t.Fatalf("CLI export: %v", err)
		}
		record, err := readStore.ReadVerifiedPackage(ctx, result.RunID)
		if err != nil || record.Binding.PackageID != verified.Manifest.PackageID || record.Binding.Archive.Digest != domain.SumBytes(raw) {
			t.Fatalf("export binding: %+v %v", record, err)
		}
		runCLI(9, "run", "export", string(result.RunID), "--output", destination)
		if calls.Load() != wantCalls || searches.Load() != 1 {
			t.Fatal("export dispatched model or Similarity")
		}
		afterExport, err := readStore.GetRun(ctx, result.RunID)
		if err != nil || afterExport.Version != result.Version {
			t.Fatalf("export changed run: %+v %v", afterExport, err)
		}
		afterBudget, err := readStore.BudgetSnapshot(ctx, result.RunID)
		beforeBytes, _ := json.Marshal(budget)
		afterBytes, _ := json.Marshal(afterBudget)
		if err != nil || !bytes.Equal(beforeBytes, afterBytes) {
			t.Fatalf("export changed budget: %v", err)
		}
		revalidateExportedPackageInDocker(t, ctx, base, raw)
	}
}

var errSolutionCLIPause = errors.New("fixture stops before solution verification")

type solutionCLIPauseStore struct {
	*sqlite.Store
	runID domain.RunID
	stage domain.StageName
}

func (s *solutionCLIPauseStore) BeginStage(ctx context.Context, command domain.BeginStageCommand) (domain.StageAttempt, error) {
	stage := s.stage
	if stage == "" {
		stage = "solution_verify"
	}
	if command.StageName == stage {
		s.runID = command.RunID
		return domain.StageAttempt{}, errSolutionCLIPause
	}
	return s.Store.BeginStage(ctx, command)
}
