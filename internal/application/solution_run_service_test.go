package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"testing"
	"time"

	docker "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
	"cpgen/internal/workflow"
)

func TestSolutionRunServiceRoutesAcceptanceThroughRealVerification(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	base := newDockerSandboxTestConfig(t, ctx)
	for _, mode := range []string{"pass", "reject", "compile_error"} {
		t.Run(mode, func(t *testing.T) {
			var response []http.HandlerFunc
			if mode == "reject" {
				response = append(response, func(w http.ResponseWriter, _ *http.Request) {
					_, _ = w.Write([]byte(`{"provider_identity":"fixture","hits":[{"source":"fixture","external_id":"one","score":0.99}],"usage":{"input_tokens":2,"output_tokens":3,"cost_micro_usd":11}}`))
				})
			}
			f, _ := newSolutionExecutorFixtureWithOutputs(t, false, solutionDockerOutputs(t, mode), response...)
			generationConfig := f.executorConfig
			generationConfig.Clock = clock.Real{}
			generation, err := application.NewGenerationExecutor(generationConfig)
			if err != nil {
				t.Fatal(err)
			}
			similarityConfig := f.config
			similarityConfig.Generation = generation
			evidence, err := application.NewSimilarityExecutor(similarityConfig)
			if err != nil {
				t.Fatal(err)
			}
			solution, err := application.NewSolutionExecutor(evidence)
			if err != nil {
				t.Fatal(err)
			}
			_, configJSON, err := f.store.RunViewDocuments(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			reconciler, err := docker.NewSandboxReconciler(docker.SandboxReconcilerOptions{Engine: base.Engine, Store: f.store, EngineIdentityDigest: base.EngineIdentity, CleanupTimeout: base.Limits.CleanupTimeout})
			if err != nil {
				t.Fatal(err)
			}
			newService := func() *application.LocalRunService {
				service, err := application.NewSlice2RunService(application.Slice2RunServiceConfig{Generation: generation, Similarity: evidence, Reviews: f.store, ActiveTimeInterval: 100 * time.Millisecond, Reconciler: reconciler, EffectiveConfigJSON: configJSON, SolutionSandbox: &base})
				if err != nil {
					t.Fatal(err)
				}
				return service
			}
			request := domain.RunRequest(f.snapshot.Request)
			seed := f.snapshot.EffectiveSeed
			request.Seed = &seed
			result, err := newService().Generate(ctx, request)
			if err != nil || result.State != domain.RunNeedsReview || result.ActiveStartedAt != nil || result.WorkflowRevision != workflow.SolutionWorkflowRevision {
				t.Fatalf("run=%+v %v", result, err)
			}
			wantStage, wantHTTP, wantContainers := domain.StageName("solution_checkpoint"), int32(3), int64(16)
			if mode == "reject" {
				wantStage, wantHTTP, wantContainers = "similarity_decision", 2, 0
			}
			if mode == "compile_error" {
				wantContainers = 3
			}
			if result.CurrentStage != wantStage || f.httpCalls.Load() != wantHTTP || f.sends.Load() != 1 {
				t.Fatalf("routing/dispatch differs: %+v LLM=%d Similarity=%d", result, f.httpCalls.Load(), f.sends.Load())
			}
			budget, err := f.store.BudgetSnapshot(ctx, result.RunID)
			if err != nil || budget.Remaining[domain.BudgetDockerContainerCreates] != 100-wantContainers {
				t.Fatalf("container usage=%+v %v", budget, err)
			}
			if mode != "reject" {
				report, err := solution.ReadVerification(ctx, result.RunID, base)
				if err != nil || report.Passed != (mode == "pass") {
					t.Fatalf("report=%+v %v", report, err)
				}
			}
			resumed, err := newService().Resume(ctx, result.RunID)
			if err != nil || resumed.Version != result.Version || f.httpCalls.Load() != wantHTTP || f.sends.Load() != 1 {
				t.Fatalf("review resume restarted work: %+v %v", resumed, err)
			}
			t.Logf("full service route: %s -> %s, LLM=%d, containers=%d", mode, result.CurrentStage, wantHTTP, wantContainers)
		})
	}
}

func TestSolutionRunServiceCancelsInterruptedVerificationWithoutRedispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	base := newDockerSandboxTestConfig(t, ctx)
	for _, boundary := range []string{"unsealed_source", "compile_receipt"} {
		t.Run(boundary, func(t *testing.T) {
			f, executor := newSolutionExecutorFixtureWithOutputs(t, false, solutionDockerOutputs(t, "pass"))
			_, content, view := beginCommittedSolutionVerification(t, f, executor)
			if boundary == "unsealed_source" {
				identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "solution_verify", AttemptID: view.AttemptID(), SandboxExecutionID: "sandbox_000000000000000000000000000000da", LogicalOperationID: "deterministic-source", Kind: domain.CallSandboxCompile, ScopeDigest: content.ContentDigest, ExpectedRunVersion: view.Version()}
				decl := port.ArtifactDeclaration{Role: domain.ArtifactSource, MediaType: "text/plain", LogicalPath: "source/main.cpp", MaxBytes: 4096, Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "solution"}}
				raw, err := json.Marshal(sandboxArtifactCrashConfig{f.path, f.blobRoot, "partial_write", identity, decl, f.clock.Now()})
				if err != nil {
					t.Fatal(err)
				}
				command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSandboxArtifactCrashHelper$")
				command.Env = append(os.Environ(), "CPGEN_SANDBOX_ARTIFACT_CRASH_FIXTURE="+string(raw))
				output, err := command.CombinedOutput()
				var exited *exec.ExitError
				if !errors.As(err, &exited) || exited.ExitCode() != 73 {
					t.Fatalf("crash: %v %s", err, output)
				}
			} else {
				gap := &sandboxReceiptGapStore{Store: f.store, kind: domain.CallSandboxCompile}
				_, err := executor.VerifyDraft(ctx, view, func(_ context.Context, identity port.SandboxAuthorizationIdentity) (application.SolutionSandbox, toolchain.Lock, error) {
					config := base
					config.Store, config.Blobs, config.Clock, config.Identity = gap, f.executorConfig.Blobs, clock.Real{}, identity
					sandbox, err := application.NewDockerSandboxSession(config)
					return sandbox, base.Lock, err
				})
				if !errors.Is(err, errInjectedSandboxReceiptGap) {
					t.Fatalf("compile receipt interruption missing: %v", err)
				}
			}
			_, raw, err := f.store.RunViewDocuments(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			service, err := application.NewSlice2RunService(application.Slice2RunServiceConfig{Generation: f.executor, Similarity: f.service, Reviews: f.store, ActiveTimeInterval: time.Second, EffectiveConfigJSON: raw, SolutionSandbox: &base})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.runGuard.Close(); err != nil {
				t.Fatal(err)
			}
			f.clock.mu.Lock()
			f.clock.now = time.Now().UTC()
			f.clock.mu.Unlock()
			current, err := f.store.GetRun(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.Cancel(ctx, domain.CancelRequest{ID: domain.ControlRequestID(coordinatorID("control", boundary)), RunID: f.runID, ExpectedRunVersion: current.Version, At: f.clock.Now(), Reason: "cancel interrupted verification", IdempotencyKey: coordinatorID("cancel", boundary)})
			if err != nil || result.State != domain.RunCancelled || result.ActiveStartedAt != nil {
				t.Fatalf("cancel=%+v %v", result, err)
			}
			calls, err := f.store.ReadAttemptSandboxCalls(ctx, f.runID, "solution_verify", view.AttemptID())
			if err != nil {
				t.Fatal(err)
			}
			for _, call := range calls {
				if call.State != domain.CallRecordTerminal {
					t.Fatalf("cancel retained unsettled call: %+v", call)
				}
			}
			budget, err := f.store.BudgetSnapshot(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			wantContainers := int64(100)
			if boundary == "compile_receipt" {
				wantContainers = 96
			}
			if budget.Remaining[domain.BudgetDockerContainerCreates] != wantContainers || f.httpCalls.Load() != 3 || f.sends.Load() != 1 {
				t.Fatalf("cancel dispatched work or changed create charges: %+v", budget)
			}
		})
	}
}
