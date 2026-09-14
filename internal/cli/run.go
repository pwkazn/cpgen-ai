package cli

import (
	sandboxexec "cpgen/internal/adapter/sandbox"

	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	dockersandbox "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
	"go.yaml.in/yaml/v3"
)

var Version = "dev"

type versionOutput struct {
	SchemaVersion string `json:"schema_version"`
	Version       string `json:"version"`
	GoVersion     string `json:"go_version"`
}

type Dependencies struct {
	GOOS           string
	CheckDocker    func(context.Context, dockersandbox.Config) (dockersandbox.StaticReport, error)
	RunWatchdog    func(context.Context, string) error
	BootstrapLocal func(context.Context, config.Config) (*application.Application, error)
	Bootstrap      func(context.Context, config.Config) (*application.Application, error)
}

type doctorOutput struct {
	SchemaVersion        string                      `json:"schema_version"`
	Status               string                      `json:"status"`
	EngineIdentityDigest domain.Digest               `json:"engine_identity_digest,omitempty"`
	Checks               []string                    `json:"checks"`
	FailureCode          domain.PortFailureCode      `json:"failure_code,omitempty"`
	FailureClass         domain.FailureClass         `json:"failure_class,omitempty"`
	Message              string                      `json:"message,omitempty"`
	Report               *dockersandbox.StaticReport `json:"report,omitempty"`
}

const cliSchema = "cpgen.cli/v1"

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Field   string `json:"field,omitempty"`
}

type envelope struct {
	SchemaVersion string         `json:"schema_version"`
	Status        string         `json:"status"`
	Data          any            `json:"data,omitempty"`
	Error         *responseError `json:"error,omitempty"`
	RunVersion    int64          `json:"run_version,omitempty"`
}

func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithDependencies(args, stdout, stderr, Dependencies{GOOS: runtime.GOOS, CheckDocker: dockersandbox.CheckStatic, RunWatchdog: dockersandbox.RunWatchdogService, Bootstrap: application.Bootstrap})
}

func RunWithDependencies(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(args) == 0 {
		writeHelp(stdout)
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		writeHelp(stdout)
		return 0
	case "version":
		if len(args) == 2 && args[1] == "--json" {
			return encodeJSON(stdout, stderr, versionOutput{SchemaVersion: "cpgen.cli-version/v1", Version: Version, GoVersion: runtime.Version()}, 0)
		}
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: cpgen version [--json]")
			return 2
		}
		fmt.Fprintln(stdout, Version)
		return 0
	case "doctor":
		return runDoctor(args[1:], stdout, stderr, dependencies)
	case "sandbox-watchdog":
		return runSandboxWatchdog(args[1:], stderr, dependencies)
	}
	if args[0] != "--config" {
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		fmt.Fprintln(stderr, "run 'cpgen help' for usage")
		return 2
	}
	if len(args) < 3 || args[1] == "" || strings.HasPrefix(args[1], "-") {
		fmt.Fprintln(stderr, "usage: cpgen --config PATH <command> ...")
		return 2
	}
	if args[2] == "--config" {
		fmt.Fprintln(stderr, "duplicate --config")
		return 2
	}
	if args[2] == "doctor" || args[2] == "version" || args[2] == "help" || args[2] == "sandbox-watchdog" {
		fmt.Fprintln(stderr, "this command is config-independent and cannot use --config")
		return 2
	}
	configPath, err := canonicalPath(args[1])
	if err != nil {
		return writeStateError(stdout, stderr, 2, "config_path", err)
	}
	return runStateful(args[2:], configPath, stdout, stderr, dependencies)
}

