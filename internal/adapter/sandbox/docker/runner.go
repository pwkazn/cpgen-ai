package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
	"cpgen/internal/transfer"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/volume"
	moby "github.com/moby/moby/client"
)

type WatchdogController interface {
	BeforeCreate(context.Context, port.PlannedResource, map[string]string) error
	ResourceCreated(context.Context, port.PlannedResource, string) error
	BeforeStart(context.Context, port.PlannedResource, string) error
}

type ControlLimits struct {
	HelperMemoryBytes int64
	HelperPIDs        int64
	MaxTransferBytes  int64
	CleanupTimeout    time.Duration
}

func (l ControlLimits) Validate() error {
	if l.HelperMemoryBytes <= 0 || l.HelperPIDs <= 0 || l.MaxTransferBytes <= 0 || l.CleanupTimeout <= 0 {
		return fmt.Errorf("Docker control limits must all be positive")
	}
	return nil
}

type RunnerOptions struct {
	Engine               Engine
	Config               Config
	Lock                 toolchain.Lock
	EngineIdentityDigest domain.Digest
	Blobs                port.VerifiedBlobReader
	Artifacts            port.MeteredArtifactSink
	Watchdog             WatchdogController
	Limits               ControlLimits
}

type Runner struct {
	engine         Engine
	config         Config
	lock           toolchain.Lock
	engineIdentity domain.Digest
	blobs          port.VerifiedBlobReader
	artifacts      port.MeteredArtifactSink
	watchdog       WatchdogController
	limits         ControlLimits
}

