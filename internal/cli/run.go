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
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	dockersandbox "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/web"
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
	return RunWithDependencies(args, stdout, stderr, Dependencies{})
}

// RunWithDependencies parses before touching configuration or application resources.
func RunWithDependencies(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	invocation, err := parseInvocation(args)
	if err != nil {
		return err.write(stdout, stderr)
	}
	return invocation.run(stdout, stderr, dependencies.withDefaults())
}

func (d Dependencies) withDefaults() Dependencies {
	if d.GOOS == "" {
		d.GOOS = runtime.GOOS
	}
	if d.CheckDocker == nil {
		d.CheckDocker = dockersandbox.CheckStatic
	}
	if d.RunWatchdog == nil {
		d.RunWatchdog = dockersandbox.RunWatchdogService
	}
	if d.Bootstrap == nil {
		d.Bootstrap = application.Bootstrap
	}
	if d.BootstrapLocal == nil {
		d.BootstrapLocal = application.BootstrapLocal
	}
	return d
}

func runStateful(command *preparedCommand, configPath string, stdout, stderr io.Writer, dependencies Dependencies) int {
	configPath, err := canonicalPath(configPath)
	if err != nil {
		return writeStateError(stdout, stderr, 2, "config_path", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return writeStateError(stdout, stderr, 2, "config_invalid", err)
	}
	if command.configOnly {
		return runConfigCommand(command.effective, cfg, stdout, stderr)
	}
	if cfg.Similarity != nil && cfg.Similarity.Endpoint == web.FixtureSimilarityEndpoint && !command.serveSimilarityFixture {
		return writeStateError(stdout, stderr, 9, "fixture_mode_required", errors.New("the local similarity fixture endpoint requires serve --fixture-similarity"))
	}
	if command.serveListen != "" {
		var server *web.Server
		if command.serveSimilarityFixture {
			server, err = web.NewWithCapacityAndSimilarityFixture(context.Background(), cfg, command.serveCapacity)
		} else {
			server, err = web.NewWithCapacity(context.Background(), cfg, command.serveCapacity)
		}
		if err != nil {
			return writeStateError(stdout, stderr, 9, "bootstrap_failed", err)
		}
		defer server.Close()
		serveCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		if err := server.Serve(serveCtx, command.serveListen, func(addr string) { fmt.Fprintf(stderr, "CPGen 本地工作台：%s\n", server.BootstrapURL(addr)) }); err != nil {
			return writeStateError(stdout, stderr, 9, "serve_failed", err)
		}
		return 0
	}
	if command.restore && cfg.Sandbox != nil && cfg.Workflow != nil && workflow.HasSolutionStages(cfg.Workflow.Revision) {
		cfg, err = restoreToolchainSnapshot(context.Background(), cfg, command.runID, dependencies)
		if err != nil {
			return writeStateError(stdout, stderr, 9, "bootstrap_failed", err)
		}
	}
	bootstrap := dependencies.Bootstrap
	if command.local {
		bootstrap = dependencies.BootstrapLocal
	}
	app, err := bootstrap(context.Background(), cfg)
	if err != nil {
		return writeStateError(stdout, stderr, 9, "bootstrap_failed", err)
	}
	defer app.Close()
	return command.execute(app, stdout, stderr)
}

func prepareServe(args []string) (*preparedCommand, *commandError) {
	flags := commandFlags("serve")
	listen := flags.String("listen", "127.0.0.1:8080", "loopback address")
	capacity := flags.Int("capacity", 1, "maximum concurrent generation tasks (1-64)")
	similarityFixture := flags.Bool("fixture-similarity", false, "use the local TLS similarity fixture (development only)")
	if err := parseFlags(flags, args); err != nil {
		return nil, err
	}
	if strings.TrimSpace(*listen) == "" || flags.NArg() != 0 || *capacity < 1 || *capacity > 64 {
		return nil, usageError("serve requires a non-empty loopback address")
	}
	return &preparedCommand{serveListen: *listen, serveCapacity: *capacity, serveSimilarityFixture: *similarityFixture}, nil
}

type runDocumentsReader interface {
	RunViewDocuments(context.Context, domain.RunID) ([]byte, []byte, error)
}

func restoreToolchainSnapshot(ctx context.Context, cfg config.Config, runID domain.RunID, dependencies Dependencies) (config.Config, error) {
	bootstrap := dependencies.withDefaults().BootstrapLocal
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

func prepareConfig(args []string, effective bool) (*preparedCommand, *commandError) {
	flags := commandFlags("config validate")
	if effective {
		flags = commandFlags("config effective")
	}
	var redact bool
	if effective {
		flags.BoolVar(&redact, "redact", false, "redact configuration")
	}
	if err := parseFlags(flags, args); err != nil {
		return nil, err
	}
	if effective && !redact {
		return nil, usageError("config effective requires --redact")
	}
	return &preparedCommand{configOnly: true, effective: effective}, nil
}

func runConfigCommand(effectiveOutput bool, cfg config.Config, stdout, stderr io.Writer) int {
	if !effectiveOutput {
		effective, err := cfg.EffectiveConfig()
		if err != nil {
			return writeStateError(stdout, stderr, 2, "config_invalid", err)
		}
		return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "VALID", Data: map[string]any{"digest": cfg.EffectiveDigest(), "schema_version": effective.SchemaVersion}}, 0)
	}
	effective, err := cfg.EffectiveConfig()
	if err != nil {
		return writeStateError(stdout, stderr, 2, "config_invalid", err)
	}
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "VALID", Data: effective}, 0)
}