func runStateful(args []string, configPath string, stdout, stderr io.Writer, dependencies Dependencies) int {
	cfg, err := config.Load(configPath)
	if err != nil {
		return writeStateError(stdout, stderr, 2, "config_invalid", err)
	}
	if len(args) == 0 {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("stateful command is required"))
	}
	if args[0] == "config" {
		return runConfigCommand(args[1:], cfg, stdout, stderr)
	}
	if args[0] != "run" && args[0] != "review" && args[0] != "generate" {
		return writeStateError(stdout, stderr, 2, "unknown_command", fmt.Errorf("unknown command %q", args[0]))
	}
	if code := validateCommandShape(args, stdout, stderr); code != 0 {
		return code
	}
	if cfg.Sandbox != nil && cfg.Workflow != nil && workflow.HasSolutionStages(cfg.Workflow.Revision) && shouldRestoreToolchainSnapshot(args) {
		cfg, err = restoreToolchainSnapshot(context.Background(), cfg, args, dependencies)
		if err != nil {
			return writeStateError(stdout, stderr, 9, "bootstrap_failed", err)
		}
	}
	bootstrap := dependencies.Bootstrap
	if bootstrap == nil {
		bootstrap = application.Bootstrap
	}
	if localReadCommand(args) {
		bootstrap = dependencies.BootstrapLocal
		if bootstrap == nil {
			bootstrap = application.BootstrapLocal
		}
	}
	app, err := bootstrap(context.Background(), cfg)
	if err != nil {
		return writeStateError(stdout, stderr, 9, "bootstrap_failed", err)
	}
	defer app.Close()
	switch args[0] {
	case "generate":
		return runGenerate(args[1:], app, stdout, stderr)
	case "run":
		return runRunCommand(args[1:], app, stdout, stderr)
	case "review":
		return runReviewCommand(args[1:], app, stdout, stderr)
	default:
		return writeStateError(stdout, stderr, 2, "unknown_command", fmt.Errorf("unknown command %q", args[0]))
	}
}

func shouldRestoreToolchainSnapshot(args []string) bool {
	if len(args) >= 2 && args[0] == "run" && (args[1] == "resume" || args[1] == "cancel") {
		return true
	}
	return len(args) >= 2 && args[0] == "review" && args[1] != "show" && args[1] != ""
}

type runDocumentsReader interface {
	RunViewDocuments(context.Context, domain.RunID) ([]byte, []byte, error)
}

func restoreToolchainSnapshot(ctx context.Context, cfg config.Config, args []string, dependencies Dependencies) (config.Config, error) {
	var runID domain.RunID
	var err error
	normalized := movePositionalToEnd(args[2:])
	for index := len(normalized) - 1; index >= 0; index-- {
		runID, err = parseRunID(normalized[index])
		if err == nil {
			break
		}
	}
	if runID == "" {
		return cfg, errors.New("run ID is required")
	}
	bootstrap := dependencies.BootstrapLocal
	if bootstrap == nil {
		bootstrap = application.BootstrapLocal
	}
	local, err := bootstrap(ctx, cfg)
	if err != nil {
		return cfg, err
	}
	defer local.Close()
	documents, ok := local.Runtime.(runDocumentsReader)
	if !ok {
		return cfg, errors.New("runtime store cannot read frozen configuration")
	}
	run, err := local.Runtime.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, sqlite.ErrNotFound) {
			return cfg, nil
		}
		return cfg, err
	}
	_, raw, err := documents.RunViewDocuments(ctx, runID)
	if err != nil {
		return cfg, err
	}
	frozen, err := config.DecodeEffective(raw)
	if err != nil {
		return cfg, err
	}
	if domain.SumBytes(raw) != run.ConfigDigest {
		return cfg, errors.New("persisted effective configuration differs from its run binding")
	}
	if cfg.Sandbox != nil && frozen.Sandbox != nil && len(frozen.Sandbox.ToolchainLockSnapshot) != 0 {
		copy := *cfg.Sandbox
		copy.ToolchainLockSnapshot = append([]byte(nil), frozen.Sandbox.ToolchainLockSnapshot...)
		cfg.Sandbox = &copy
	} else if cfg.Sandbox != nil && frozen.Sandbox != nil {
		// Preserve the distinction between a legacy run and a new run while
		// keeping the optional field omitted from its effective JSON identity.
		copy := *cfg.Sandbox
		copy.ToolchainLockSnapshot = []byte{}
		cfg.Sandbox = &copy
	}
	if cfg.Sandbox != nil && len(cfg.Sandbox.ToolchainLockSnapshot) != 0 {
		if _, err := application.LoadConfiguredToolchainLock(cfg); err != nil {
			return cfg, err
		}
	}
	if cfg.EffectiveDigest() != run.ConfigDigest {
		return cfg, errors.New("current configuration differs from its frozen run binding")
	}
	return cfg, nil
}

