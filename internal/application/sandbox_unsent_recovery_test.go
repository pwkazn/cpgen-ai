package application_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sandboxexec "cpgen/internal/adapter/sandbox"
	docker "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/toolchain"
	"cpgen/internal/watchdog"
	"cpgen/internal/workflow"
)

var errWatchdogPreparation = errors.New("injected watchdog preparation failure before execution persistence")

type failingPreparationWatchdog struct {
	docker.WatchdogController
	fail bool
}

func (w *failingPreparationWatchdog) TokenDigest() domain.Digest {
	if w.WatchdogController == nil {
		return domain.SumBytes([]byte("unsent recovery watchdog"))
	}
	return w.WatchdogController.TokenDigest()
}

func (w *failingPreparationWatchdog) Prepare(ctx context.Context, record watchdog.ControlRecord) (docker.PreparedWatchdog, error) {
	if w.fail {
		return nil, errWatchdogPreparation
	}
	return w.WatchdogController.(docker.WatchdogPreparer).Prepare(ctx, record)
}

// A nil embedded Engine makes any unexpected Docker I/O fail the ordinary test.
type noSendEngine struct{ docker.Engine }

func TestSandboxUnsentVerificationResume(t *testing.T) {
	runSandboxUnsentVerificationResume(t, false)
}

func TestDockerSandboxUnsentVerificationResumeToReady(t *testing.T) {
	runSandboxUnsentVerificationResume(t, true)
}