func prepareGenerate(args []string) (*preparedCommand, *commandError) {
	flags := commandFlags("generate")
	request := flags.String("request", "", "request YAML/JSON path")
	if err := parseFlags(flags, args); err != nil {
		return nil, err
	}
	if *request == "" {
		return nil, usageError("generate requires --request PATH")
	}
	return &preparedCommand{execute: func(app *application.Application, out, diagnostic io.Writer) int {
		return runGenerate(*request, app, out, diagnostic)
	}}, nil
}

func runGenerate(requestPath string, app *application.Application, stdout, stderr io.Writer) int {
	request, err := loadRequest(requestPath)
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

func prepareRunList(args []string) (*preparedCommand, *commandError) {
	flags := commandFlags("run list")
	state := flags.String("state", "", "run state")
	limit := flags.Int("limit", 0, "maximum rows")
	if err := parseFlags(flags, args); err != nil {
		return nil, err
	}
	if *limit < 0 || *limit > 1000 {
		return nil, argumentError("invalid_argument", 2, errors.New("run list limit must be between zero and 1000"))
	}
	filter := domain.RunFilter{Limit: *limit}
	if *state != "" {
		value := domain.RunState(*state)
		if !value.Valid() {
			return nil, argumentError("invalid_state", 5, fmt.Errorf("invalid run state %q", *state))
		}
		filter.State = &value
	}
	return &preparedCommand{local: true, execute: func(app *application.Application, out, diagnostic io.Writer) int {
		return runList(filter, app, out, diagnostic)
	}}, nil
}

func runList(filter domain.RunFilter, app *application.Application, stdout, stderr io.Writer) int {
	rows, err := app.Runtime.ListRuns(context.Background(), filter)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].RunID < rows[j].RunID })
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "OK", Data: rows}, 0)

}

func prepareRunShow(args []string) (*preparedCommand, *commandError) {
	id, err := parseRunFlags(commandFlags("run show"), args)
	if err != nil {
		return nil, err
	}
	return &preparedCommand{runID: id, local: true, execute: func(app *application.Application, out, diagnostic io.Writer) int {
		return runShow(id, app, out, diagnostic)
	}}, nil
}

func runShow(id domain.RunID, app *application.Application, stdout, stderr io.Writer) int {
	snapshot, err := app.Runtime.GetRun(context.Background(), id)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: string(snapshot.State), Data: snapshot, RunVersion: snapshot.Version}, 0)

}

func prepareRunResume(args []string) (*preparedCommand, *commandError) {
	id, err := parseRunFlags(commandFlags("run resume"), args)
	if err != nil {
		return nil, err
	}
	return &preparedCommand{runID: id, restore: true, execute: func(app *application.Application, out, diagnostic io.Writer) int {
		return runResume(id, app, out, diagnostic)
	}}, nil
}

func runResume(id domain.RunID, app *application.Application, stdout, stderr io.Writer) int {
	snapshot, err := app.Runs.Resume(context.Background(), id)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	code := exitForSnapshot(snapshot.State)
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: string(snapshot.State), Data: snapshot, RunVersion: snapshot.Version}, code)

}