func runConfigCommand(args []string, cfg config.Config, stdout, stderr io.Writer) int {
	if len(args) == 0 || (args[0] != "validate" && args[0] != "effective") {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: config validate|effective --redact"))
	}
	if args[0] == "validate" {
		if len(args) != 1 {
			return writeStateError(stdout, stderr, 2, "usage", errors.New("config validate takes no flags"))
		}
		effective, err := cfg.EffectiveConfig()
		if err != nil {
			return writeStateError(stdout, stderr, 2, "config_invalid", err)
		}
		return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "VALID", Data: map[string]any{"digest": cfg.EffectiveDigest(), "schema_version": effective.SchemaVersion}}, 0)
	}
	if len(args) != 2 || args[1] != "--redact" {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: config effective --redact"))
	}
	effective, err := cfg.EffectiveConfig()
	if err != nil {
		return writeStateError(stdout, stderr, 2, "config_invalid", err)
	}
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "VALID", Data: effective}, 0)
}

func runGenerate(args []string, app *application.Application, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("generate", flag.ContinueOnError)
	flags.SetOutput(stderr)
	requestPath := flags.String("request", "", "request YAML/JSON path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *requestPath == "" {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: generate --request PATH"))
	}
	request, err := loadRequest(*requestPath)
	if err != nil {
		return writeStateError(stdout, stderr, 2, "request_invalid", err)
	}
	snapshot, err := app.Runs.Generate(context.Background(), request)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	status := string(snapshot.State)
	code := exitForSnapshot(snapshot.State)
	if snapshot.State == domain.RunNeedsReview {
		status = "NEEDS_REVIEW"
	}
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: status, Data: snapshot, RunVersion: snapshot.Version}, code)
}

func runRunCommand(args []string, app *application.Application, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("run list|show|events|resume|cancel"))
	}
	switch args[0] {
	case "list":
		flags := flag.NewFlagSet("run list", flag.ContinueOnError)
		flags.SetOutput(stderr)
		state := flags.String("state", "", "run state")
		limit := flags.Int("limit", 0, "maximum rows")
		if err := flags.Parse(movePositionalToEnd(args[1:])); err != nil || flags.NArg() != 0 {
			return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: run list [--state STATE]"))
		}
		filter := domain.RunFilter{Limit: *limit}
		if *limit < 0 || *limit > 1000 {
			return writeStateError(stdout, stderr, 2, "invalid_argument", errors.New("run list limit must be between zero and 1000"))
		}
		if *state != "" {
			parsed := domain.RunState(*state)
			if !parsed.Valid() {
				return writeStateError(stdout, stderr, 5, "invalid_state", fmt.Errorf("invalid run state %q", *state))
			}
			filter.State = &parsed
		}
		rows, err := app.Runtime.ListRuns(context.Background(), filter)
		if err != nil {
			return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].RunID < rows[j].RunID })
		return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "OK", Data: rows}, 0)
	case "show":
		if len(args) != 2 {
			return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: run show RUN_ID"))
		}
		id, err := parseRunID(args[1])
		if err != nil {
			return writeStateError(stdout, stderr, 3, "invalid_id", err)
		}
		snapshot, err := app.Runtime.GetRun(context.Background(), id)
		if err != nil {
			return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
		}
		return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: string(snapshot.State), Data: snapshot, RunVersion: snapshot.Version}, 0)
	case "events":
		return runEvents(args[1:], app, stdout, stderr)
	case "export":
		return runPackageExport(args[1:], app, stdout, stderr)
	case "resume":
		if len(args) != 2 {
			return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: run resume RUN_ID"))
		}
		id, err := parseRunID(args[1])
		if err != nil {
			return writeStateError(stdout, stderr, 3, "invalid_id", err)
		}
		snapshot, err := app.Runs.Resume(context.Background(), id)
		if err != nil {
			return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
		}
		code := exitForSnapshot(snapshot.State)
		return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: string(snapshot.State), Data: snapshot, RunVersion: snapshot.Version}, code)
	case "cancel":
		return runCancel(args[1:], app, stdout, stderr)
	default:
		return writeStateError(stdout, stderr, 2, "unknown_command", fmt.Errorf("unknown run command %q", args[0]))
	}
}

