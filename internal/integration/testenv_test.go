package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

// integrationEnvironment is intentionally assembled through the same config
// and application entry points used by the CLI.  Integration tests must not
// replace SQLite, the filesystem store, or the process lock with an in-memory
// fake.
type integrationEnvironment struct {
	root        string
	configPath  string
	requestPath string
	cfg         config.Config
}

func newIntegrationEnvironment(t *testing.T, scenario string) integrationEnvironment {
	t.Helper()
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	configPath := filepath.Join(root, "cpgen.yaml")
	requestPath := filepath.Join(root, "request.yaml")
	configText := fmt.Sprintf("storage:\n  state_root: %q\nsqlite:\n  busy_timeout: 2s\n  max_readers: 4\nruntime:\n  lock_poll_interval: 10ms\n  control_poll_interval: 25ms\n  accounting_heartbeat: 25ms\n  cleanup_wait: 2s\nfake_workflow:\n  scenario: %s\n", stateRoot, scenario)
	if err := os.WriteFile(configPath, []byte(configText), 0o600); err != nil {
		t.Fatal(err)
	}
	requestText := "schema_version: cpgen.request/v1\nmode: offline\nbrief: integration A+B\nlanguage: cpp\ndifficulty: easy\ntime_limit_milliseconds: 1000\nmemory_limit_megabytes: 64\nsolution_language: go\nverification_profile: default\nbudget_limits:\n  max_active_time_milliseconds: 5000\n"
	if err := os.WriteFile(requestPath, []byte(requestText), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return integrationEnvironment{root: root, configPath: configPath, requestPath: requestPath, cfg: cfg}
}

func openIntegrationApp(t *testing.T, env integrationEnvironment) *application.Application {
	t.Helper()
	app, err := application.Bootstrap(context.Background(), env.cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

func integrationRequest() domain.RunRequest {
	return domain.RunRequest{
		SchemaVersion: "cpgen.request/v1", Mode: "offline", Brief: "integration A+B",
		Language: "cpp", Difficulty: "easy", TimeLimitMilliseconds: 1000,
		MemoryLimitMegabytes: 64, SolutionLanguage: "go", VerificationProfile: "default",
		BudgetLimits: domain.BudgetLimits{MaxSandboxCreates: 4, MaxArtifactBytes: 1 << 20, MaxActiveTimeMilliseconds: 5000},
	}
}

type helperProcess struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr bytes.Buffer
}

// TestSlice1IntegrationHelper is the only test entry point executed in a
// child process.  It is deliberately opt-in: invoking `go test` normally can
// never accidentally turn a test into a lock holder.
func TestSlice1IntegrationHelper(t *testing.T) {
	if os.Getenv("CPGEN_SLICE1_HELPER") != "1" {
		return
	}
	env, err := config.Load(os.Getenv("CPGEN_SLICE1_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	action := os.Getenv("CPGEN_SLICE1_HELPER_ACTION")
	runID := domain.RunID(os.Getenv("CPGEN_SLICE1_RUN_ID"))
	if err := runID.Validate(); err != nil {
		t.Fatal(err)
	}
	if action == "hold-lock" {
		manager, err := runlock.NewManager(env.Paths.Locks, runlock.Options{PollInterval: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		defer manager.Close()
		guard, err := manager.TryAcquireRun(runID, runlock.Exclusive)
		if err != nil {
			t.Fatal(err)
		}
		defer guard.Close()
		fmt.Fprintf(os.Stdout, "READY %s\n", action)
		select {}
	}

	app, err := application.Bootstrap(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()

	if strings.HasPrefix(action, "crash-") {
		if err := runBoundaryUntil(context.Background(), app, env, runID, strings.TrimPrefix(action, "crash-")); err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(os.Stdout, "READY %s\n", action)
		select {}
	}
	t.Fatalf("unknown integration helper action %q", action)
}

func runBoundaryUntil(ctx context.Context, app *application.Application, cfg config.Config, runID domain.RunID, boundary string) error {
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := integrationRequest()
	submitted, err := canonicalIntegrationJSON(request)
	if err != nil {
		return err
	}
	effective, err := cfg.Effective()
	if err != nil {
		return err
	}
	create := domain.CreateRunRequest{
		RunID: runID, SubmittedRequestJSON: submitted, SubmittedRequestDigest: domain.SumBytes(submitted),
		EffectiveSeed: int64(len(request.Brief)), RedactedEffectiveConfigJSON: effective,
		RedactedEffectiveConfigDigest: domain.SumBytes(effective), WorkflowRevision: workflow.Slice1WorkflowRevision,
		SchemaVersion: domain.SchemaVersion(request.SchemaVersion), WorkflowDigest: domain.SumBytes([]byte(workflow.Slice1WorkflowRevision)),
		BudgetLimits: request.BudgetLimits, StageSequence: []domain.StageName{"prepare", "exercise", "checkpoint"}, CreatedAt: now,
		IdempotencyKey: "control_00000000000000000000000000000001",
	}
	created, err := app.Runtime.CreateRun(ctx, create)
	if err != nil {
		return err
	}
	if boundary == "run_create" {
		return nil
	}
	input := domain.SumBytes([]byte("integration prepare input"))
	attemptID := domain.AttemptID("attempt_00000000000000000000000000000001")
	attempt, err := app.Runtime.BeginStage(ctx, domain.BeginStageCommand{RunID: runID, ExpectedRunVersion: created.Version, StageName: "prepare", AttemptID: attemptID, InputDigest: input, IdempotencyKey: "begin_00000000000000000000000000000001", At: now.Add(time.Microsecond)})
	if err != nil {
		return err
	}
	if boundary == "stage_begin" {
		return nil
	}

	// The following records model every metered physical durable boundary.
	// The external operation is intentionally absent: the process is killed
	// between these short, committed calls and the later resume must not mint
	// another logical or physical identity.
	ledger, ok := app.Runtime.(port.CallLedger)
	if !ok {
		return errors.New("bootstrap runtime does not expose call ledger")
	}
	policyDigest := domain.SumBytes([]byte("integration policy"))
	callRecordID := domain.CallRecordID("callrec_00000000000000000000000000000001")
	call, err := ledger.OpenCall(ctx, domain.OpenCallRequest{ID: callRecordID, RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, LogicalOperationID: "integration-sandbox", Kind: domain.CallSandboxRun, Provider: "local-test", RequestDigest: domain.SumBytes([]byte("integration request")), PolicyDigest: policyDigest, RetryPolicy: domain.RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, JitterSeedDigest: policyDigest}, IdempotencyKey: "open_00000000000000000000000000000001", At: now.Add(2 * time.Microsecond)})
	if err != nil {
		return err
	}
	physicalID := domain.AttemptCallID("call_00000000000000000000000000000001")
	reservationID := domain.ReservationID("res_00000000000000000000000000000001")
	prepared, err := ledger.PrepareCalls(ctx, domain.PrepareCallsRequest{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, CallRecordID: call.ID, PlanDigest: domain.SumBytes([]byte("integration plan")), Calls: []domain.PhysicalCallPlan{{ID: physicalID, Ordinal: 1, RetryGroup: "integration", RetryOrdinal: 1, Kind: domain.PhysicalDockerContainerCreate, Provider: "local-test", RequestDigest: domain.SumBytes([]byte("physical request")), IdempotencyKey: "physical_00000000000000000000000000000001", Reservations: []domain.ReservationPlan{{ID: reservationID, Dimension: domain.BudgetDockerContainerCreates, Subkey: "create", UpperBound: 1}}}}, IdempotencyKey: "prepare_00000000000000000000000000000001", At: now.Add(3 * time.Microsecond)})
	if err != nil {
		return err
	}
	if boundary == "budget_reserve" {
		return nil
	}
	grant, err := ledger.BeginDispatch(ctx, domain.BeginDispatchRequest{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, CallRecordID: call.ID, AttemptCallID: prepared.PhysicalCalls[0].ID, IdempotencyKey: "dispatch_00000000000000000000000000000001", At: now.Add(4 * time.Microsecond)})
	if err != nil {
		return err
	}
	if boundary == "dispatching" {
		return nil
	}
	if err := ledger.MarkSent(ctx, grant, now.Add(5*time.Microsecond)); err != nil {
		return err
	}
	if boundary == "sent" {
		return nil
	}
	responseDigest := domain.SumBytes([]byte("integration response"))
	if err := ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, CallRecordID: call.ID, AttemptCallID: physicalID, State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ProviderRequestID: "provider-integration-1", ResponseDigest: &responseDigest, Usage: []domain.ReservationUsage{{ReservationID: reservationID, Dimension: domain.BudgetDockerContainerCreates, Subkey: "create", Value: 1, Verified: true}}, IdempotencyKey: "complete_00000000000000000000000000000001", At: now.Add(6 * time.Microsecond)}); err != nil {
		return err
	}
	if boundary == "physical_completion" {
		return nil
	}
	trace, err := ledger.FinishCall(ctx, domain.FinishCallRequest{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, CallRecordID: call.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &physicalID, IdempotencyKey: "finish_call_00000000000000000000000000000001", At: now.Add(7 * time.Microsecond)})
	if err != nil {
		return err
	}
	if err := trace.Validate(); err != nil {
		return err
	}
	// Blob and sandbox publication/cleanup boundaries are exercised by their
	// adapter suites. In the local Fake bootstrap they have no external call;
	// use the committed physical finish as the restart checkpoint while
	// retaining explicit names in the matrix in slice1_crash_test.go.
	if boundary != "stage_finish" && boundary != "blob_sealed" && boundary != "blob_published" && boundary != "blob_ready" && boundary != "occurrence_commit" && boundary != "sandbox_resource" {
		return fmt.Errorf("unknown durable boundary %q", boundary)
	}
	output := domain.SumBytes([]byte("integration prepare output"))
	_, err = app.Runtime.FinishStage(ctx, domain.FinishStageCommand{RunID: runID, ExpectedRunVersion: created.Version + 1, StageName: "prepare", AttemptID: attempt.AttemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, IdempotencyKey: "finish_00000000000000000000000000000001", At: now.Add(8 * time.Microsecond)})
	return err
}

func canonicalIntegrationJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

// startIntegrationHelper runs this package's own test binary.  The helper
// only uses exported application/run-lock APIs and emits a readiness record
// before it blocks, so tests never synchronize with an arbitrary sleep.
func startIntegrationHelper(t *testing.T, env integrationEnvironment, action string, runID domain.RunID) *helperProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSlice1IntegrationHelper$", "-test.v")
	cmd.Env = append(os.Environ(),
		"CPGEN_SLICE1_HELPER=1",
		"CPGEN_SLICE1_HELPER_ACTION="+action,
		"CPGEN_SLICE1_CONFIG="+env.configPath,
		"CPGEN_SLICE1_RUN_ID="+string(runID),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	helper := &helperProcess{cmd: cmd, stdout: stdout}
	cmd.Stderr = &helper.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		reader := bufio.NewReader(stdout)
		var output []string
		for {
			line, readErr := reader.ReadString('\n')
			if strings.HasPrefix(line, "READY ") {
				ready <- nil
				return
			}
			if line != "" {
				output = append(output, line)
			}
			if readErr != nil {
				ready <- fmt.Errorf("%w (output=%q)", readErr, strings.Join(output, ""))
				return
			}
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("helper %s did not become ready: %v; stderr=%q", action, err, helper.stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("helper %s readiness timed out; stderr=%q", action, helper.stderr.String())
	}
	if cmd.ProcessState != nil {
		t.Fatalf("helper %s exited immediately after readiness", action)
	}
	return helper
}

func (h *helperProcess) kill(t *testing.T) {
	t.Helper()
	if h.cmd.ProcessState == nil {
		if err := h.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) && !strings.Contains(strings.ToLower(err.Error()), "access is denied") {
			t.Fatalf("kill helper: %v", err)
		}
	}
	if h.cmd.ProcessState == nil {
		if err := h.cmd.Wait(); err != nil && h.cmd.ProcessState == nil {
			t.Fatalf("wait helper: %v", err)
		}
	}
	_ = h.stdout.Close()
}

func runCLI(t *testing.T, args ...string) (int, []byte, []byte) {
	t.Helper()
	command := exec.Command("go", append([]string{"run", "./cmd/cpgen", "--config"}, args...)...)
	command.Dir = filepath.Join("..", "..")
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run cpgen: %v", err)
		}
	}
	return command.ProcessState.ExitCode(), stdout.Bytes(), stderr.Bytes()
}

func decodeEnvelope(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatalf("decode CLI envelope: %v; output=%q", err, data)
	}
	return envelope
}
