package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dockersandbox "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/cli"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestVersionJSON(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{"version", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	var output map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode version JSON: %v", err)
	}
	if output["schema_version"] != "cpgen.cli-version/v1" || output["version"] == "" || output["go_version"] == "" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestStatefulConfigAndGenerateCommandsUseExplicitConfig(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	configPath := filepath.Join(root, "cpgen.yaml")
	requestPath := filepath.Join(root, "request.yaml")
	if err := os.WriteFile(configPath, []byte("storage:\n  state_root: "+stateRoot+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	request := "schema_version: cpgen.request/v1\nmode: offline\nbrief: demo\nlanguage: cpp\ndifficulty: easy\ntime_limit_milliseconds: 1000\nmemory_limit_megabytes: 64\nsolution_language: go\nverification_profile: default\nbudget_limits:\n  max_active_time_milliseconds: 5000\n"
	if err := os.WriteFile(requestPath, []byte(request), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := cli.Run([]string{"--config", configPath, "config", "validate"}, &stdout, &stderr); code != 0 {
		t.Fatalf("config validate code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var envelope map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope["schema_version"] != "cpgen.cli/v1" || envelope["status"] != "VALID" {
		t.Fatalf("unexpected config envelope: %#v", envelope)
	}
	stdout.Reset()
	stderr.Reset()
	if code := cli.Run([]string{"--config", configPath, "generate", "--request", requestPath}, &stdout, &stderr); code != 6 {
		t.Fatalf("generate code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "NEEDS_REVIEW") || stderr.Len() != 0 {
		t.Fatalf("unexpected generate output stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestReviewShowReturnsNotFoundForMissingRun(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "cpgen.yaml")
	stateRoot := filepath.Join(root, "state")
	if err := os.WriteFile(configPath, []byte("storage:\n  state_root: "+stateRoot+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{
		"--config", configPath,
		"review", "show", "run_00000000000000000000000000000000",
	}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var envelope struct {
		Status string `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; stdout=%q", err, stdout.String())
	}
	if envelope.Status != "ERROR" || envelope.Error.Code != "not_found" {
		t.Fatalf("unexpected error envelope: %#v", envelope)
	}
}

func TestReviewRetryBudgetPatchUsesSharedPersistentApprovalPath(t *testing.T) {
	root := t.TempDir()
	stateRoot := filepath.Join(root, "state")
	if err := os.WriteFile(filepath.Join(root, "cpgen.yaml"), []byte("storage:\n  state_root: "+filepath.ToSlash(stateRoot)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.ToSlash(stateRoot) + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	effective, err := cfg.EffectiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(effective.Paths.Locks, 0700); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), sqlite.Config{Path: effective.Paths.Database, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	runID := domain.RunID("run_000000000000000000000000000000a1")
	configJSON, err := cfg.Effective()
	if err != nil {
		t.Fatal(err)
	}
	configDigest := cfg.EffectiveDigest()
	limits := domain.BudgetLimits{MaxSimilarityCostMicroUSD: 7000, MaxActiveTimeMilliseconds: 30000}
	submitted := domain.RunRequest{SchemaVersion: domain.RequestSchemaV1, Mode: "manual", Brief: "CLI review test", Tags: []string{"graphs"}, NormalizedTags: []string{"graphs"}, Language: "en", Difficulty: "easy", TimeLimitMilliseconds: 1000, MemoryLimitMegabytes: 64, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: limits}
	raw, err := json.Marshal(submitted)
	if err != nil {
		t.Fatal(err)
	}
	var canonicalValue any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&canonicalValue); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(canonicalValue)
	if err != nil {
		t.Fatal(err)
	}
	createdAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	create := domain.CreateRunRequest{RunID: runID, SubmittedRequestJSON: raw, SubmittedRequestDigest: domain.SumBytes(raw), EffectiveSeed: 7,
		RedactedEffectiveConfigJSON: configJSON, RedactedEffectiveConfigDigest: configDigest, WorkflowRevision: workflow.FakeRevision, SchemaVersion: domain.RequestSchemaV1,
		WorkflowDigest: domain.SumBytes([]byte(workflow.FakeRevision)), BudgetLimits: limits, StageSequence: []domain.StageName{"prepare", "exercise", "checkpoint"}, CreatedAt: createdAt,
		IdempotencyKey: "create_000000000000000000000000000000a1"}
	if _, err := store.CreateRun(context.Background(), create); err != nil {
		t.Fatalf("CreateRun fixture: %v", err)
	}
	input := domain.SumBytes([]byte("CLI review input"))
	attemptID := domain.AttemptID("attempt_000000000000000000000000000000a1")
	if _, err := store.BeginStage(context.Background(), domain.BeginStageCommand{RunID: runID, ExpectedRunVersion: 1, StageName: "prepare", AttemptID: attemptID, InputDigest: input, IdempotencyKey: "begin_000000000000000000000000000000a1", At: createdAt.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishStage(context.Background(), domain.FinishStageCommand{RunID: runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: attemptID, AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview, ReviewEvidenceDigest: &input, ReviewPolicyDigest: &configDigest, IdempotencyKey: "finish_000000000000000000000000000000a1", At: createdAt.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}

	patchPath := filepath.Join(root, "budget-patch.json")
	patchJSON, _ := json.Marshal(domain.BudgetLimits{MaxSimilarityCostMicroUSD: 100000})
	if err := os.WriteFile(patchPath, patchJSON, 0600); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{"--config", filepath.Join(root, "cpgen.yaml"), "review", "retry", string(runID), "--reviewer", "cli-test", "--reason", "resume with approved headroom", "--budget-patch", patchPath}, &stdout, &stderr)
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("review retry code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var response struct {
		Status string `json:"status"`
		Data   struct {
			BudgetIncrease domain.BudgetLimits `json:"budget_increase"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Status != "PENDING" || response.Data.BudgetIncrease.MaxSimilarityCostMicroUSD != 100000 {
		t.Fatalf("CLI did not persist the exact budget patch: %+v", response)
	}
	store, err = sqlite.Open(context.Background(), sqlite.Config{Path: effective.Paths.Database, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingReview(context.Background(), runID)
	if err != nil || pending == nil || pending.BudgetIncrease.MaxSimilarityCostMicroUSD != 100000 {
		t.Fatalf("persisted CLI review = %+v, %v", pending, err)
	}
	account := readCLIBudgetAccount(t, store, runID)
	if account != 7000 {
		t.Fatalf("review creation applied its budget before explicit resume: %d", account)
	}
	if _, err := store.GetRun(context.Background(), runID); err != nil {
		t.Fatalf("run projection invalid before explicit resume: %v (cause: %v)", err, errors.Unwrap(err))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = cli.Run([]string{"--config", filepath.Join(root, "cpgen.yaml"), "run", "resume", string(runID)}, &stdout, &stderr)
	if code != 0 && code != 6 {
		t.Fatalf("run resume code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	store, err = sqlite.Open(context.Background(), sqlite.Config{Path: effective.Paths.Database, BusyTimeout: time.Second, MaxReaders: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if account := readCLIBudgetAccount(t, store, runID); account != 107000 {
		t.Fatalf("CLI resume did not apply approved budget: %d", account)
	}
}

func readCLIBudgetAccount(t *testing.T, store *sqlite.Store, runID domain.RunID) int64 {
	t.Helper()
	budget, err := store.BudgetSnapshot(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	return budget.Limits.MaxSimilarityCostMicroUSD
}

func TestRunEventsReturnsNotFoundForMissingRun(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "cpgen.yaml")
	stateRoot := filepath.Join(root, "state")
	if err := os.WriteFile(configPath, []byte("storage:\n  state_root: "+stateRoot+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{
		"--config", configPath,
		"run", "events", "run_00000000000000000000000000000000",
	}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit code = %d, stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	var envelope struct {
		Status string `json:"status"`
		Error  struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode error envelope: %v; stdout=%q", err, stdout.String())
	}
	if envelope.Status != "ERROR" || envelope.Error.Code != "not_found" {
		t.Fatalf("unexpected error envelope: %#v", envelope)
	}
}

func TestRunListRejectsInvalidLimitWithArgumentExitCode(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "cpgen.yaml")
	if err := os.WriteFile(configPath, []byte("storage:\n  state_root: "+filepath.Join(root, "state")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []string{"-1", "1001"} {
		var stdout, stderr bytes.Buffer
		code := cli.Run([]string{"--config", configPath, "run", "list", "--limit", limit}, &stdout, &stderr)
		if code != 2 {
			t.Fatalf("limit %s exit code = %d, stdout=%q stderr=%q", limit, code, stdout.String(), stderr.String())
		}
		if !strings.Contains(stdout.String(), "invalid_argument") {
			t.Fatalf("limit %s missing invalid_argument: %q", limit, stdout.String())
		}
	}
}

func TestDoctorJSONUsesExplicitFlagsAndReportsHealthy(t *testing.T) {
	t.Parallel()
	digest := domain.SumBytes([]byte("image"))
	var gotConfig dockersandbox.Config
	dependencies := cli.Dependencies{
		GOOS: "windows",
		CheckDocker: func(_ context.Context, config dockersandbox.Config) (dockersandbox.StaticReport, error) {
			gotConfig = config
			return dockersandbox.StaticReport{EngineIdentityDigest: domain.SumBytes([]byte("engine"))}, nil
		},
	}
	args := []string{
		"doctor", "--json",
		"--engine-endpoint", "npipe:////./pipe/docker_engine",
		"--api-version", "1.55",
		"--builder-image", string(digest),
		"--runtime-image", string(digest),
		"--transfer-image", string(digest),
		"--execution-protocol", "docker-direct-v2",
	}
	var stdout, stderr bytes.Buffer
	code := cli.RunWithDependencies(args, &stdout, &stderr, dependencies)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	var output struct {
		SchemaVersion        string        `json:"schema_version"`
		Status               string        `json:"status"`
		EngineIdentityDigest domain.Digest `json:"engine_identity_digest"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.SchemaVersion != "cpgen.doctor/v1" || output.Status != "HEALTHY" || output.EngineIdentityDigest == "" {
		t.Fatalf("unexpected doctor output: %#v", output)
	}
	if gotConfig.EngineEndpoint != "npipe:////./pipe/docker_engine" || gotConfig.ExecutionProtocol != "docker-direct-v2" {
		t.Fatalf("unexpected doctor config: %#v", gotConfig)
	}
}

func TestDoctorRejectsMissingFlagsBeforeDispatch(t *testing.T) {
	t.Parallel()
	called := false
	var stdout, stderr bytes.Buffer
	code := cli.RunWithDependencies([]string{"doctor", "--json"}, &stdout, &stderr, cli.Dependencies{
		GOOS: "windows",
		CheckDocker: func(context.Context, dockersandbox.Config) (dockersandbox.StaticReport, error) {
			called = true
			return dockersandbox.StaticReport{}, nil
		},
	})
	if code != 2 || called {
		t.Fatalf("exit code = %d, called = %v, stderr = %q", code, called, stderr.String())
	}
}

func TestDoctorReturnsBlockedJSONForUnavailableEngine(t *testing.T) {
	t.Parallel()
	digest := domain.SumBytes([]byte("image"))
	args := []string{
		"doctor", "--json",
		"--engine-endpoint", "npipe:////./pipe/docker_engine",
		"--api-version", "1.55",
		"--builder-image", string(digest),
		"--runtime-image", string(digest),
		"--transfer-image", string(digest),
		"--execution-protocol", "docker-direct-v2",
	}
	var stdout, stderr bytes.Buffer
	code := cli.RunWithDependencies(args, &stdout, &stderr, cli.Dependencies{
		GOOS: "windows",
		CheckDocker: func(context.Context, dockersandbox.Config) (dockersandbox.StaticReport, error) {
			return dockersandbox.StaticReport{}, &dockersandbox.CheckError{
				Failure: domain.PortFailure{Code: domain.FailureUnavailable, Class: domain.FailureBlocked},
				Cause:   errors.New("dial refused"),
			}
		},
	})
	if code != 10 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output["status"] != "BLOCKED" || output["failure_code"] != "unavailable" {
		t.Fatalf("unexpected blocked output: %#v", output)
	}
}

func TestUnknownCommandFailsWithoutExposingProbe(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{"probe"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
}

func TestWatchdogHiddenCommandRequiresOnlyAbsoluteOwnerControlPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := ""
	control := filepath.Join(t.TempDir(), "control.json")
	code := cli.RunWithDependencies([]string{"sandbox-watchdog", "--control", control}, &stdout, &stderr, cli.Dependencies{
		RunWatchdog: func(_ context.Context, path string) error {
			called = path
			return nil
		},
	})
	if code != 0 || called != control || stdout.Len() != 0 {
		t.Fatalf("code=%d called=%q stdout=%q stderr=%q", code, called, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{
		{"sandbox-watchdog", "--control", "relative.json"},
		{"sandbox-watchdog", "--control", control, "extra"},
		{"sandbox-watchdog", "--control", control, "--engine-endpoint", "tcp://attacker"},
	} {
		called = ""
		stdout.Reset()
		stderr.Reset()
		if code := cli.RunWithDependencies(args, &stdout, &stderr, cli.Dependencies{RunWatchdog: func(context.Context, string) error {
			called = "called"
			return nil
		}}); code != 2 || called != "" {
			t.Fatalf("args=%#v code=%d called=%q", args, code, called)
		}
	}
	stdout.Reset()
	stderr.Reset()
	cli.Run([]string{"help"}, &stdout, &stderr)
	if strings.Contains(stdout.String(), "sandbox-watchdog") {
		t.Fatal("hidden watchdog command appeared in help")
	}
}