func runSandboxUnsentVerificationResume(t *testing.T, realDocker bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var base sandboxexec.Config
	if realDocker {
		base = newDockerSandboxTestConfig(t, ctx)
	} else {
		file, err := os.Open(filepath.Join("..", "..", "config", "toolchains", "docker-v1.lock.json"))
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
		base = sandboxexec.Config{Engine: noSendEngine{}, Lock: lock, EngineIdentity: domain.SumBytes([]byte("engine")), Config: docker.Config{EngineEndpoint: endpoint, APIVersion: docker.RequiredAPIVersion, BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID), ExecutionProtocol: docker.ExecutionProtocolDockerDirectV2}, Limits: docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: 15 * time.Second}}
	}
	wd := &failingPreparationWatchdog{WatchdogController: base.Watchdog, fail: true}
	base.Watchdog = wd
	outputs := dataDockerOutputs(t, "pass")
	sequence := []string{"idea.draft", "statement.draft", "solution.draft", "data.draft"}
	var calls atomic.Int32
	f := similarityExecutorFixtureFromGeneration(t, newGenerationExecutorFixtureWithWorkflow(t, 4, false, workflow.RetryingGenerationRevision, 2, func(w http.ResponseWriter, _ *http.Request) {
		index := int(calls.Add(1)) - 1
		if index >= len(sequence) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(outputs[sequence[index]])}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	}))
	g := f.executorConfig
	g.Clock = clock.Real{}
	generation, err := application.NewGenerationExecutor(g)
	if err != nil {
		t.Fatal(err)
	}
	config := f.config
	config.Generation, config.WorkflowRevision = generation, workflow.RetryingGenerationRevision
	evidence, err := application.NewSimilarityExecutor(config)
	if err != nil {
		t.Fatal(err)
	}
	_, frozen, err := f.store.RunViewDocuments(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	newService := func() *application.LocalRunService {
		service, err := application.NewGenerationRunService(application.GenerationRunConfig{Store: f.store, Blobs: g.Blobs, Clock: g.Clock, Locks: g.Locks, Generation: generation, Similarity: evidence, Reviews: f.store, EffectiveConfigJSON: frozen, SolutionSandbox: &base})
		if err != nil {
			t.Fatal(err)
		}
		return service
	}
	request := domain.RunRequest(f.snapshot.Request)
	request.BudgetLimits.MaxSandboxCreates = 256
	result, err := newService().Generate(ctx, request)
	if !errors.Is(err, errWatchdogPreparation) {
		t.Fatalf("initial failure: %+v %v", result, err)
	}
	if strings.Contains(err.Error(), "expected sandbox execution version must be positive") {
		t.Fatalf("cleanup tried to finish an execution that was never persisted: %v", err)
	}
	old, err := f.store.CurrentStageAttempt(ctx, result.RunID, "solution_verify")
	if err != nil {
		t.Fatal(err)
	}
	oldCalls, err := f.store.ReadAttemptSandboxCalls(ctx, result.RunID, old.StageName, old.AttemptID)
	if err != nil || len(oldCalls) == 0 {
		t.Fatalf("missing failure calls: %v", err)
	}
	for _, call := range oldCalls {
		if call.State != domain.CallRecordTerminal {
			t.Fatalf("unsettled failure: %+v", call)
		}
	}
	// Repeating resume while the environment is still broken must reach a new,
	// separately budgeted attempt, without repeating upstream generation.
	_, err = newService().Resume(ctx, result.RunID)
	if !errors.Is(err, errWatchdogPreparation) {
		t.Fatalf("resume cannot retry unsent verification: %v", err)
	}
	next, err := f.store.CurrentStageAttempt(ctx, result.RunID, "solution_verify")
	if err != nil || next.Ordinal != old.Ordinal+1 || next.AttemptID == old.AttemptID || next.InputDigest != old.InputDigest {
		t.Fatalf("new verification attempt: %+v %v", next, err)
	}
	retained, err := f.store.ReadAttemptSandboxCalls(ctx, result.RunID, old.StageName, old.AttemptID)
	if err != nil || !reflect.DeepEqual(retained, oldCalls) {
		t.Fatalf("original calls changed: %v", err)
	}
	budget, err := f.store.BudgetSnapshot(ctx, result.RunID)
	if err != nil || budget.Remaining[domain.BudgetDockerContainerCreates] != 256 || calls.Load() != 3 || f.sends.Load() != 1 {
		t.Fatalf("unsent recovery changed charges or upstream sends: %+v %v LLM=%d", budget, err, calls.Load())
	}
	audit, err := sql.Open("sqlite", f.path)
	if err != nil {
		t.Fatal(err)
	}
	defer audit.Close()
	assertCount := func(query string, want int) {
		t.Helper()
		var got int
		if err := audit.QueryRowContext(ctx, query, result.RunID).Scan(&got); err != nil || got != want {
			t.Fatalf("audit count=%d want=%d: %s: %v", got, want, query, err)
		}
	}
	assertCount(`SELECT count(*) FROM sandbox_executions WHERE run_id=?`, 0)
	assertCount(`SELECT count(*) FROM budget_reservations WHERE run_id=? AND state='RESERVED'`, 0)
	assertCount(`SELECT count(*) FROM stage_attempts WHERE run_id=? AND stage_name='solution_verify' AND state='INTERRUPTED'`, 1)
	if !realDocker {
		current, err := f.store.GetRun(ctx, result.RunID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.store.RequestCancel(ctx, domain.CancelRequest{ID: domain.ControlRequestID(coordinatorID("control", "unsent cancel")), RunID: result.RunID, ExpectedRunVersion: current.Version, Reason: "cancel before recovery admission", IdempotencyKey: coordinatorID("cancel", "unsent cancel"), At: time.Now().UTC()})
		if err != nil {
			t.Fatal(err)
		}
		cancelled, err := newService().Resume(ctx, result.RunID)
		if err != nil || cancelled.State != domain.RunCancelled || calls.Load() != 3 || f.sends.Load() != 1 {
			t.Fatalf("pending cancellation admitted new work: %+v %v", cancelled, err)
		}
		assertCount(`SELECT count(*) FROM stage_attempts WHERE run_id=? AND stage_name='solution_verify'`, 2)
		again, err := newService().Resume(ctx, result.RunID)
		if err != nil || again.Version != cancelled.Version {
			t.Fatalf("CANCELLED resume changed the run: %+v %v", again, err)
		}
		return
	}
	wd.fail = false
	ready, err := newService().Resume(ctx, result.RunID)
	if err != nil || ready.State != domain.RunReady || calls.Load() != 4 || f.sends.Load() != 1 {
		t.Fatalf("recovery to READY: %+v %v LLM=%d", ready, err, calls.Load())
	}
	finished, err := f.store.CurrentStageAttempt(ctx, result.RunID, "solution_verify")
	if err != nil || finished.Ordinal != 3 || finished.State != domain.StageAttemptSucceeded {
		t.Fatalf("completed verification: %+v %v", finished, err)
	}
	finalBudget, err := f.store.BudgetSnapshot(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	again, err := newService().Resume(ctx, result.RunID)
	after, budgetErr := f.store.BudgetSnapshot(ctx, result.RunID)
	if err != nil || budgetErr != nil || again.Version != ready.Version || !reflect.DeepEqual(after, finalBudget) || calls.Load() != 4 || f.sends.Load() != 1 {
		t.Fatalf("READY resume was not idempotent: %+v %v %v", again, err, budgetErr)
	}
	assertCount(`SELECT count(*) FROM sandbox_executions WHERE run_id=? AND state!='CLEANED'`, 0)
	assertCount(`SELECT count(*) FROM call_records WHERE run_id=? AND state!='TERMINAL'`, 0)
	assertCount(`SELECT count(*) FROM physical_calls WHERE run_id=? AND state NOT IN ('COMPLETED','ABORTED_NO_DISPATCH')`, 0)
	assertCount(`SELECT count(*) FROM budget_reservations WHERE run_id=? AND state='RESERVED'`, 0)
	assertCount(`SELECT count(*) FROM stage_attempts WHERE run_id=? AND stage_name='solution_verify' AND state='INTERRUPTED'`, 2)
	t.Logf("run=%s READY after two pre-execution failures; solution_verify attempts=3, LLM=4, Similarity fixture=1; budget=%+v", ready.RunID, finalBudget)
}