func NewRunner(options RunnerOptions) (*Runner, error) {
	if options.Engine == nil || options.Blobs == nil || options.Artifacts == nil || options.Watchdog == nil {
		return nil, fmt.Errorf("Docker Runner dependencies are required")
	}
	if _, err := options.Config.Validate(runtime.GOOS); err != nil {
		return nil, err
	}
	if err := options.Lock.Validate(); err != nil {
		return nil, err
	}
	if options.Config.BuilderImage != string(options.Lock.Builder.ImageID) || options.Config.RuntimeImage != string(options.Lock.Runtime.ImageID) || options.Config.TransferImage != string(options.Lock.Transfer.ImageID) {
		return nil, fmt.Errorf("Docker config images do not match the toolchain lock")
	}
	if err := options.EngineIdentityDigest.Validate(); err != nil {
		return nil, fmt.Errorf("Engine identity: %w", err)
	}
	if err := options.Limits.Validate(); err != nil {
		return nil, err
	}
	encoded, err := options.Lock.MarshalIndent()
	if err != nil {
		return nil, err
	}
	lockCopy, err := toolchain.LoadLock(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	return &Runner{
		engine: options.Engine, config: options.Config, lock: lockCopy, engineIdentity: options.EngineIdentityDigest,
		blobs: options.Blobs, artifacts: options.Artifacts, watchdog: options.Watchdog, limits: options.Limits,
	}, nil
}

type payloadGroup struct {
	volume port.PlannedResource
	files  []transfer.StreamFile
}

type preparedArtifact struct {
	declaration port.ArtifactDeclaration
	frame       port.OutputDeclaration
	wantMode    transfer.FrameMode
	writer      port.ArtifactWriter
	finalized   bool
	pending     domain.PendingArtifact
}

type ownedContainer struct {
	resource port.PlannedResource
	name     string
	id       string
	labels   map[string]string
	running  bool
}

type ownedVolume struct {
	resource port.PlannedResource
	name     string
	labels   map[string]string
	driver   string
	options  map[string]string
}

type operation struct {
	runner             *Runner
	auth               port.SandboxDispatchAuthorization
	plan               port.ContainerPlan
	identity           PlanIdentity
	nextContainerIndex int
	physical           []domain.AttemptCallID
	targetCall         *domain.AttemptCallID
	containers         []*ownedContainer
	volumes            []*ownedVolume
	writers            []*preparedArtifact
}

var plannedNoncePattern = regexp.MustCompile(`^cpgen-s0-([0-9a-f]{32})-`)

func (r *Runner) newOperation(auth port.SandboxDispatchAuthorization) (*operation, error) {
	if auth == nil {
		return nil, fmt.Errorf("sandbox authorization is required")
	}
	plan := auth.ContainerPlan()
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	probeIdentity := port.ProbeAuthorizationIdentity{
		LogicalOperationID: auth.LogicalOperationID(), RunID: auth.RunID(), AttemptID: auth.AttemptID(), OwnerID: auth.OwnerID(),
		LeaseEpoch: auth.LeaseEpoch(), ScopeDigest: auth.ScopeDigest(), PlanDigest: auth.PlanDigest(),
	}
	if err := probeIdentity.Validate(); err != nil {
		return nil, err
	}
	if auth.PlanDigest() != plan.PlanDigest || plan.EngineIdentityDigest != r.engineIdentity || len(plan.Resources) == 0 {
		return nil, fmt.Errorf("authorization plan or Engine identity mismatch")
	}
	match := plannedNoncePattern.FindStringSubmatch(plan.Resources[0].DeterministicName)
	if len(match) != 2 {
		return nil, fmt.Errorf("planned resource name does not carry an operation nonce")
	}
	identity := PlanIdentity{
		RunID: auth.RunID(), AttemptID: auth.AttemptID(), LogicalOperationID: auth.LogicalOperationID(), LeaseEpoch: auth.LeaseEpoch(),
		OperationNonce: match[1], EngineIdentityDigest: r.engineIdentity,
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if plan.TransferBytesMax > r.limits.MaxTransferBytes {
		return nil, fmt.Errorf("planned transfer bytes %d exceed control limit %d", plan.TransferBytesMax, r.limits.MaxTransferBytes)
	}
	return &operation{runner: r, auth: auth, plan: plan, identity: identity}, nil
}

func (r *Runner) Compile(ctx context.Context, auth port.SandboxDispatchAuthorization, request port.CompileRequest) (result port.CompileResult, returnErr error) {
	if err := request.Validate(); err != nil {
		return result, err
	}
	op, err := r.newOperation(auth)
	if err != nil {
		return result, err
	}
	expectedPlan, err := BuildCompilePlan(request, r.lock, op.identity)
	if err != nil || !reflect.DeepEqual(op.plan, expectedPlan) {
		if err == nil {
			err = fmt.Errorf("authorized plan does not match the compile request")
		}
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, op.finish()) }()

	groups, err := r.preflightCompile(ctx, op, request)
	if err != nil {
		return result, err
	}
	program, err := r.prepareCompileArtifact(ctx, op, request)
	if err != nil {
		return result, err
	}
	processArtifacts, err := r.prepareProcessArtifacts(ctx, op, "compile", request.SourceBundle.Digest, compileDiagnosticMaxBytes, compileDiagnosticMaxBytes)
	if err != nil {
		return result, err
	}
	if err := op.createVolumes(ctx, request.Limits.OutputBytes); err != nil {
		return result, err
	}
	for _, group := range groups {
		if err := op.runImport(ctx, group); err != nil {
			return result, err
		}
	}
	keeper, err := op.startKeeper(ctx)
	if err != nil {
		return result, err
	}
	_ = keeper
	targetOptions := func(_ port.PlannedResource, callID domain.AttemptCallID) (moby.ContainerCreateOptions, error) {
		return TargetCreateOptions(CompileTarget(request), r.lock, op.identity, op.plan, callID)
	}
	target, targetCall, err := op.createContainer(ctx, port.ContainerTarget, targetOptions)
	if err != nil {
		return result, err
	}
	inspected, err := r.engine.ContainerInspect(ctx, target.id, moby.ContainerInspectOptions{})
	if err != nil {
		return result, err
	}
	expectedTarget, _ := targetOptions(target.resource, targetCall)
	if err := VerifyTargetInspect(expectedTarget, inspected); err != nil {
		return result, err
	}
	stdout, stderr, err := processArtifacts.limiters()
	if err != nil {
		return result, err
	}
	execution, executeErr := op.executeTarget(ctx, target, targetCall, request.Limits.Time, nil, stdout, stderr)
	if execution.Record.SchemaVersion != "" {
		executeErr = errors.Join(executeErr, finalizeProcessArtifacts(processArtifacts, execution.Record, r.limits.CleanupTimeout))
	}
	if executeErr != nil {
		return result, executeErr
	}
	result = port.CompileResult{
		CallTrace: op.callTrace(targetCall), Outcome: domain.CompileInfraError,
		Stdout: &processArtifacts.stdout.pending, Stderr: &processArtifacts.stderr.pending, Execution: &processArtifacts.execution.pending,
		Details: map[string]string{"process_outcome": string(execution.Outcome)},
	}
	if execution.Outcome == domain.ProcessExited && execution.Record.ExitCode != nil {
		if *execution.Record.ExitCode != 0 {
			result.Outcome = domain.CompileCE
			return result, nil
		}
		if err := op.runExport(ctx, []*preparedArtifact{program}, request.Limits.OutputBytes); err != nil {
			return port.CompileResult{}, err
		}
		result.CallTrace = op.callTrace(targetCall)
		result.Outcome = domain.CompileOK
		result.Program = &program.pending
	}
	return result, nil
}

func (r *Runner) Run(ctx context.Context, auth port.SandboxDispatchAuthorization, request port.RunRequest) (result port.RunResult, returnErr error) {
	if err := request.Validate(); err != nil {
		return result, err
	}
	op, err := r.newOperation(auth)
	if err != nil {
		return result, err
	}
	expectedPlan, err := BuildRunPlan(request, r.lock, op.identity)
	if err != nil || !reflect.DeepEqual(op.plan, expectedPlan) {
		if err == nil {
			err = fmt.Errorf("authorized plan does not match the run request")
		}
		return result, err
	}
	defer func() { returnErr = errors.Join(returnErr, op.finish()) }()
	groups, stdin, err := r.preflightRun(ctx, op, request)
	if err != nil {
		return result, err
	}
	outputs, outputTotal, err := r.prepareRunArtifacts(ctx, op, request)
	if err != nil {
		return result, err
	}
	processArtifacts, err := r.prepareProcessArtifacts(ctx, op, "run", request.Program.Digest, request.Limits.StdoutBytes, request.Limits.StderrBytes)
	if err != nil {
		return result, err
	}
	if err := op.createVolumes(ctx, outputTotal); err != nil {
		return result, err
	}
	for _, group := range groups {
		if err := op.runImport(ctx, group); err != nil {
			return result, err
		}
	}
	if len(outputs) != 0 {
		if _, err := op.startKeeper(ctx); err != nil {
			return result, err
		}
	}
	targetOptions := func(_ port.PlannedResource, callID domain.AttemptCallID) (moby.ContainerCreateOptions, error) {
		return TargetCreateOptions(RunTarget(request), r.lock, op.identity, op.plan, callID)
	}
	target, targetCall, err := op.createContainer(ctx, port.ContainerTarget, targetOptions)
	if err != nil {
		return result, err
	}
	inspected, err := r.engine.ContainerInspect(ctx, target.id, moby.ContainerInspectOptions{})
	if err != nil {
		return result, err
	}
	expectedTarget, _ := targetOptions(target.resource, targetCall)
	if err := VerifyTargetInspect(expectedTarget, inspected); err != nil {
		return result, err
	}
	stdout, stderr, err := processArtifacts.limiters()
	if err != nil {
		return result, err
	}
	execution, executeErr := op.executeTarget(ctx, target, targetCall, request.Limits.Time, stdin, stdout, stderr)
	if execution.Record.SchemaVersion != "" {
		executeErr = errors.Join(executeErr, finalizeProcessArtifacts(processArtifacts, execution.Record, r.limits.CleanupTimeout))
	}
	if executeErr != nil {
		return result, executeErr
	}
	if execution.Outcome == domain.ProcessExited && execution.Record.ExitCode != nil && *execution.Record.ExitCode == 0 && len(outputs) != 0 {
		if err := op.runExport(ctx, outputs, outputTotal); err != nil {
			return result, err
		}
	}
	artifacts := make([]domain.PendingArtifact, 0, len(outputs))
	for _, output := range outputs {
		if output.finalized {
			artifacts = append(artifacts, output.pending)
		}
	}
	var exitCode *int
	if execution.Outcome == domain.ProcessExited {
		exitCode = execution.Record.ExitCode
	}
	return port.RunResult{
		CallTrace: op.callTrace(targetCall), Outcome: execution.Outcome, ExitCode: exitCode, Signal: execution.Record.Signal,
		Metrics: port.ProcessMetrics{WallTime: execution.Record.WallTime, CPUTime: execution.Record.CPUTime, PeakRSSBytes: execution.Record.PeakRSSBytes, OOMKilled: execution.Record.OOMKilled},
		Stdout:  &processArtifacts.stdout.pending, Stderr: &processArtifacts.stderr.pending, Execution: &processArtifacts.execution.pending,
		Outputs: artifacts,
	}, nil
}

func (r *Runner) Probe(context.Context, port.SandboxDispatchAuthorization, port.DockerProbeRequest) (port.DockerProbeResult, error) {
	return port.DockerProbeResult{}, fmt.Errorf("Docker probe harness is not installed")
}

func (r *Runner) preflightCompile(ctx context.Context, op *operation, request port.CompileRequest) ([]payloadGroup, error) {
	volumes := volumeResources(op.plan)
	if len(volumes) < 1 {
		return nil, fmt.Errorf("compile input volume is missing")
	}
	files := make([]transfer.StreamFile, 0, len(request.SourceBundle.Files))
	for _, source := range request.SourceBundle.Files {
		data, err := r.readVerified(ctx, source.Blob)
		if err != nil {
			return nil, err
		}
		files = append(files, transfer.StreamFile{Path: source.Path, Mode: transfer.FrameModeRegular, Data: data})
	}
	return []payloadGroup{{volume: volumes[0], files: files}}, nil
}

func (r *Runner) preflightRun(ctx context.Context, op *operation, request port.RunRequest) ([]payloadGroup, *[]byte, error) {
	volumes := volumeResources(op.plan)
	if len(volumes) < 1 {
		return nil, nil, fmt.Errorf("program volume is missing")
	}
	program, err := r.readVerified(ctx, request.Program)
	if err != nil {
		return nil, nil, err
	}
	groups := []payloadGroup{{volume: volumes[0], files: []transfer.StreamFile{{Path: "main", Mode: transfer.FrameModeExecutable, Data: program}}}}
	var stdin *[]byte
	if request.Stdin != nil {
		data, err := r.readVerified(ctx, *request.Stdin)
		if err != nil {
			return nil, nil, err
		}
		stdin = &data
	}
	if len(request.Files) != 0 {
		files := make([]transfer.StreamFile, 0, len(request.Files))
		for _, input := range request.Files {
			data, err := r.readVerified(ctx, input.Blob)
			if err != nil {
				return nil, nil, err
			}
			files = append(files, transfer.StreamFile{Path: input.Path, Mode: transfer.FrameModeRegular, Data: data})
		}
		groups = append(groups, payloadGroup{volume: volumes[1], files: files})
	}
	return groups, stdin, nil
}

func (r *Runner) readVerified(ctx context.Context, expected domain.BlobRef) ([]byte, error) {
	reader, err := r.blobs.OpenVerified(ctx, expected)
	if err != nil {
		return nil, err
	}
	if reader.BlobRef() != expected {
		_ = reader.Close()
		return nil, fmt.Errorf("verified reader identity does not match requested blob")
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, expected.Size+1))
	closeErr := reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if int64(len(data)) != expected.Size || domain.SumBytes(data) != expected.Digest {
		return nil, fmt.Errorf("verified blob bytes do not match %s", expected.Digest)
	}
	return data, nil
}