func runEvents(args []string, app *application.Application, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run events", flag.ContinueOnError)
	flags.SetOutput(stderr)
	after := flags.Int64("after-version", 0, "event version")
	if err := flags.Parse(movePositionalToEnd(args)); err != nil || flags.NArg() != 1 || *after < 0 {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: run events RUN_ID [--after-version N]"))
	}
	id, err := parseRunID(flags.Arg(0))
	if err != nil {
		return writeStateError(stdout, stderr, 3, "invalid_id", err)
	}
	// Events intentionally returns an empty slice for an existing run when no
	// event is newer than after-version. Verify the run first so a well-formed
	// but nonexistent RUN_ID remains distinguishable from that empty result.
	if _, err := app.Runtime.GetRun(context.Background(), id); err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	events, err := app.Runtime.Events(context.Background(), id, *after)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Version < events[j].Version })
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "OK", Data: events}, 0)
}

func runCancel(args []string, app *application.Application, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("run cancel", flag.ContinueOnError)
	flags.SetOutput(stderr)
	reason := flags.String("reason", "", "cancellation reason")
	if err := flags.Parse(movePositionalToEnd(args)); err != nil || flags.NArg() != 1 || strings.TrimSpace(*reason) == "" {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: run cancel RUN_ID --reason REASON"))
	}
	id, err := parseRunID(flags.Arg(0))
	if err != nil {
		return writeStateError(stdout, stderr, 3, "invalid_id", err)
	}
	snapshot, err := app.Runtime.GetRun(context.Background(), id)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	controlID, err := domain.NewID("control")
	if err != nil {
		return writeStateError(stdout, stderr, 9, "id_generation", err)
	}
	request := domain.CancelRequest{ID: domain.ControlRequestID(controlID), RunID: id, ExpectedRunVersion: snapshot.Version, Reason: strings.TrimSpace(*reason), IdempotencyKey: controlID, At: time.Now().UTC()}
	result, err := app.Runs.Cancel(context.Background(), request)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: string(result.State), Data: result, RunVersion: result.Version}, 0)
}

func runReviewCommand(args []string, app *application.Application, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("review show|revise|retry|waive|reject"))
	}
	if args[0] == "show" {
		if len(args) != 2 {
			return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: review show RUN_ID"))
		}
		id, err := parseRunID(args[1])
		if err != nil {
			return writeStateError(stdout, stderr, 3, "invalid_id", err)
		}
		// PendingReview intentionally returns a nil decision when an existing
		// run has no pending review. Verify the run first so a well-formed but
		// nonexistent RUN_ID remains distinguishable from that empty result.
		if _, err := app.Runtime.GetRun(context.Background(), id); err != nil {
			return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
		}
		review, err := app.Reviews.PendingReview(context.Background(), id)
		if err != nil {
			return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
		}
		return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "OK", Data: review}, 0)
	}
	return runReviewMutation(args, app, stdout, stderr)
}