func prepareRunEvents(args []string) (*preparedCommand, *commandError) {
	flags := commandFlags("run events")
	after := flags.Int64("after-version", 0, "event version")
	id, err := parseRunFlags(flags, args)
	if err != nil {
		return nil, err
	}
	if *after < 0 {
		return nil, usageError("--after-version must be nonnegative")
	}
	return &preparedCommand{runID: id, local: true, execute: func(app *application.Application, out, diagnostic io.Writer) int {
		return runEvents(id, *after, app, out, diagnostic)
	}}, nil
}

func runEvents(id domain.RunID, after int64, app *application.Application, stdout, stderr io.Writer) int {
	// Events intentionally returns an empty slice for an existing run when no
	// event is newer than after-version. Verify the run first so a well-formed
	// but nonexistent RUN_ID remains distinguishable from that empty result.
	if _, err := app.Runtime.GetRun(context.Background(), id); err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	events, err := app.Runtime.Events(context.Background(), id, after)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Version < events[j].Version })
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "OK", Data: events}, 0)
}

func prepareRunCancel(args []string) (*preparedCommand, *commandError) {
	flags := commandFlags("run cancel")
	reason := flags.String("reason", "", "cancellation reason")
	id, err := parseRunFlags(flags, args)
	if err != nil {
		return nil, err
	}
	*reason = strings.TrimSpace(*reason)
	if *reason == "" {
		return nil, usageError("run cancel requires --reason REASON")
	}
	return &preparedCommand{runID: id, restore: true, execute: func(app *application.Application, out, diagnostic io.Writer) int {
		return runCancel(id, *reason, app, out, diagnostic)
	}}, nil
}

func runCancel(id domain.RunID, reason string, app *application.Application, stdout, stderr io.Writer) int {
	snapshot, err := app.Runtime.GetRun(context.Background(), id)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	controlID, err := domain.NewID("control")
	if err != nil {
		return writeStateError(stdout, stderr, 9, "id_generation", err)
	}
	request := domain.CancelRequest{ID: domain.ControlRequestID(controlID), RunID: id, ExpectedRunVersion: snapshot.Version, Reason: reason, IdempotencyKey: controlID, At: time.Now().UTC()}
	result, err := app.Runs.Cancel(context.Background(), request)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: string(result.State), Data: result, RunVersion: result.Version}, 0)
}

func prepareReviewShow(args []string) (*preparedCommand, *commandError) {
	id, err := parseRunFlags(commandFlags("review show"), args)
	if err != nil {
		return nil, err
	}
	return &preparedCommand{runID: id, local: true, execute: func(app *application.Application, out, diagnostic io.Writer) int {
		return runReviewShow(id, app, out, diagnostic)
	}}, nil
}