func (r *Runner) prepareCompileArtifact(ctx context.Context, op *operation, request port.CompileRequest) (*preparedArtifact, error) {
	digest := request.SourceBundle.Digest
	declaration := port.ArtifactDeclaration{
		MediaType: "application/vnd.cpgen.executable", Role: domain.ArtifactProgram, LogicalPath: "program/main", MaxBytes: request.Limits.OutputBytes,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: ExecutionProtocolDockerDirectV2, InputDigest: &digest},
	}
	writer, err := r.artifacts.Prepare(ctx, declaration)
	if err != nil {
		return nil, err
	}
	prepared := &preparedArtifact{declaration: declaration, frame: port.OutputDeclaration{Path: "main", MaxBytes: request.Limits.OutputBytes}, wantMode: transfer.FrameModeExecutable, writer: writer}
	op.writers = append(op.writers, prepared)
	return prepared, nil
}

func (r *Runner) prepareRunArtifacts(ctx context.Context, op *operation, request port.RunRequest) ([]*preparedArtifact, int64, error) {
	outputs := make([]*preparedArtifact, 0, len(request.Outputs))
	var total int64
	for _, output := range request.Outputs {
		declaration := port.ArtifactDeclaration{
			MediaType: "application/octet-stream", Role: domain.ArtifactOutput, LogicalPath: output.Path, MaxBytes: output.MaxBytes,
			Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: ExecutionProtocolDockerDirectV2, InputDigest: &request.Program.Digest},
		}
		writer, err := r.artifacts.Prepare(ctx, declaration)
		if err != nil {
			return nil, 0, err
		}
		prepared := &preparedArtifact{declaration: declaration, frame: output, wantMode: transfer.FrameModeRegular, writer: writer}
		op.writers = append(op.writers, prepared)
		outputs = append(outputs, prepared)
		total += output.MaxBytes
	}
	return outputs, total, nil
}

