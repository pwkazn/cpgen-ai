package application_test

import (
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
)

// Opt-in paid-provider acceptance. Only Similarity uses a local TLS fixture.
// The persistent root contains private generated content, never credentials.
func TestDeepSeekLiveMVPWithFixtureSimilarity(t *testing.T) {
	if os.Getenv("CPGEN_RUN_DEEPSEEK_MVP") != "1" {
		t.Skip("paid DeepSeek acceptance is opt-in")
	}
	if os.Getenv("CPGEN_DEEPSEEK_API_KEY") == "" {
		t.Fatal("CPGEN_DEEPSEEK_API_KEY is required")
	}
	root := os.Getenv("CPGEN_DEEPSEEK_TEST_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("select an absolute private CPGEN_DEEPSEEK_TEST_ROOT")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(root, 0700))
	// A fresh directory prevents an accidental test rerun from creating another paid run.
	marker, err := os.OpenFile(filepath.Join(root, "started"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	must(err)
	must(marker.Close())
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	base := newDockerSandboxTestConfig(t, ctx)
	binary := filepath.Join(root, "cpgen")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/cpgen")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build export CLI before paid dispatch: %v %s", err, output)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "mvp.example.yaml"))
	must(err)
	digest, err := base.Lock.Digest()
	must(err)
	modelName := os.Getenv("CPGEN_DEEPSEEK_MODEL")
	if modelName == "" {
		modelName = "deepseek-v4-flash"
	}
	raw = []byte(strings.NewReplacer(
		"D:/cpgen-private/mvp", filepath.ToSlash(filepath.Join(root, "state")),
		"https://provider.example.com/v1", "https://api.deepseek.com",
		"replace-with-supported-model", modelName,
		"CPGEN_LLM_API_KEY", "CPGEN_DEEPSEEK_API_KEY",
		"timeout: 60s", "timeout: 180s",
		"max_output_tokens: 8192", "max_output_tokens: 32768",
		"max_format_repairs: 0", "max_format_repairs: 1",
		"https://similarity.example.com/search", "https://93.184.216.34/search",
		"replace-with-provider-identity", "fixture",
		"replace-with-index-revision", "local-fixture-only",
		"D:/cpgen-private/toolchains/docker-v1.lock.json", filepath.ToSlash(os.Getenv("CPGEN_DOCKER_TOOLCHAIN_LOCK")),
		"sha256:"+strings.Repeat("0", 64), string(digest),
		"npipe:////./pipe/docker_engine", base.Config.EngineEndpoint,
	).Replace(string(raw)))
	cfg, err := config.Decode(raw)
	must(err)
	base.Limits.CleanupTimeout = cfg.Runtime.CleanupWait
	configPath := filepath.Join(root, "config.yaml")
	must(os.WriteFile(configPath, raw, 0600))
	app, err := application.Bootstrap(ctx, cfg)
	must(err)
	defer app.Close()
	store := app.Runtime.(*sqlite.Store)
	blobs, err := blob.NewStore(cfg.Paths.Artifacts)
	must(err)
	llmConfig, _, err := application.BuildLLMConfig(cfg)
	must(err)
	// No custom client or transport is installed on the real model adapter.
	model, err := agent.NewLangChain(llmConfig)
	must(err)
	content, retry, err := application.BuildSlice2ExecutionSettings(cfg)
	must(err)
	gc := application.GenerationExecutorConfig{Store: store, Blobs: blobs, LLM: model, Clock: clock.Real{}, Locks: app.Locks, Content: content, RetryPolicy: retry, CostUpperBoundMicroUSD: cfg.Workflow.LLMCostUpperBoundMicroUSD}
	for step, target := range map[string]*application.FormatRepairPolicy{"idea.draft": &gc.IdeaRepair, "statement.draft": &gc.StatementRepair, "solution.draft": &gc.SolutionRepair, "data.draft": &gc.DataRepair} {
		*target, err = application.BuildFormatRepairPolicy(cfg, step)
		must(err)
	}
	generation, err := application.NewGenerationExecutor(gc)
	must(err)
	var searches atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			http.NotFound(w, r)
			return
		}
		searches.Add(1)
		similarityExecutorSuccess(w, r)
	}))
	defer server.Close()
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig = transport.TLSClientConfig.Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "93.184.216.34:443" {
			return nil, errors.New("unexpected similarity fixture address")
		}
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	defer transport.CloseIdleConnections()
	sc, policy, err := application.BuildSimilarityConfig(cfg)
	must(err)
	sc.HTTPClient = &http.Client{Transport: transport}
	provider, err := similarity.New(sc)
	must(err)
	t.Setenv(cfg.Similarity.APIKeyEnv, "fixture-only")
	evidence, err := application.NewSimilarityExecutor(application.SimilarityExecutorConfig{Generation: generation, Provider: provider, Policy: policy, Limit: cfg.Similarity.Limit, RetryPolicy: retry, CostUpperBoundMicroUSD: cfg.Workflow.SimilarityCostUpperBoundMicroUSD, WorkflowRevision: cfg.Workflow.Revision})
	must(err)
	effective, err := cfg.Effective()
	must(err)
	service, err := application.NewSlice2RunService(application.Slice2RunServiceConfig{Generation: generation, Similarity: evidence, Reviews: store, ActiveTimeInterval: cfg.Runtime.AccountingHeartbeat, EffectiveConfigJSON: effective, SolutionSandbox: &base})
	must(err)
	seed := int64(202609101831)
	request := domain.RunRequest{SchemaVersion: domain.RequestSchemaV1, Mode: domain.RequestModeManual, Brief: "Create a small ordinary programming contest problem about an undirected unweighted graph. Prefer a clear tractable specification and an independent brute-force oracle. Keep generated formal cases modest for this integration test; do not require special judging.", Language: "en", Difficulty: "medium", SolutionLanguage: "cpp", TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, Seed: &seed, VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: domain.BudgetLimits{MaxLLMCalls: 8, MaxSimilarityCalls: 2, MaxLLMInputTokens: 1000000, MaxLLMOutputTokens: 262144, MaxLLMCostMicroUSD: 800000, MaxSimilarityCostMicroUSD: 200000, MaxSandboxCreates: 256, MaxArtifactBytes: 256 << 20, MaxPackageBytes: 64 << 20, MaxActiveTimeMilliseconds: 1200000}}
	must(request.Validate())
	writeJSON := func(name string, v any) {
		t.Helper()
		b, e := json.MarshalIndent(v, "", "  ")
		must(e)
		must(os.WriteFile(filepath.Join(root, name), b, 0600))
	}
	writeJSON("request.json", request)
	done := make(chan struct{})
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		tick := time.NewTicker(3 * time.Second)
		defer tick.Stop()
		last := ""
		for {
			select {
			case <-done:
				return
			case <-tick.C:
				runs, e := store.ListRuns(ctx, domain.RunFilter{Limit: 1})
				if e == nil && len(runs) == 1 {
					s := string(runs[0].RunID) + " " + string(runs[0].CurrentStage) + " " + string(runs[0].State)
					if s != last {
						t.Log(s)
						last = s
					}
				}
			}
		}
	}()
	result, runErr := service.Generate(ctx, request)
	close(done)
	<-progressDone
	writeJSON("result.json", result)
	if result.RunID != "" {
		budget, e := store.BudgetSnapshot(ctx, result.RunID)
		must(e)
		writeJSON("budget.json", budget)
		events, e := store.Events(ctx, result.RunID, 0)
		must(e)
		writeJSON("events.json", events)
	}
	t.Logf("result: state=%s stage=%s run=%s similarity_fixture_requests=%d", result.State, result.CurrentStage, result.RunID, searches.Load())
	if runErr != nil {
		t.Fatalf("live execution failed: %v; private evidence: %s", runErr, root)
	}
	if result.State != domain.RunReady {
		t.Fatalf("live run stopped at %s/%s; private evidence: %s", result.State, result.CurrentStage, root)
	}
	archive, record, err := service.ReadPackageArchive(ctx, result.RunID)
	must(err)
	writeJSON("package-record.json", record)
	must(app.Close())
	destination := filepath.Join(root, "problem.zip")
	command := exec.CommandContext(ctx, binary, "--config", configPath, "run", "export", string(result.RunID), "--output", destination)
	// Export must reconstruct proof without either provider credential.
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, cfg.LLM.APIKeyEnv+"=") && !strings.HasPrefix(entry, cfg.Similarity.APIKeyEnv+"=") {
			command.Env = append(command.Env, entry)
		}
	}
	output, err := command.CombinedOutput()
	must(os.WriteFile(filepath.Join(root, "export.json"), output, 0600))
	must(err)
	exported, err := os.ReadFile(destination)
	must(err)
	if domain.SumBytes(exported) != domain.SumBytes(archive) {
		t.Fatal("CLI export differs from committed archive")
	}
	_, err = packageprobe.ReadArchive(ctx, exported)
	must(err)
	revalidateExportedPackageInDocker(t, ctx, base, exported)
	writeJSON("acceptance.json", map[string]any{"passed": true, "model": modelName, "similarity": "local TLS fixture", "docker": true, "export_recompiled": true, "run_id": result.RunID})
}