func runReviewShow(id domain.RunID, app *application.Application, stdout, stderr io.Writer) int {
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

type reviewOptions struct {
	reviewer, reason, step, patchPath, budgetPath string
	gate, evidence                                string
}

func prepareReviewMutation(kind string, args []string) (*preparedCommand, *commandError) {
	flags := commandFlags("review " + kind)
	var options reviewOptions
	flags.StringVar(&options.reviewer, "reviewer", "", "reviewer")
	flags.StringVar(&options.reason, "reason", "", "reason")
	switch kind {
	case "revise":
		flags.StringVar(&options.step, "step", "", "step")
		flags.StringVar(&options.patchPath, "patch", "", "revision patch")
	case "retry":
		flags.StringVar(&options.budgetPath, "budget-patch", "", "budget patch")
		flags.StringVar(&options.evidence, "evidence", "", "evidence digest")
	case "waive":
		flags.StringVar(&options.gate, "gate", "", "waivable gate digest")
		flags.StringVar(&options.evidence, "evidence", "", "evidence digest")
	}
	id, err := parseRunFlags(flags, args)
	if err != nil {
		return nil, err
	}
	options.reason = strings.TrimSpace(options.reason)
	options.reviewer = strings.TrimSpace(options.reviewer)
	if options.reason == "" || options.reviewer == "" {
		return nil, usageError("review mutation requires --reviewer NAME --reason REASON")
	}
	switch kind {
	case "revise":
		if options.step == "" {
			return nil, usageError("revise requires --step TARGET_STAGE")
		}
	case "retry":
		if options.budgetPath == "" && options.evidence == "" {
			return nil, usageError("retry requires --budget-patch or --evidence")
		}
	case "waive":
		if options.gate == "" || options.evidence == "" {
			return nil, usageError("waive requires --gate and --evidence")
		}
	}
	for _, raw := range []string{options.gate, options.evidence} {
		if raw != "" {
			if _, err := domain.ParseDigest(raw); err != nil {
				return nil, argumentError("digest_invalid", 2, err)
			}
		}
	}
	return &preparedCommand{runID: id, restore: true, execute: func(app *application.Application, out, diagnostic io.Writer) int {
		return runReviewMutation(id, kind, options, app, out, diagnostic)
	}}, nil
}

func runReviewMutation(id domain.RunID, kind string, options reviewOptions, app *application.Application, stdout, stderr io.Writer) int {
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
	bindingReader, ok := app.Runtime.(port.ReviewBindingReader)
	if !ok {
		return writeStateError(stdout, stderr, 5, "review_binding", errors.New("runtime store cannot read exact review evidence binding"))
	}
	stageInput, evidence, policy, err := bindingReader.ReadReviewBinding(context.Background(), id, snapshot.CurrentStage)
	if err != nil {
		return writeStateError(stdout, stderr, 5, "review_binding", err)
	}
	kindValue := map[string]domain.ReviewDecisionKind{"revise": domain.ReviewRevise, "retry": domain.ReviewRetry, "waive": domain.ReviewWaive, "reject": domain.ReviewReject}[kind]
	decisionID, err := domain.NewID("review")
	if err != nil {
		return writeStateError(stdout, stderr, 9, "id_generation", err)
	}
	request := domain.CreateReviewRequest{ID: domain.ReviewDecisionID(decisionID), RunID: id, ExpectedRunVersion: snapshot.Version, Kind: kindValue, WorkflowRevision: snapshot.WorkflowRevision, StageName: snapshot.CurrentStage, StageInputDigest: stageInput, EvidenceDigest: evidence, PolicyDigest: policy, Reviewer: strings.TrimSpace(options.reviewer), Reason: strings.TrimSpace(options.reason), IdempotencyKey: decisionID, At: time.Now().UTC()}
	if kind == "revise" {
		target := domain.StageName(options.step)
		request.RevisionTargetStage = &target
	}
	if options.patchPath != "" {
		data, readErr := os.ReadFile(options.patchPath)
		if readErr != nil {
			return writeStateError(stdout, stderr, 2, "patch_invalid", readErr)
		}
		digest := domain.SumBytes(data)
		request.RequestedEditsDigest = &digest
	}
	if kind == "revise" && request.RequestedEditsDigest == nil {
		digest, digestErr := domain.ReviewRevisionIntentDigest(*request.RevisionTargetStage, request.Reason)
		if digestErr != nil {
			return writeStateError(stdout, stderr, 2, "revision_invalid", digestErr)
		}
		request.RequestedEditsDigest = &digest
	}
	if options.budgetPath != "" {
		data, readErr := os.ReadFile(options.budgetPath)
		if readErr != nil {
			return writeStateError(stdout, stderr, 2, "budget_invalid", readErr)
		}
		if err := json.Unmarshal(data, &request.BudgetIncrease); err != nil {
			return writeStateError(stdout, stderr, 2, "budget_invalid", err)
		}
	}
	if options.gate != "" {
		digest, parseErr := domain.ParseDigest(options.gate)
		if parseErr != nil {
			return writeStateError(stdout, stderr, 2, "digest_invalid", parseErr)
		}
		request.WaiverScopeDigest = &digest
	}
	if options.evidence != "" {
		digest, parseErr := domain.ParseDigest(options.evidence)
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

func parseRunID(raw string) (domain.RunID, error) {
	id := domain.RunID(raw)
	if err := id.Validate(); err != nil {
		return "", err
	}
	return id, nil
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
  cpgen --config PATH config validate
  cpgen --config PATH config effective --redact
  cpgen --config PATH generate --request PATH
  cpgen --config PATH run list|show|events|resume|cancel
  cpgen --config PATH run export RUN_ID --output PATH
  cpgen --config PATH review show|revise|retry|waive|reject

The stateful executor is local and foreground-only.`)
}