func (op *operation) createVolumes(ctx context.Context, outputBytes int64) error {
	for _, resource := range op.plan.Resources {
		if resource.Kind != port.ResourceVolume {
			continue
		}
		labels, err := ResourceLabels(op.identity, op.plan, resource, nil)
		if err != nil {
			return err
		}
		driverOptions := map[string]string(nil)
		if resource.Role == port.ResourceOutput {
			if outputBytes <= 0 {
				return fmt.Errorf("output volume requires a positive declared byte budget")
			}
			driverOptions = outputVolumeOptions(outputBytes)
		}
		owned := &ownedVolume{resource: resource, name: resource.DeterministicName, labels: maps.Clone(labels), driver: "local", options: maps.Clone(driverOptions)}
		op.volumes = append(op.volumes, owned)
		if err := op.runner.watchdog.BeforeCreate(ctx, resource, maps.Clone(labels)); err != nil {
			return err
		}
		created, err := op.runner.engine.VolumeCreate(ctx, moby.VolumeCreateOptions{Name: owned.name, Driver: owned.driver, DriverOpts: driverOptions, Labels: labels})
		if err != nil {
			return err
		}
		if err := verifyVolumeOwnership(created.Volume, owned); err != nil {
			return err
		}
		inspected, err := op.runner.engine.VolumeInspect(ctx, owned.name, moby.VolumeInspectOptions{})
		if err != nil {
			return err
		}
		if err := verifyVolumeOwnership(inspected.Volume, owned); err != nil {
			return err
		}
		if err := op.runner.watchdog.ResourceCreated(ctx, resource, owned.name); err != nil {
			return err
		}
	}
	return nil
}