func runReviewMutation(args []string, app *application.Application, stdout, stderr io.Writer) int {
	kind := args[0]
	if kind != "revise" && kind != "retry" && kind != "waive" && kind != "reject" {
		return writeStateError(stdout, stderr, 2, "unknown_command", fmt.Errorf("unknown review command %q", kind))
	}
	flags := flag.NewFlagSet("review "+kind, flag.ContinueOnError)
	flags.SetOutput(stderr)
	reviewer := flags.String("reviewer", "", "reviewer")
	reason := flags.String("reason", "", "reason")
	step := flags.String("step", "", "step")
	patchPath := flags.String("patch", "", "revision patch")
	budgetPath := flags.String("budget-patch", "", "budget patch")
	gate := flags.String("gate", "", "waivable gate digest")
	evidence := flags.String("evidence", "", "evidence digest")
	if err := flags.Parse(movePositionalToEnd(args[1:])); err != nil || flags.NArg() != 1 || strings.TrimSpace(*reviewer) == "" || strings.TrimSpace(*reason) == "" {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("review mutation requires RUN_ID --reviewer NAME --reason REASON"))
	}
	if kind == "revise" && (*step == "" || *patchPath == "") {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("revise requires --step and --patch"))
	}
	if kind == "retry" && *budgetPath == "" && *evidence == "" {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("retry requires --budget-patch or --evidence"))
	}
	if kind == "waive" && (*gate == "" || *evidence == "") {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("waive requires --gate and --evidence"))
	}
	id, err := parseRunID(flags.Arg(0))
	if err != nil {
		return writeStateError(stdout, stderr, 3, "invalid_id", err)
	}
	guard, err := app.Locks.TryAcquireRun(id, runlock.Exclusive)
	if errors.Is(err, runlock.ErrBusy) {
		return writeStateError(stdout, stderr, 4, "lock_busy", err)
	}
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	defer guard.Close()
	snapshot, err := app.Runtime.GetRun(context.Background(), id)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	if snapshot.State != domain.RunNeedsReview {
		return writeStateError(stdout, stderr, 5, "invalid_state", fmt.Errorf("run is %s, review requires NEEDS_REVIEW", snapshot.State))
	}
	stageInput, err := reviewStageInput(context.Background(), app.Runtime, snapshot)
	if err != nil {
		return writeStateError(stdout, stderr, 5, "review_binding", err)
	}
	kindValue := map[string]domain.ReviewDecisionKind{"revise": domain.ReviewRevise, "retry": domain.ReviewRetry, "waive": domain.ReviewWaive, "reject": domain.ReviewReject}[kind]
	decisionID, err := domain.NewID("review")
	if err != nil {
		return writeStateError(stdout, stderr, 9, "id_generation", err)
	}
	request := domain.CreateReviewRequest{ID: domain.ReviewDecisionID(decisionID), RunID: id, ExpectedRunVersion: snapshot.Version, Kind: kindValue, WorkflowRevision: snapshot.WorkflowRevision, StageName: snapshot.CurrentStage, StageInputDigest: stageInput, EvidenceDigest: stageInput, PolicyDigest: snapshot.ConfigDigest, Reviewer: strings.TrimSpace(*reviewer), Reason: strings.TrimSpace(*reason), IdempotencyKey: decisionID, At: time.Now().UTC()}
	if *step != "" {
		if kind != "revise" {
			return writeStateError(stdout, stderr, 2, "usage", errors.New("--step is valid only for revise"))
		}
		request.StageName = domain.StageName(*step)
	}
	if *patchPath != "" {
		if kind != "revise" {
			return writeStateError(stdout, stderr, 2, "usage", errors.New("--patch is valid only for revise"))
		}
		data, readErr := os.ReadFile(*patchPath)
		if readErr != nil {
			return writeStateError(stdout, stderr, 2, "patch_invalid", readErr)
		}
		digest := domain.SumBytes(data)
		request.RequestedEditsDigest = &digest
	}
	if *budgetPath != "" {
		if kind != "retry" {
			return writeStateError(stdout, stderr, 2, "usage", errors.New("--budget-patch is valid only for retry"))
		}
		data, readErr := os.ReadFile(*budgetPath)
		if readErr != nil {
			return writeStateError(stdout, stderr, 2, "budget_invalid", readErr)
		}
		if err := json.Unmarshal(data, &request.BudgetIncrease); err != nil {
			return writeStateError(stdout, stderr, 2, "budget_invalid", err)
		}
	}
	if *gate != "" {
		digest, parseErr := domain.ParseDigest(*gate)
		if parseErr != nil {
			return writeStateError(stdout, stderr, 2, "digest_invalid", parseErr)
		}
		request.WaiverScopeDigest = &digest
	}
	if *evidence != "" {
		digest, parseErr := domain.ParseDigest(*evidence)
		if parseErr != nil {
			return writeStateError(stdout, stderr, 2, "digest_invalid", parseErr)
		}
		if kind == "waive" {
			request.EvidenceDigest = digest
		} else {
			request.ExternalConditionDigest = &digest
		}
	}
	decision, err := app.Reviews.CreateReview(context.Background(), request)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: string(decision.State), Data: decision, RunVersion: decision.RunVersion}, 0)
}

func reviewStageInput(ctx context.Context, runtimeStore port.RuntimeStore, snapshot domain.RunSnapshot) (domain.Digest, error) {
	reader, ok := runtimeStore.(interface {
		CurrentStageAttempt(context.Context, domain.RunID, domain.StageName) (domain.StageAttempt, error)
	})
	if !ok {
		return snapshot.RequestDigest, nil
	}
	attempt, err := reader.CurrentStageAttempt(ctx, snapshot.RunID, snapshot.CurrentStage)
	if err != nil {
		return "", err
	}
	return attempt.InputDigest, nil
}