func outputVolumeOptions(bytes int64) map[string]string {
	return map[string]string{
		"type": "tmpfs", "device": "tmpfs",
		"o": fmt.Sprintf("size=%d,uid=65532,gid=65532,nosuid,nodev,noexec", bytes),
	}
}

func verifyVolumeOwnership(actual volume.Volume, expected *ownedVolume) error {
	if actual.Name != expected.name || actual.Driver != expected.driver || !maps.Equal(actual.Labels, expected.labels) || !maps.Equal(actual.Options, expected.options) {
		return fmt.Errorf("volume %q ownership or options do not match the immutable plan", expected.name)
	}
	return nil
}

type containerOptionsFactory func(port.PlannedResource, domain.AttemptCallID) (moby.ContainerCreateOptions, error)

func (op *operation) createContainer(ctx context.Context, role port.ContainerRole, factory containerOptionsFactory) (*ownedContainer, domain.AttemptCallID, error) {
	resource, err := op.nextContainer(role)
	if err != nil {
		return nil, "", err
	}
	grant, err := op.auth.ClaimNextContainer(ctx, role)
	if err != nil {
		return nil, "", err
	}
	callID := grant.Auth.CallID()
	if grant.Role != role || grant.Auth.RunID() != op.identity.RunID || grant.Auth.AttemptID() != op.identity.AttemptID || grant.Auth.LeaseEpoch() != op.identity.LeaseEpoch {
		return nil, "", fmt.Errorf("container grant identity mismatch")
	}
	options, err := factory(resource, callID)
	if err != nil {
		return nil, "", err
	}
	labels := maps.Clone(options.Config.Labels)
	owned := &ownedContainer{resource: resource, name: resource.DeterministicName, labels: labels}
	op.containers = append(op.containers, owned)
	if err := op.runner.watchdog.BeforeCreate(ctx, resource, maps.Clone(labels)); err != nil {
		return nil, "", err
	}
	op.physical = append(op.physical, callID)
	created, err := op.runner.engine.ContainerCreate(ctx, options)
	if err != nil {
		return nil, callID, err
	}
	if created.ID == "" {
		return nil, callID, fmt.Errorf("Docker returned an empty container ID")
	}
	owned.id = created.ID
	if err := op.runner.watchdog.ResourceCreated(ctx, resource, created.ID); err != nil {
		return nil, callID, err
	}
	inspected, err := op.runner.engine.ContainerInspect(ctx, created.ID, moby.ContainerInspectOptions{})
	if err != nil {
		return nil, callID, err
	}
	if err := verifyContainerOwnership(inspected, owned); err != nil {
		return nil, callID, err
	}
	if role != port.ContainerTarget {
		if err := VerifyHelperInspect(options, inspected); err != nil {
			return nil, callID, err
		}
	}
	if role == port.ContainerTarget {
		value := callID
		op.targetCall = &value
	}
	return owned, callID, nil
}

func (op *operation) nextContainer(role port.ContainerRole) (port.PlannedResource, error) {
	for op.nextContainerIndex < len(op.plan.Resources) && op.plan.Resources[op.nextContainerIndex].Kind != port.ResourceContainer {
		op.nextContainerIndex++
	}
	if op.nextContainerIndex >= len(op.plan.Resources) {
		return port.PlannedResource{}, fmt.Errorf("plan has no remaining container")
	}
	resource := op.plan.Resources[op.nextContainerIndex]
	op.nextContainerIndex++
	want, err := resourceRoleToContainer(resource.Role)
	if err != nil || want != role {
		return port.PlannedResource{}, fmt.Errorf("next planned container role is %q, got %q", want, role)
	}
	return resource, nil
}

func resourceRoleToContainer(role port.ResourceRole) (port.ContainerRole, error) {
	switch role {
	case port.ResourceImport:
		return port.ContainerImport, nil
	case port.ResourceKeeper:
		return port.ContainerKeeper, nil
	case port.ResourceTarget:
		return port.ContainerTarget, nil
	case port.ResourceExport:
		return port.ContainerExport, nil
	default:
		return "", fmt.Errorf("resource role %q is not a container", role)
	}
}

func verifyContainerOwnership(actual moby.ContainerInspectResult, expected *ownedContainer) error {
	if actual.Container.Config == nil || actual.Container.ID == "" || actual.Container.ID != expected.id && expected.id != "" ||
		actual.Container.Config.Labels == nil || !maps.Equal(actual.Container.Config.Labels, expected.labels) ||
		trimContainerName(actual.Container.Name) != expected.name {
		return fmt.Errorf("container %q ownership does not match the immutable plan", expected.name)
	}
	return nil
}

func trimContainerName(name string) string {
	if len(name) != 0 && name[0] == '/' {
		return name[1:]
	}
	return name
}