func parseRunID(raw string) (domain.RunID, error) {
	id := domain.RunID(raw)
	if err := id.Validate(); err != nil {
		return "", err
	}
	return id, nil
}

// movePositionalToEnd accepts the documented ergonomic form (`run cancel
// RUN_ID --reason ...`) while still using Go's strict FlagSet parser. Unknown
// flags are not swallowed: FlagSet reports them after this mechanical reorder.
func movePositionalToEnd(args []string) []string {
	flags := make([]string, 0, len(args))
	positional := make([]string, 0, 1)
	for index := 0; index < len(args); index++ {
		value := args[index]
		if strings.HasPrefix(value, "-") {
			flags = append(flags, value)
			if index+1 < len(args) && !strings.HasPrefix(args[index+1], "-") {
				flags = append(flags, args[index+1])
				index++
			}
			continue
		}
		positional = append(positional, value)
	}
	return append(flags, positional...)
}

func loadRequest(path string) (domain.RunRequest, error) {
	canonical, err := canonicalPath(path)
	if err != nil {
		return domain.RunRequest{}, err
	}
	data, err := os.ReadFile(canonical)
	if err != nil {
		return domain.RunRequest{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		return domain.RunRequest{}, err
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return domain.RunRequest{}, errors.New("request contains trailing YAML document")
		}
		return domain.RunRequest{}, err
	}
	var value any
	if err := node.Decode(&value); err != nil {
		return domain.RunRequest{}, err
	}
	jsonBytes, err := json.Marshal(value)
	if err != nil {
		return domain.RunRequest{}, err
	}
	var request domain.RunRequest
	jsonDecoder := json.NewDecoder(bytes.NewReader(jsonBytes))
	jsonDecoder.DisallowUnknownFields()
	if err := jsonDecoder.Decode(&request); err != nil {
		return domain.RunRequest{}, err
	}
	if err := request.Validate(); err != nil {
		return domain.RunRequest{}, err
	}
	return request, nil
}

func canonicalPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return filepath.Clean(resolved), nil
}

func encodeEnvelope(stdout, stderr io.Writer, value envelope, code int) int {
	return encodeJSON(stdout, stderr, value, code)
}

func writeStateError(stdout, stderr io.Writer, code int, errorCode string, err error) int {
	if errorCode == "" {
		errorCode = "error"
	}
	fieldName := ""
	var fieldErr *config.FieldError
	if errors.As(err, &fieldErr) {
		fieldName = fieldErr.Field
	}
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "ERROR", Error: &responseError{Code: errorCode, Message: err.Error(), Field: fieldName}}, code)
}

func codeForError(err error) string {
	switch {
	case errors.Is(err, sandboxexec.ErrCleanupPending):
		return "cleanup_pending"
	case errors.Is(err, runlock.ErrBusy):
		return "lock_busy"
	case errors.Is(err, sqlite.ErrNotFound):
		return "not_found"
	case errors.Is(err, sqlite.ErrVersionConflict):
		return "version_conflict"
	case errors.Is(err, sqlite.ErrInvalidTransition):
		return "invalid_state"
	case errors.Is(err, sqlite.ErrConsistency):
		return "consistency"
	default:
		return "operation_failed"
	}
}

func exitForError(err error) int {
	switch {
	case errors.Is(err, sandboxexec.ErrCleanupPending):
		return 10
	case errors.Is(err, runlock.ErrBusy):
		return 4
	case errors.Is(err, sqlite.ErrNotFound):
		return 3
	case errors.Is(err, sqlite.ErrInvalidTransition):
		return 5
	case errors.Is(err, sqlite.ErrVersionConflict):
		return 7
	default:
		return 9
	}
}

// exitForSnapshot preserves the CLI's stable review fixture code (6) and
// gives every other non-terminal or terminal workflow outcome its own
// non-zero result. These values are intentionally distinct from malformed
// input (2), missing IDs (3), lock conflicts (4), and cleanup timeout (10).
func exitForSnapshot(state domain.RunState) int {
	switch state {
	case domain.RunBlocked:
		return 5
	case domain.RunNeedsReview:
		return 6
	case domain.RunFailed:
		return 7
	case domain.RunCancelled:
		return 8
	default:
		return 0
	}
}