func waitContainer(ctx context.Context, engine Engine, id string) (int64, error) {
	wait := engine.ContainerWait(ctx, id, moby.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	resultChannel, errorChannel := wait.Result, wait.Error
	for resultChannel != nil || errorChannel != nil {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case result, ok := <-resultChannel:
			if !ok {
				resultChannel = nil
				continue
			}
			if result.Error != nil {
				return 0, fmt.Errorf("container wait: %s", result.Error.Message)
			}
			return result.StatusCode, nil
		case err, ok := <-errorChannel:
			if !ok {
				errorChannel = nil
				continue
			}
			if err != nil {
				return 0, err
			}
		}
	}
	return 0, fmt.Errorf("container wait ended without a result")
}

func (op *operation) callTrace(resultCall domain.AttemptCallID) domain.CallTrace {
	call := resultCall
	return domain.CallTrace{
		LogicalOperationID: op.identity.LogicalOperationID, DispatchKind: domain.DispatchDispatched,
		ResultAttemptCallID: &call, PhysicalAttemptCallIDs: slices.Clone(op.physical),
	}
}

func (op *operation) finish() error {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), op.runner.limits.CleanupTimeout)
	defer cancel()
	var failures []error
	for _, writer := range op.writers {
		if !writer.finalized {
			if err := writer.writer.Abort(cleanupCtx); err != nil {
				failures = append(failures, fmt.Errorf("abort artifact %q: %w", writer.declaration.LogicalPath, err))
			}
		}
	}
	if err := op.auth.AbortRemaining(cleanupCtx); err != nil {
		failures = append(failures, fmt.Errorf("abort unused container claims: %w", err))
	}
	if err := op.cleanup(cleanupCtx); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (op *operation) cleanup(ctx context.Context) error {
	var failures []error
	for index := len(op.containers) - 1; index >= 0; index-- {
		owned := op.containers[index]
		lookup := owned.id
		if lookup == "" {
			lookup = owned.name
		}
		inspected, err := op.runner.engine.ContainerInspect(ctx, lookup, moby.ContainerInspectOptions{})
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("inspect cleanup container %q: %w", owned.name, err))
			continue
		}
		if err := verifyContainerOwnership(inspected, owned); err != nil {
			failures = append(failures, err)
			continue
		}
		id := inspected.Container.ID
		if inspected.Container.State != nil && inspected.Container.State.Running {
			if owned.resource.Role == port.ResourceTarget {
				if _, err := portableStop(ctx, op.runner.engine, id, func(result moby.ContainerInspectResult) error {
					return verifyContainerOwnership(result, owned)
				}); err != nil {
					failures = append(failures, fmt.Errorf("prove stopped cleanup target %q: %w", owned.name, err))
					continue
				}
			} else {
				zero := 0
				if _, err := op.runner.engine.ContainerStop(ctx, id, moby.ContainerStopOptions{Signal: "SIGTERM", Timeout: &zero}); err != nil && !errdefs.IsNotFound(err) {
					failures = append(failures, fmt.Errorf("stop cleanup container %q: %w", owned.name, err))
					continue
				}
			}
		}
		if _, err := op.runner.engine.ContainerRemove(ctx, id, moby.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("remove cleanup container %q: %w", owned.name, err))
		}
	}
	for index := len(op.volumes) - 1; index >= 0; index-- {
		owned := op.volumes[index]
		inspected, err := op.runner.engine.VolumeInspect(ctx, owned.name, moby.VolumeInspectOptions{})
		if errdefs.IsNotFound(err) {
			continue
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("inspect cleanup volume %q: %w", owned.name, err))
			continue
		}
		if err := verifyVolumeOwnership(inspected.Volume, owned); err != nil {
			failures = append(failures, err)
			continue
		}
		if _, err := op.runner.engine.VolumeRemove(ctx, owned.name, moby.VolumeRemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("remove cleanup volume %q: %w", owned.name, err))
		}
	}
	return errors.Join(failures...)
}

var _ port.DockerSandbox = (*Runner)(nil)