func runSandboxWatchdog(args []string, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("sandbox-watchdog", flag.ContinueOnError)
	flags.SetOutput(stderr)
	control := flags.String("control", "", "owner-only watchdog control record")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *control == "" || !filepath.IsAbs(*control) || filepath.Clean(*control) != *control {
		fmt.Fprintln(stderr, "invalid sandbox watchdog invocation")
		return 2
	}
	if dependencies.RunWatchdog == nil {
		fmt.Fprintln(stderr, "sandbox watchdog dependency is not configured")
		return 1
	}
	if err := dependencies.RunWatchdog(context.Background(), *control); err != nil {
		fmt.Fprintf(stderr, "sandbox watchdog failed: %v\n", err)
		return 1
	}
	return 0
}

func runDoctor(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "emit versioned JSON")
	engineEndpoint := flags.String("engine-endpoint", "", "explicit local Docker Engine endpoint")
	apiVersion := flags.String("api-version", "", "pinned Docker Engine API version")
	builderImage := flags.String("builder-image", "", "immutable builder image ID")
	runtimeImage := flags.String("runtime-image", "", "immutable runtime image ID")
	transferImage := flags.String("transfer-image", "", "immutable transfer image ID")
	executionProtocol := flags.String("execution-protocol", "", "sandbox execution protocol")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || !*jsonOutput {
		fmt.Fprintln(stderr, "usage: cpgen doctor --json --engine-endpoint ENDPOINT --api-version VERSION --builder-image SHA256 --runtime-image SHA256 --transfer-image SHA256 --execution-protocol docker-direct-v2")
		return 2
	}
	dockerConfig := dockersandbox.Config{EngineEndpoint: *engineEndpoint, APIVersion: *apiVersion, BuilderImage: *builderImage, RuntimeImage: *runtimeImage, TransferImage: *transferImage, ExecutionProtocol: *executionProtocol}
	goos := dependencies.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if _, err := dockerConfig.Validate(goos); err != nil {
		fmt.Fprintf(stderr, "invalid doctor configuration: %v\n", err)
		return 2
	}
	if dependencies.CheckDocker == nil {
		fmt.Fprintln(stderr, "doctor dependency is not configured")
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := dependencies.CheckDocker(ctx, dockerConfig)
	if err == nil {
		return encodeJSON(stdout, stderr, doctorOutput{SchemaVersion: "cpgen.doctor/v1", Status: "HEALTHY", EngineIdentityDigest: report.EngineIdentityDigest, Checks: staticDoctorChecks(), Report: &report}, 0)
	}
	var checkErr *dockersandbox.CheckError
	if errors.As(err, &checkErr) {
		if validationErr := checkErr.Failure.Validate(); validationErr != nil {
			fmt.Fprintf(stderr, "invalid doctor failure: %v\n", validationErr)
			return 1
		}
		return encodeJSON(stdout, stderr, doctorOutput{SchemaVersion: "cpgen.doctor/v1", Status: string(checkErr.Failure.Class), Checks: staticDoctorChecks(), FailureCode: checkErr.Failure.Code, FailureClass: checkErr.Failure.Class, Message: checkErr.Error()}, 10)
	}
	fmt.Fprintf(stderr, "doctor failed: %v\n", err)
	return 1
}

func staticDoctorChecks() []string {
	return []string{"engine_ping", "server_version", "server_info", "builder_image", "runtime_image", "transfer_image"}
}

func encodeJSON(stdout, stderr io.Writer, value any, successCode int) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(stderr, "write JSON: %v\n", err)
		return 1
	}
	return successCode
}

func writeHelp(writer io.Writer) {
	fmt.Fprintln(writer, `cpgen - competitive programming problem generator

Usage:
  cpgen help
  cpgen version [--json]
  cpgen doctor --json --engine-endpoint ENDPOINT --api-version VERSION --builder-image SHA256 --runtime-image SHA256 --transfer-image SHA256 --execution-protocol docker-direct-v2
  cpgen --config PATH config validate|effective --redact
  cpgen --config PATH generate --request PATH
  cpgen --config PATH run list|show|events|resume|cancel
  cpgen --config PATH run export RUN_ID --output PATH
  cpgen --config PATH review show|revise|retry|waive|reject

The stateful executor is local and foreground-only.`)
}
