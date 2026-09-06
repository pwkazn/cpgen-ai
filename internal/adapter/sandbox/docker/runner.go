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
	"sort"
	"strconv"
	"strings"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
	"cpgen/internal/transfer"
	"cpgen/internal/watchdog"
	"github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/volume"
	moby "github.com/moby/moby/client"
)

type WatchdogController interface {
	TokenDigest() domain.Digest
	Arm(context.Context, watchdog.ControlRecord) (WatchdogSession, error)
}

type WatchdogSession interface {
	PreCreate(context.Context, port.PlannedResource, map[string]string) error
	ResourceCreated(context.Context, port.PlannedResource, string) error
	TargetPhase(context.Context, port.PlannedResource, string, time.Duration) error
	Stopped(context.Context, port.PlannedResource, string) error
	Cleaned(context.Context) error
	Close() error
}

// WatchdogSessionEvidence exposes the exact fsynced control-file reference and
// digest produced by Arm. Lifecycle persistence rejects synthetic endpoint
// hashes; legacy sessions without this evidence cannot authorize a durable
// SandboxExecution.
type WatchdogSessionEvidence interface {
	ControlRecordRef() string
	ControlFileDigest() domain.Digest
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
	Lifecycle            port.SandboxLifecycleRecorder
	CallLedger           port.CallLedger
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
	lifecycle      port.SandboxLifecycleRecorder
	callLedger     port.CallLedger
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
	if err := options.Watchdog.TokenDigest().Validate(); err != nil {
		return nil, fmt.Errorf("watchdog token digest: %w", err)
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
		lifecycle: options.Lifecycle, callLedger: options.CallLedger,
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
	physicalGrants     map[domain.AttemptCallID]domain.DispatchGrant
	physicalSettled    map[domain.AttemptCallID]bool
	volumeGrants       map[domain.AttemptCallID]port.VolumeDispatchGrant
	targetCall         *domain.AttemptCallID
	containers         []*ownedContainer
	volumes            []*ownedVolume
	writers            []*preparedArtifact
	watchdog           WatchdogSession
	targetPhase        bool
	targetStopped      bool
	lifecycleResources map[int]domain.SandboxResource
	lifecycleVersion   int64
	lifecycleAt        time.Time
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
		LogicalOperationID: auth.LogicalOperationID(), RunID: auth.RunID(), AttemptID: auth.AttemptID(),
		ScopeDigest: auth.ScopeDigest(), PlanDigest: auth.PlanDigest(), EngineIdentityDigest: r.engineIdentity,
	}
	if executionID, ok := port.SandboxExecutionIDOf(auth); ok {
		probeIdentity.SandboxExecutionID = executionID
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
		RunID: auth.RunID(), AttemptID: auth.AttemptID(), LogicalOperationID: auth.LogicalOperationID(),
		OperationNonce: match[1], EngineIdentityDigest: r.engineIdentity,
	}
	if executionID, ok := port.SandboxExecutionIDOf(auth); ok {
		identity.SandboxExecutionID = executionID
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if plan.TransferBytesMax > r.limits.MaxTransferBytes {
		return nil, fmt.Errorf("planned transfer bytes %d exceed control limit %d", plan.TransferBytesMax, r.limits.MaxTransferBytes)
	}
	if identity.SandboxExecutionID == "" {
		return nil, fmt.Errorf("SandboxExecutionID is required for every Docker authorization")
	}
	if r.lifecycle == nil {
		return nil, fmt.Errorf("sandbox lifecycle recorder is required for SandboxExecutionID authorization")
	}
	if _, ok := r.lifecycle.(port.SandboxLifecycleReader); !ok {
		return nil, fmt.Errorf("sandbox lifecycle reader is required for SandboxExecutionID authorization")
	}
	if _, ok := r.lifecycle.(port.SandboxCleanupRecorder); !ok {
		return nil, fmt.Errorf("sandbox cleanup proof recorder is required for SandboxExecutionID authorization")
	}
	if r.callLedger == nil {
		return nil, fmt.Errorf("call ledger is required for SandboxExecutionID authorization")
	}
	op := &operation{runner: r, auth: auth, plan: plan, identity: identity, physicalGrants: make(map[domain.AttemptCallID]domain.DispatchGrant), physicalSettled: make(map[domain.AttemptCallID]bool), volumeGrants: make(map[domain.AttemptCallID]port.VolumeDispatchGrant)}
	return op, nil
}

func (op *operation) prepareLifecycle(ctx context.Context, controlRef string, controlDigest domain.Digest) error {
	stage := domain.StageName("sandbox")
	if value, ok := op.auth.(interface{ StageName() domain.StageName }); ok && value.StageName() != "" {
		stage = value.StageName()
	}
	if strings.TrimSpace(controlRef) == "" {
		return fmt.Errorf("watchdog control reference is required")
	}
	if err := controlDigest.Validate(); err != nil {
		return fmt.Errorf("watchdog control file digest: %w", err)
	}
	now := stableExecutionTime(string(op.identity.SandboxExecutionID), string(op.plan.PlanDigest))
	reader, ok := op.runner.lifecycle.(port.SandboxLifecycleReader)
	if !ok {
		return fmt.Errorf("sandbox lifecycle reader is required")
	}
	if existing, err := reader.GetSandboxExecution(ctx, op.identity.SandboxExecutionID); err == nil {
		if existing.RunID != op.identity.RunID || existing.AttemptID != op.identity.AttemptID || existing.StageName != stage || existing.LogicalOperationID != op.identity.LogicalOperationID || existing.ScopeDigest != op.auth.ScopeDigest() || existing.PlanDigest != op.plan.PlanDigest || existing.EngineIdentityDigest != op.identity.EngineIdentityDigest || existing.WatchdogControlRef != controlRef || existing.WatchdogTokenDigest != op.runner.watchdog.TokenDigest() {
			return fmt.Errorf("persisted SandboxExecution identity differs from authorization")
		}
		controlReader, ok := op.runner.lifecycle.(port.SandboxWatchdogReader)
		if !ok {
			return fmt.Errorf("sandbox watchdog evidence reader is required for execution replay")
		}
		control, controlErr := controlReader.GetSandboxWatchdogControl(ctx, existing.ID)
		if controlErr != nil {
			return fmt.Errorf("read persisted watchdog evidence: %w", controlErr)
		}
		if control.ControlID != controlRef || control.ProcessRecordRef != controlRef || control.ControlFileDigest != controlDigest || control.TokenDigest != op.runner.watchdog.TokenDigest() {
			return fmt.Errorf("persisted watchdog control evidence differs from replay")
		}
		op.lifecycleAt = existing.CreatedAt
		op.lifecycleVersion = existing.LifecycleVersion
		op.lifecycleResources = make(map[int]domain.SandboxResource, len(existing.Resources))
		for _, resource := range existing.Resources {
			op.lifecycleResources[resource.PlanOrdinal] = resource
		}
		return nil
	} else if err != nil && !strings.Contains(strings.ToLower(err.Error()), "not found") && !strings.Contains(strings.ToLower(err.Error()), "does not exist") {
		return err
	}
	resources := make([]domain.SandboxResource, 0, len(op.plan.Resources))
	op.lifecycleResources = make(map[int]domain.SandboxResource, len(op.plan.Resources))
	for _, planned := range op.plan.Resources {
		id := stableSandboxID("resource", string(op.identity.SandboxExecutionID), strconv.Itoa(planned.Ordinal))
		resource := domain.SandboxResource{ID: domain.SandboxResourceID(id), ExecutionID: op.identity.SandboxExecutionID, PlanOrdinal: planned.Ordinal, Kind: string(planned.Kind), Role: string(planned.Role), DeterministicName: planned.DeterministicName, ExpectedLabelsDigest: planned.ExpectedLabelsDigest, EngineIdentityDigest: op.identity.EngineIdentityDigest, Phase: domain.SandboxResourcePlanned, Version: 1, CreatedAt: now, UpdatedAt: now}
		resources = append(resources, resource)
		op.lifecycleResources[planned.Ordinal] = resource
	}
	key := stableSandboxKey("prepare", string(op.identity.SandboxExecutionID), string(op.plan.PlanDigest))
	execution, err := op.runner.lifecycle.PrepareExecution(ctx, domain.PrepareExecutionRequest{ExecutionID: op.identity.SandboxExecutionID, RunID: op.identity.RunID, AttemptID: op.identity.AttemptID, StageName: stage, LogicalOperationID: op.identity.LogicalOperationID, ScopeDigest: op.auth.ScopeDigest(), PlanDigest: op.plan.PlanDigest, EngineIdentityDigest: op.identity.EngineIdentityDigest, Resources: resources, WatchdogControlRef: controlRef, WatchdogTokenDigest: op.runner.watchdog.TokenDigest(), SafetyDeadlineUTC: now.Add(time.Hour), CleanupDeadlineUTC: now.Add(2 * time.Hour), IdempotencyKey: key, At: now})
	if err != nil {
		return err
	}
	op.lifecycleVersion = execution.LifecycleVersion
	op.lifecycleAt = execution.CreatedAt
	if len(execution.Resources) == len(op.plan.Resources) {
		op.lifecycleResources = make(map[int]domain.SandboxResource, len(execution.Resources))
		for _, resource := range execution.Resources {
			op.lifecycleResources[resource.PlanOrdinal] = resource
		}
	}
	armAt := stableLifecycleTime(execution.CreatedAt, "arm")
	if err := op.runner.lifecycle.RecordWatchdogArmed(ctx, domain.WatchdogArmed{ExecutionID: execution.ID, ExpectedVersion: op.lifecycleVersion, ControlRecordRef: controlRef, ControlFileDigest: controlDigest, TokenDigest: op.runner.watchdog.TokenDigest(), IdempotencyKey: stableSandboxKey("arm", string(execution.ID)), At: armAt}); err != nil {
		return err
	}
	op.lifecycleVersion++
	return nil
}

func (op *operation) armWatchdog(ctx context.Context, programLimit time.Duration) error {
	if op.watchdog != nil {
		return fmt.Errorf("watchdog is already armed")
	}
	if programLimit <= 0 {
		return fmt.Errorf("program limit must be positive")
	}
	now := op.lifecycleAt
	if now.IsZero() {
		now = stableExecutionTime(string(op.identity.SandboxExecutionID), string(op.plan.PlanDigest))
	}
	deadline := now.Add(programLimit + op.runner.limits.CleanupTimeout + 5*time.Second)
	if contextDeadline, ok := ctx.Deadline(); ok {
		bounded := contextDeadline.Add(5 * time.Second)
		if bounded.After(now) && bounded.Before(deadline) {
			deadline = bounded
		}
	}
	if !deadline.After(now) {
		return fmt.Errorf("watchdog safety envelope is already exhausted")
	}
	record := watchdog.ControlRecord{
		SchemaVersion: watchdog.ControlRecordSchemaVersion,
		TokenDigest:   op.runner.watchdog.TokenDigest(), EngineEndpoint: op.runner.config.EngineEndpoint,
		EngineIdentityDigest: op.identity.EngineIdentityDigest, LogicalOperationID: op.identity.LogicalOperationID,
		RunID: op.identity.RunID, AttemptID: op.identity.AttemptID, SandboxExecutionID: op.identity.SandboxExecutionID,
		ScopeDigest: op.auth.ScopeDigest(),
		Plan:        op.plan.Clone(), SafetyDeadlineUTC: deadline.UTC(),
	}
	if err := record.Validate(); err != nil {
		return err
	}
	session, err := op.runner.watchdog.Arm(ctx, record)
	if err != nil {
		return err
	}
	if session == nil {
		return fmt.Errorf("watchdog Arm returned no session")
	}
	op.watchdog = session
	evidence, ok := session.(WatchdogSessionEvidence)
	if !ok {
		_ = session.Close()
		return fmt.Errorf("watchdog session did not provide durable control evidence")
	}
	if err := op.prepareLifecycle(ctx, evidence.ControlRecordRef(), evidence.ControlFileDigest()); err != nil {
		_ = session.Close()
		return err
	}
	return nil
}

func (op *operation) lifecycleBeginCreate(ctx context.Context, planned port.PlannedResource, labels map[string]string, callID *domain.AttemptCallID) error {
	if op.runner.lifecycle == nil || op.identity.SandboxExecutionID == "" {
		return nil
	}
	resource, ok := op.lifecycleResources[planned.Ordinal]
	if !ok {
		return fmt.Errorf("sandbox resource %d was not durably prepared", planned.Ordinal)
	}
	key := stableSandboxKey("creating", string(resource.ID))
	pre, err := op.runner.lifecycle.BeginResourceCreate(ctx, domain.BeginResourceCreate{ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: resource.Version, IdempotencyKey: key, At: stableLifecycleTime(resource.CreatedAt, "creating")})
	if err != nil {
		return err
	}
	resource = pre.Resource
	op.lifecycleResources[planned.Ordinal] = resource
	if err := op.watchdog.PreCreate(ctx, planned, maps.Clone(labels)); err != nil {
		return err
	}
	reader, ok := op.runner.lifecycle.(port.SandboxLifecycleReader)
	if !ok {
		return fmt.Errorf("sandbox lifecycle reader is required")
	}
	execution, err := reader.GetSandboxExecution(ctx, resource.ExecutionID)
	if err != nil {
		return err
	}
	ackKey := stableSandboxKey("precreate", string(resource.ID))
	if err := op.runner.lifecycle.RecordPreCreateACK(ctx, domain.PreCreateACK{ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ResourceVersion: pre.Version, LabelsDigest: digestLabels(labels), WatchdogRecordRef: execution.WatchdogControlRef, IdempotencyKey: ackKey, At: stableLifecycleTime(resource.CreatedAt, "precreate")}); err != nil {
		return err
	}
	dispatchKey := stableSandboxKey("dispatch", string(resource.ID))
	resource, err = op.runner.lifecycle.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: pre.Version, Phase: domain.SandboxResourceDispatching, PhysicalCallID: callID, EngineIdentityDigest: resource.EngineIdentityDigest, LabelsDigest: digestLabels(labels), IdempotencyKey: dispatchKey, At: stableLifecycleTime(resource.CreatedAt, "dispatch")})
	if err != nil {
		return err
	}
	op.lifecycleResources[planned.Ordinal] = resource
	return nil
}

func (op *operation) lifecycleCompleteCreate(ctx context.Context, planned port.PlannedResource, engineID string, labels map[string]string, callID *domain.AttemptCallID) error {
	if op.runner.lifecycle == nil || op.identity.SandboxExecutionID == "" {
		return nil
	}
	resource, ok := op.lifecycleResources[planned.Ordinal]
	if !ok {
		return fmt.Errorf("sandbox resource %d was not durably prepared", planned.Ordinal)
	}
	key := stableSandboxKey("complete", string(resource.ID), engineID)
	resource, err := op.runner.lifecycle.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: resource.Version, Phase: domain.SandboxResourceCompleted, PhysicalCallID: callID, EngineResourceID: engineID, EngineIdentityDigest: resource.EngineIdentityDigest, LabelsDigest: digestLabels(labels), IdempotencyKey: key, At: stableLifecycleTime(resource.CreatedAt, "complete")})
	if err != nil {
		return err
	}
	op.lifecycleResources[planned.Ordinal] = resource
	return nil
}

func (op *operation) lifecycleAdvanceStarted(ctx context.Context, planned port.PlannedResource, engineID string) error {
	if op.runner.lifecycle == nil || op.identity.SandboxExecutionID == "" {
		return nil
	}
	resource, ok := op.lifecycleResources[planned.Ordinal]
	if !ok {
		return fmt.Errorf("sandbox resource %d was not durably prepared", planned.Ordinal)
	}
	key := stableSandboxKey("started", string(resource.ID), engineID)
	resource, err := op.runner.lifecycle.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: resource.Version, Phase: domain.SandboxResourceStarted, EngineResourceID: engineID, EngineIdentityDigest: resource.EngineIdentityDigest, IdempotencyKey: key, At: stableLifecycleTime(resource.CreatedAt, "started")})
	if err != nil {
		return err
	}
	op.lifecycleResources[planned.Ordinal] = resource
	return nil
}

func (op *operation) lifecycleMarkUnknown(ctx context.Context, planned port.PlannedResource) error {
	if op.runner.lifecycle == nil || op.identity.SandboxExecutionID == "" {
		return nil
	}
	resource, ok := op.lifecycleResources[planned.Ordinal]
	if !ok {
		return fmt.Errorf("sandbox resource %d was not durably prepared", planned.Ordinal)
	}
	if resource.Phase == domain.SandboxResourceUnknown {
		return nil
	}
	if resource.Phase != domain.SandboxResourceCreating && resource.Phase != domain.SandboxResourceDispatching && resource.Phase != domain.SandboxResourceSent {
		return fmt.Errorf("sandbox resource %d cannot be marked UNKNOWN from %s", planned.Ordinal, resource.Phase)
	}
	updated, err := op.runner.lifecycle.AdvanceResource(ctx, domain.AdvanceResourceRequest{ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: resource.Version, Phase: domain.SandboxResourceUnknown, PhysicalCallID: resource.PhysicalCallID, EngineResourceID: resource.EngineResourceID, EngineIdentityDigest: resource.EngineIdentityDigest, LabelsDigest: resource.LabelsDigest, IdempotencyKey: stableSandboxKey("unknown", string(resource.ID)), At: stableLifecycleTime(resource.CreatedAt, "unknown")})
	if err != nil {
		return err
	}
	op.lifecycleResources[planned.Ordinal] = updated
	return nil
}

// lifecycleSettlementFailure records durable uncertainty after an external
// create has crossed (or may have crossed) the Engine boundary. Once the
// resource is already COMPLETED/STARTED, UNKNOWN is no longer a legal
// monotone transition; CLEANUP_PENDING is the durable cleanup obligation.
func (op *operation) lifecycleSettlementFailure(ctx context.Context, planned port.PlannedResource) error {
	if op.runner.lifecycle == nil || op.identity.SandboxExecutionID == "" {
		return nil
	}
	resource, ok := op.lifecycleResources[planned.Ordinal]
	if !ok {
		return fmt.Errorf("sandbox resource %d was not durably prepared", planned.Ordinal)
	}
	// A persistence call may have committed before returning an error. Refresh
	// the authoritative row before selecting the monotone settlement edge so a
	// stale in-memory phase cannot strand a COMPLETED resource.
	if reader, ok := op.runner.lifecycle.(port.SandboxLifecycleReader); ok {
		if execution, err := reader.GetSandboxExecution(ctx, resource.ExecutionID); err == nil {
			for _, current := range execution.Resources {
				if current.PlanOrdinal == planned.Ordinal {
					resource = current
					op.lifecycleResources[planned.Ordinal] = current
					break
				}
			}
		}
	}
	switch resource.Phase {
	case domain.SandboxResourceCreating, domain.SandboxResourceDispatching, domain.SandboxResourceSent:
		return op.lifecycleMarkUnknown(ctx, planned)
	case domain.SandboxResourceUnknown, domain.SandboxResourceCleanupPending,
		domain.SandboxResourceStopped, domain.SandboxResourceCleaned, domain.SandboxResourceInterrupted:
		return nil
	case domain.SandboxResourceCompleted, domain.SandboxResourceStarted:
		updated, err := op.runner.lifecycle.AdvanceResource(ctx, domain.AdvanceResourceRequest{
			ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: resource.Version,
			Phase: domain.SandboxResourceCleanupPending, EngineResourceID: resource.EngineResourceID,
			EngineIdentityDigest: resource.EngineIdentityDigest, LabelsDigest: resource.LabelsDigest,
			IdempotencyKey: stableSandboxKey("settlement_cleanup", string(resource.ID)), At: stableLifecycleTime(resource.CreatedAt, "settlement_cleanup"),
		})
		if err == nil {
			op.lifecycleResources[planned.Ordinal] = updated
		}
		return err
	default:
		return fmt.Errorf("sandbox resource %d has unsupported settlement phase %s", planned.Ordinal, resource.Phase)
	}
}

func (op *operation) lifecycleEnsureCleanupPending(ctx context.Context, planned port.PlannedResource) (domain.SandboxResource, error) {
	resource, ok := op.lifecycleResources[planned.Ordinal]
	if !ok {
		return domain.SandboxResource{}, fmt.Errorf("sandbox resource %d was not durably prepared", planned.Ordinal)
	}
	if resource.Phase == domain.SandboxResourceCleanupPending || resource.Phase == domain.SandboxResourceStopped || resource.Phase == domain.SandboxResourceCleaned || resource.Phase == domain.SandboxResourceInterrupted || resource.Phase == domain.SandboxResourcePlanned {
		return resource, nil
	}
	updated, err := op.runner.lifecycle.AdvanceResource(ctx, domain.AdvanceResourceRequest{
		ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: resource.Version,
		Phase: domain.SandboxResourceCleanupPending, PhysicalCallID: resource.PhysicalCallID,
		EngineResourceID: resource.EngineResourceID, EngineIdentityDigest: resource.EngineIdentityDigest,
		LabelsDigest: resource.LabelsDigest, IdempotencyKey: stableSandboxKey("cleanup_resource", string(resource.ID)),
		At: stableLifecycleTime(resource.CreatedAt, "cleanup"),
	})
	if err != nil {
		return resource, err
	}
	op.lifecycleResources[planned.Ordinal] = updated
	return updated, nil
}

func unknownPhysicalCompletion(dispatch domain.DispatchGrant, key string, at time.Time) domain.CompletePhysicalRequest {
	return domain.CompletePhysicalRequest{
		RunID: dispatch.RunID, ExpectedRunVersion: dispatch.ExpectedRunVersion, StageName: dispatch.StageName,
		AttemptID: dispatch.AttemptID, CallRecordID: dispatch.CallRecordID, AttemptCallID: dispatch.AttemptCallID,
		State: domain.PhysicalUnknown, Outcome: domain.PhysicalOutcomeUnknown,
		Failure:        &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown},
		IdempotencyKey: key, At: at,
	}
}

func (op *operation) settlePhysicalUnknown(ctx context.Context, dispatch domain.DispatchGrant, resource port.PlannedResource, at time.Time) error {
	var failures []error
	settleCtx := ctx
	if ctx == nil || ctx.Err() != nil {
		var cancel context.CancelFunc
		settleCtx, cancel = context.WithTimeout(context.Background(), op.runner.limits.CleanupTimeout)
		defer cancel()
	}
	// Once the success settlement is durably recorded, it cannot be changed to
	// UNKNOWN. The lifecycle still needs settlement, but retrying the physical
	// call would turn a later watchdog/lifecycle write failure into an
	// idempotency conflict and obscure the real cleanup obligation.
	if !op.physicalSettled[dispatch.AttemptCallID] {
		if err := op.runner.callLedger.CompletePhysical(settleCtx, unknownPhysicalCompletion(dispatch, stableSandboxKey("physical_complete", string(dispatch.AttemptCallID)), at)); err != nil {
			failures = append(failures, err)
		} else {
			op.physicalSettled[dispatch.AttemptCallID] = true
		}
	}
	if err := op.lifecycleSettlementFailure(settleCtx, resource); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
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
	if err := op.armWatchdog(ctx, request.Limits.Time); err != nil {
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
	if err := op.armWatchdog(ctx, request.Limits.Time); err != nil {
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

func (r *Runner) Probe(ctx context.Context, auth port.SandboxDispatchAuthorization, request port.DockerProbeRequest) (port.DockerProbeResult, error) {
	profile := port.SandboxProfile(request.Profile)
	if !profile.Valid() {
		return port.DockerProbeResult{}, fmt.Errorf("invalid Docker probe profile %q", request.Profile)
	}
	if auth == nil {
		return port.DockerProbeResult{}, fmt.Errorf("Docker probe authorization is required")
	}
	plan := auth.ContainerPlan()
	identity := port.ProbeAuthorizationIdentity{
		LogicalOperationID: auth.LogicalOperationID(), RunID: auth.RunID(), AttemptID: auth.AttemptID(),
		ScopeDigest: auth.ScopeDigest(), PlanDigest: auth.PlanDigest(), EngineIdentityDigest: r.engineIdentity,
	}
	if executionID, ok := port.SandboxExecutionIDOf(auth); ok && executionID != "" {
		identity.SandboxExecutionID = executionID
	}
	if err := identity.Validate(); err != nil {
		return port.DockerProbeResult{}, err
	}
	if err := plan.Validate(); err != nil {
		return port.DockerProbeResult{}, err
	}
	if len(plan.Resources) != 0 || plan.TransferBytesMax != 0 || plan.EngineIdentityDigest != r.engineIdentity || auth.PlanDigest() != plan.PlanDigest {
		return port.DockerProbeResult{}, fmt.Errorf("Engine ping requires an exact empty plan for the configured Engine")
	}
	grant, err := auth.ClaimEnginePing(ctx)
	if err != nil {
		return port.DockerProbeResult{}, err
	}
	callID := grant.CallID()
	if grant.RunID() != auth.RunID() || grant.AttemptID() != auth.AttemptID() || grant.ScopeDigest() != auth.ScopeDigest() {
		return port.DockerProbeResult{}, fmt.Errorf("Engine ping grant identity mismatch")
	}
	trace := domain.CallTrace{
		LogicalOperationID: auth.LogicalOperationID(), DispatchKind: domain.DispatchDispatched,
		PhysicalAttemptCallIDs: []domain.AttemptCallID{callID}, ResultAttemptCallID: &callID,
	}
	result := port.DockerProbeResult{CallTrace: trace}
	doctor, err := NewDoctor(r.engine, r.config, runtime.GOOS)
	if err != nil {
		return result, err
	}
	report, err := doctor.Check(ctx)
	if err != nil {
		return result, err
	}
	if report.EngineIdentityDigest != r.engineIdentity {
		return result, checkFailure(domain.FailureProtocol, domain.FailureIncompatible, fmt.Errorf("Docker Engine identity changed during capability ping"))
	}
	cgroupVersion, err := strconv.Atoi(report.CgroupVersion)
	if err != nil {
		return result, checkFailure(domain.FailureProtocol, domain.FailureIncompatible, fmt.Errorf("invalid cgroup version %q", report.CgroupVersion))
	}
	result.Capabilities = port.CapabilitySnapshot{
		Profile: profile, EngineIdentityDigest: report.EngineIdentityDigest, EndpointDigest: report.EndpointDigest,
		ServerOS: report.ServerOS, APIVersion: report.APIVersion, CgroupVersion: cgroupVersion, Flags: map[string]bool{},
		BuilderImageDigest: domain.Digest(report.BuilderImageID), RuntimeImageDigest: domain.Digest(report.RuntimeImageID),
		TransferImageDigest: domain.Digest(report.TransferImageID), ExecutionProtocol: report.ExecutionProtocol,
	}
	return result, nil
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
		callID, claimed, err := op.claimVolume(ctx, resource)
		if err != nil {
			return err
		}
		if err := op.lifecycleBeginCreate(ctx, resource, maps.Clone(labels), &callID); err != nil {
			return err
		}
		if op.runner.lifecycle == nil || op.identity.SandboxExecutionID == "" {
			if err := op.watchdog.PreCreate(ctx, resource, maps.Clone(labels)); err != nil {
				return err
			}
		}
		lifecycleResource := op.lifecycleResources[resource.Ordinal]
		if lifecycleResource.CreatedAt.IsZero() {
			lifecycleResource.CreatedAt = op.lifecycleAt
		}
		stage := domain.StageName("sandbox")
		if value, ok := op.auth.(interface{ StageName() domain.StageName }); ok && value.StageName() != "" {
			stage = value.StageName()
		}
		callRecordID := callRecordForAttempt(callID)
		expectedRunVersion := int64(1)
		if grant, ok := op.volumeGrants[callID]; ok {
			if grant.CallRecordID.Validate() == nil {
				callRecordID = grant.CallRecordID
			}
			if grant.ExpectedRunVersion > 0 {
				expectedRunVersion = grant.ExpectedRunVersion
			}
		}
		dispatch, err := op.runner.callLedger.BeginDispatch(ctx, domain.BeginDispatchRequest{
			RunID: op.identity.RunID, ExpectedRunVersion: expectedRunVersion, StageName: stage,
			AttemptID: op.identity.AttemptID, CallRecordID: callRecordID, AttemptCallID: callID,
			IdempotencyKey: stableSandboxKey("physical_dispatch", string(callID)), At: stableLifecycleTime(lifecycleResource.CreatedAt, "dispatch"),
		})
		if err != nil {
			return err
		}
		op.physicalGrants[callID] = dispatch
		if claimed {
			op.physical = append(op.physical, callID)
		}
		created, err := op.runner.engine.VolumeCreate(ctx, moby.VolumeCreateOptions{Name: owned.name, Driver: owned.driver, DriverOpts: driverOptions, Labels: labels})
		if err != nil {
			return errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
		}
		if created.Volume.Name == "" {
			err = fmt.Errorf("Docker returned an empty volume name")
			return errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
		}
		if err := verifyVolumeOwnership(created.Volume, owned); err != nil {
			return errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
		}
		if err := op.runner.callLedger.MarkSent(ctx, dispatch, stableLifecycleTime(lifecycleResource.CreatedAt, "sent")); err != nil {
			return errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
		}
		responseDigest := domain.SumBytes([]byte("cpgen.docker.volume-create/v1\n" + string(callID) + "\n" + created.Volume.Name))
		if err := op.runner.callLedger.CompletePhysical(ctx, domain.CompletePhysicalRequest{
			RunID: dispatch.RunID, ExpectedRunVersion: dispatch.ExpectedRunVersion, StageName: dispatch.StageName,
			AttemptID: dispatch.AttemptID, CallRecordID: dispatch.CallRecordID, AttemptCallID: dispatch.AttemptCallID,
			State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ProviderRequestID: created.Volume.Name,
			ResponseDigest: &responseDigest, IdempotencyKey: stableSandboxKey("physical_complete", string(callID)), At: stableLifecycleTime(lifecycleResource.CreatedAt, "complete"),
		}); err != nil {
			return errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
		}
		op.physicalSettled[callID] = true
		inspected, err := op.runner.engine.VolumeInspect(ctx, owned.name, moby.VolumeInspectOptions{})
		if err != nil {
			return errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
		}
		if err := verifyVolumeOwnership(inspected.Volume, owned); err != nil {
			return errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
		}
		if err := op.lifecycleCompleteCreate(ctx, resource, created.Volume.Name, labels, &callID); err != nil {
			return errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
		}
		if err := op.watchdog.ResourceCreated(ctx, resource, owned.name); err != nil {
			return errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
		}
	}
	return nil
}

func (op *operation) claimVolume(ctx context.Context, resource port.PlannedResource) (domain.AttemptCallID, bool, error) {
	if claimer, ok := op.auth.(port.VolumeDispatchAuthorization); ok {
		grant, err := claimer.ClaimNextVolume(ctx, resource.Role)
		if err != nil {
			return "", false, err
		}
		if grant.Auth == nil || grant.Role != resource.Role || grant.Auth.RunID() != op.identity.RunID || grant.Auth.AttemptID() != op.identity.AttemptID || grant.Auth.ScopeDigest() != op.auth.ScopeDigest() {
			return "", false, fmt.Errorf("volume grant identity mismatch")
		}
		callID := grant.Auth.CallID()
		if err := callID.Validate(); err != nil {
			return "", false, err
		}
		op.volumeGrants[callID] = grant
		return callID, grant.Durable, nil
	}
	// The legacy Slice 0 fixture has no volume claim method. Keep its call
	// surface compatible while still forcing the CallLedger to find a prepared
	// physical row before Docker I/O. Real callers must implement the optional
	// claim interface and provide that row from Task 4.
	callID := domain.AttemptCallID(stableSandboxID("call", string(op.identity.SandboxExecutionID), "volume", strconv.Itoa(resource.Ordinal), resource.DeterministicName))
	if err := callID.Validate(); err != nil {
		return "", false, err
	}
	return callID, false, nil
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
	if grant.Role != role || grant.Auth.RunID() != op.identity.RunID || grant.Auth.AttemptID() != op.identity.AttemptID || grant.Auth.ScopeDigest() != op.auth.ScopeDigest() {
		return nil, "", fmt.Errorf("container grant identity mismatch")
	}
	options, err := factory(resource, callID)
	if err != nil {
		return nil, "", err
	}
	labels := maps.Clone(options.Config.Labels)
	owned := &ownedContainer{resource: resource, name: resource.DeterministicName, labels: labels}
	op.containers = append(op.containers, owned)
	if err := op.lifecycleBeginCreate(ctx, resource, maps.Clone(labels), &callID); err != nil {
		return nil, "", err
	}
	if op.runner.lifecycle == nil || op.identity.SandboxExecutionID == "" {
		if err := op.watchdog.PreCreate(ctx, resource, maps.Clone(labels)); err != nil {
			return nil, "", err
		}
	}
	op.physical = append(op.physical, callID)
	lifecycleResource := op.lifecycleResources[resource.Ordinal]
	if lifecycleResource.CreatedAt.IsZero() {
		lifecycleResource.CreatedAt = op.lifecycleAt
	}
	callRecordID := grant.CallRecordID
	if err := callRecordID.Validate(); err != nil {
		callRecordID = callRecordForAttempt(callID)
	}
	expectedRunVersion := grant.ExpectedRunVersion
	if expectedRunVersion <= 0 {
		expectedRunVersion = 1
	}
	stage := domain.StageName("sandbox")
	if value, ok := op.auth.(interface{ StageName() domain.StageName }); ok && value.StageName() != "" {
		stage = value.StageName()
	}
	dispatchAt := stableLifecycleTime(lifecycleResource.CreatedAt, "dispatch")
	dispatch, err := op.runner.callLedger.BeginDispatch(ctx, domain.BeginDispatchRequest{
		RunID: op.identity.RunID, ExpectedRunVersion: expectedRunVersion, StageName: stage,
		AttemptID: op.identity.AttemptID, CallRecordID: callRecordID, AttemptCallID: callID,
		IdempotencyKey: stableSandboxKey("physical_dispatch", string(callID)), At: dispatchAt,
	})
	if err != nil {
		return nil, callID, err
	}
	op.physicalGrants[callID] = dispatch
	completeKey := stableSandboxKey("physical_complete", string(callID))
	created, err := op.runner.engine.ContainerCreate(ctx, options)
	if err != nil {
		return nil, callID, errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
	}
	if created.ID == "" {
		err = fmt.Errorf("Docker returned an empty container ID")
		return nil, callID, errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
	}
	// Retain the identity immediately. If a subsequent ledger/watchdog/lifecycle
	// write fails, cleanup can still inspect this exact returned resource.
	owned.id = created.ID
	if err := op.runner.callLedger.MarkSent(ctx, dispatch, stableLifecycleTime(lifecycleResource.CreatedAt, "sent")); err != nil {
		return nil, callID, errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
	}
	responseDigest := domain.SumBytes([]byte("cpgen.docker.container-create/v1\n" + string(callID) + "\n" + created.ID))
	if err := op.runner.callLedger.CompletePhysical(ctx, domain.CompletePhysicalRequest{
		RunID: dispatch.RunID, ExpectedRunVersion: dispatch.ExpectedRunVersion, StageName: dispatch.StageName,
		AttemptID: dispatch.AttemptID, CallRecordID: dispatch.CallRecordID, AttemptCallID: dispatch.AttemptCallID,
		State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ProviderRequestID: created.ID,
		ResponseDigest: &responseDigest, IdempotencyKey: completeKey, At: stableLifecycleTime(lifecycleResource.CreatedAt, "complete"),
	}); err != nil {
		return nil, callID, errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
	}
	op.physicalSettled[callID] = true
	if err := op.watchdog.ResourceCreated(ctx, resource, created.ID); err != nil {
		return nil, callID, errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
	}
	inspected, err := op.runner.engine.ContainerInspect(ctx, created.ID, moby.ContainerInspectOptions{})
	if err != nil {
		return nil, callID, errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
	}
	if err := verifyContainerOwnership(inspected, owned); err != nil {
		return nil, callID, errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
	}
	if err := op.lifecycleCompleteCreate(ctx, resource, created.ID, labels, &callID); err != nil {
		return nil, callID, errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
	}
	if role != port.ContainerTarget {
		if err := VerifyHelperInspect(options, inspected); err != nil {
			return nil, callID, errors.Join(err, op.settlePhysicalUnknown(ctx, dispatch, resource, stableLifecycleTime(lifecycleResource.CreatedAt, "complete")))
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
	if op.runner.lifecycle != nil && op.identity.SandboxExecutionID != "" && op.lifecycleVersion > 0 {
		key := stableSandboxKey("cleanup", string(op.identity.SandboxExecutionID))
		if execution, markErr := op.runner.lifecycle.MarkCleanupPending(cleanupCtx, domain.MarkCleanupPendingCommand{ExecutionID: op.identity.SandboxExecutionID, ExpectedVersion: op.lifecycleVersion, Reason: "foreground operation settlement", IdempotencyKey: key, At: stableLifecycleTime(op.lifecycleAt, "cleanup")}); markErr != nil {
			failures = append(failures, markErr)
		} else {
			op.lifecycleVersion = execution.LifecycleVersion
		}
	}
	if err := op.settleOutstandingPhysical(cleanupCtx); err != nil {
		failures = append(failures, err)
	}
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
	cleanupErr := op.cleanup(cleanupCtx)
	if cleanupErr != nil {
		failures = append(failures, cleanupErr)
	}
	if op.runner.lifecycle != nil && op.identity.SandboxExecutionID != "" && cleanupErr == nil {
		proofs, proofRecorder := op.runner.lifecycle.(port.SandboxCleanupRecorder)
		if !proofRecorder {
			failures = append(failures, fmt.Errorf("sandbox cleanup proof recorder is required"))
		} else {
			for ordinal, resource := range op.lifecycleResources {
				if resource.Phase == domain.SandboxResourcePlanned {
					updated, interruptErr := proofs.RecordResourceInterrupted(cleanupCtx, domain.RecordResourceInterruptedCommand{ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: resource.Version, ReasonDigest: domain.SumBytes([]byte("cpgen.no-create/v1\n" + string(resource.ID))), At: stableLifecycleTime(resource.CreatedAt, "interrupt")})
					if interruptErr != nil {
						failures = append(failures, interruptErr)
					} else {
						op.lifecycleResources[ordinal] = updated
					}
					continue
				}
				if resource.Phase != domain.SandboxResourceCleaned && resource.Phase != domain.SandboxResourceInterrupted {
					failures = append(failures, fmt.Errorf("resource %s lacks persisted cleanup proof (phase %s)", resource.ID, resource.Phase))
				}
			}
			if len(failures) == 0 {
				key := stableSandboxKey("cleanup_finish", string(op.identity.SandboxExecutionID))
				if _, finishErr := op.runner.lifecycle.FinishCleanup(cleanupCtx, domain.FinishCleanupCommand{ExecutionID: op.identity.SandboxExecutionID, ExpectedVersion: op.lifecycleVersion, ReconciliationDigest: reconciliationDigest(op.lifecycleResources), IdempotencyKey: key, At: stableLifecycleTime(op.lifecycleAt, "finish")}); finishErr != nil {
					failures = append(failures, finishErr)
				}
			}
		}
	}
	if op.watchdog != nil {
		ackCtx, cancelAck := context.WithTimeout(context.Background(), op.runner.limits.CleanupTimeout)
		defer cancelAck()
		if cleanupErr == nil && op.targetPhase && !op.targetStopped {
			target := op.targetContainer()
			if target == nil || target.id == "" {
				failures = append(failures, fmt.Errorf("target phase has no persisted target identity"))
			} else if err := op.watchdog.Stopped(ackCtx, target.resource, target.id); err != nil {
				failures = append(failures, fmt.Errorf("watchdog STOPPED acknowledgement after cleanup: %w", err))
			} else {
				op.targetStopped = true
			}
		}
		if cleanupErr == nil {
			if err := op.watchdog.Cleaned(ackCtx); err != nil {
				failures = append(failures, fmt.Errorf("watchdog CLEANED acknowledgement: %w", err))
			}
		}
		if err := op.watchdog.Close(); err != nil {
			failures = append(failures, fmt.Errorf("close watchdog session: %w", err))
		}
	}
	return errors.Join(failures...)
}

func (op *operation) settleOutstandingPhysical(ctx context.Context) error {
	var failures []error
	for callID, dispatch := range op.physicalGrants {
		if op.physicalSettled[callID] {
			continue
		}
		var planned port.PlannedResource
		found := false
		for _, candidate := range op.plan.Resources {
			if resource := op.lifecycleResources[candidate.Ordinal]; resource.PhysicalCallID != nil && *resource.PhysicalCallID == callID {
				planned = candidate
				found = true
				break
			}
		}
		if !found {
			// A call grant itself is enough to settle the Task 4 boundary; a
			// missing resource row is an integrity failure, but do not skip the
			// physical UNKNOWN settlement.
			planned = port.PlannedResource{Ordinal: -1}
		}
		at := dispatch.DispatchStartedAt
		if !at.IsZero() {
			at = at.Add(5 * time.Second)
		}
		if err := op.runner.callLedger.CompletePhysical(ctx, unknownPhysicalCompletion(dispatch, stableSandboxKey("physical_complete", string(callID)), at.UTC())); err != nil {
			failures = append(failures, fmt.Errorf("settle physical call %s as UNKNOWN: %w", callID, err))
			continue
		}
		op.physicalSettled[callID] = true
		if found {
			if err := op.lifecycleSettlementFailure(ctx, planned); err != nil {
				failures = append(failures, fmt.Errorf("settle sandbox resource %s after UNKNOWN: %w", callID, err))
			}
		} else {
			failures = append(failures, fmt.Errorf("physical call %s has no persisted sandbox resource", callID))
		}
	}
	return errors.Join(failures...)
}

func (op *operation) targetContainer() *ownedContainer {
	for _, candidate := range op.containers {
		if candidate.resource.Role == port.ResourceTarget {
			return candidate
		}
	}
	return nil
}

func (op *operation) cleanup(ctx context.Context) error {
	var failures []error
	proofs, canPersistProof := op.runner.lifecycle.(port.SandboxCleanupRecorder)
	for index := len(op.containers) - 1; index >= 0; index-- {
		owned := op.containers[index]
		resource, tracked := op.lifecycleResources[owned.resource.Ordinal]
		if !tracked {
			failures = append(failures, fmt.Errorf("cleanup resource %d was not durably prepared", owned.resource.Ordinal))
			continue
		}
		if resource.Phase == domain.SandboxResourceCleaned {
			continue
		}
		if resource.Phase != domain.SandboxResourcePlanned && resource.Phase != domain.SandboxResourceInterrupted {
			var transitionErr error
			resource, transitionErr = op.lifecycleEnsureCleanupPending(ctx, owned.resource)
			if transitionErr != nil {
				failures = append(failures, fmt.Errorf("mark cleanup pending for container %q: %w", owned.name, transitionErr))
				continue
			}
		}
		lookup := owned.id
		if lookup == "" {
			lookup = owned.name
		}
		inspected, err := op.runner.engine.ContainerInspect(ctx, lookup, moby.ContainerInspectOptions{})
		proof := StopProof{}
		engineID := resource.EngineResourceID
		if errdefs.IsNotFound(err) {
			if engineID == "" {
				failures = append(failures, fmt.Errorf("cleanup container %q is not found without persisted engine identity", owned.name))
				continue
			}
			proof = StopProof{NotFound: true}
		} else if err != nil {
			failures = append(failures, fmt.Errorf("inspect cleanup container %q: %w", owned.name, err))
			continue
		} else {
			if err := verifyContainerOwnership(inspected, owned); err != nil {
				failures = append(failures, err)
				continue
			}
			engineID = inspected.Container.ID
			proof, err = portableStop(ctx, op.runner.engine, engineID, func(result moby.ContainerInspectResult) error {
				return verifyContainerOwnership(result, owned)
			})
			if err != nil {
				failures = append(failures, fmt.Errorf("prove stopped cleanup container %q: %w", owned.name, err))
				continue
			}
		}
		if !canPersistProof {
			failures = append(failures, fmt.Errorf("sandbox cleanup proof recorder is required"))
			continue
		}
		updated, err := proofs.RecordResourceStopProof(ctx, domain.RecordResourceStopProofCommand{ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: resource.Version, EngineResourceID: engineID, EngineIdentityDigest: resource.EngineIdentityDigest, LabelsDigest: digestLabels(owned.labels), ProofDigest: proof.Digest(), ProofKind: "STOP_KILL_WAIT_INSPECT", At: stableLifecycleTime(resource.CreatedAt, "stop")})
		if err != nil {
			failures = append(failures, fmt.Errorf("persist stop proof for %q: %w", owned.name, err))
			continue
		}
		if _, err := op.runner.engine.ContainerRemove(ctx, engineID, moby.ContainerRemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("remove cleanup container %q: %w", owned.name, err))
			continue
		}
		cleaned, err := proofs.RecordResourceCleaned(ctx, domain.RecordResourceCleanedCommand{ExecutionID: updated.ExecutionID, ResourceID: updated.ID, ExpectedVersion: updated.Version, EngineResourceID: engineID, EngineIdentityDigest: updated.EngineIdentityDigest, EvidenceDigest: domain.SumBytes([]byte("cpgen.remove/v1\n" + engineID)), At: stableLifecycleTime(resource.CreatedAt, "clean")})
		if err != nil {
			failures = append(failures, fmt.Errorf("persist cleanup evidence for %q: %w", owned.name, err))
		} else {
			op.lifecycleResources[owned.resource.Ordinal] = cleaned
		}
	}
	for index := len(op.volumes) - 1; index >= 0; index-- {
		owned := op.volumes[index]
		resource, tracked := op.lifecycleResources[owned.resource.Ordinal]
		if !tracked {
			failures = append(failures, fmt.Errorf("cleanup resource %d was not durably prepared", owned.resource.Ordinal))
			continue
		}
		if resource.Phase == domain.SandboxResourceCleaned {
			continue
		}
		if resource.Phase != domain.SandboxResourcePlanned && resource.Phase != domain.SandboxResourceInterrupted {
			var transitionErr error
			resource, transitionErr = op.lifecycleEnsureCleanupPending(ctx, owned.resource)
			if transitionErr != nil {
				failures = append(failures, fmt.Errorf("mark cleanup pending for volume %q: %w", owned.name, transitionErr))
				continue
			}
		}
		inspected, err := op.runner.engine.VolumeInspect(ctx, owned.name, moby.VolumeInspectOptions{})
		if errdefs.IsNotFound(err) {
			if resource.EngineResourceID == "" {
				failures = append(failures, fmt.Errorf("cleanup volume %q is not found without persisted engine identity", owned.name))
				continue
			}
			// A volume has no running process; absence is explicit removal proof.
			if !canPersistProof {
				failures = append(failures, fmt.Errorf("sandbox cleanup proof recorder is required"))
				continue
			}
			updated, proofErr := proofs.RecordResourceStopProof(ctx, domain.RecordResourceStopProofCommand{ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: resource.Version, EngineResourceID: resource.EngineResourceID, EngineIdentityDigest: resource.EngineIdentityDigest, LabelsDigest: digestLabels(owned.labels), ProofDigest: domain.SumBytes([]byte("cpgen.volume-not-found/v1\n" + resource.EngineResourceID)), ProofKind: "VOLUME_NOT_FOUND", At: stableLifecycleTime(resource.CreatedAt, "stop")})
			if proofErr != nil {
				failures = append(failures, proofErr)
				continue
			}
			cleaned, cleanErr := proofs.RecordResourceCleaned(ctx, domain.RecordResourceCleanedCommand{ExecutionID: updated.ExecutionID, ResourceID: updated.ID, ExpectedVersion: updated.Version, EngineResourceID: updated.EngineResourceID, EngineIdentityDigest: updated.EngineIdentityDigest, EvidenceDigest: domain.SumBytes([]byte("cpgen.remove/v1\n" + updated.EngineResourceID)), At: stableLifecycleTime(resource.CreatedAt, "clean")})
			if cleanErr != nil {
				failures = append(failures, cleanErr)
			} else {
				op.lifecycleResources[owned.resource.Ordinal] = cleaned
			}
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
		if !canPersistProof {
			failures = append(failures, fmt.Errorf("sandbox cleanup proof recorder is required"))
			continue
		}
		updated, err := proofs.RecordResourceStopProof(ctx, domain.RecordResourceStopProofCommand{ExecutionID: resource.ExecutionID, ResourceID: resource.ID, ExpectedVersion: resource.Version, EngineResourceID: inspected.Volume.Name, EngineIdentityDigest: resource.EngineIdentityDigest, LabelsDigest: digestLabels(owned.labels), ProofDigest: domain.SumBytes([]byte("cpgen.volume-inspect/v1\n" + inspected.Volume.Name)), ProofKind: "VOLUME_INSPECT", At: stableLifecycleTime(resource.CreatedAt, "stop")})
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if _, err := op.runner.engine.VolumeRemove(ctx, owned.name, moby.VolumeRemoveOptions{}); err != nil && !errdefs.IsNotFound(err) {
			failures = append(failures, fmt.Errorf("remove cleanup volume %q: %w", owned.name, err))
			continue
		}
		cleaned, err := proofs.RecordResourceCleaned(ctx, domain.RecordResourceCleanedCommand{ExecutionID: updated.ExecutionID, ResourceID: updated.ID, ExpectedVersion: updated.Version, EngineResourceID: updated.EngineResourceID, EngineIdentityDigest: updated.EngineIdentityDigest, EvidenceDigest: domain.SumBytes([]byte("cpgen.remove/v1\n" + updated.EngineResourceID)), At: stableLifecycleTime(resource.CreatedAt, "clean")})
		if err != nil {
			failures = append(failures, err)
		} else {
			op.lifecycleResources[owned.resource.Ordinal] = cleaned
		}
	}
	return errors.Join(failures...)
}

func reconciliationDigest(resources map[int]domain.SandboxResource) domain.Digest {
	ordinals := make([]int, 0, len(resources))
	for ordinal := range resources {
		ordinals = append(ordinals, ordinal)
	}
	sort.Ints(ordinals)
	var encoded strings.Builder
	for _, ordinal := range ordinals {
		resource := resources[ordinal]
		encoded.WriteString(strconv.Itoa(ordinal))
		encoded.WriteByte('|')
		encoded.WriteString(string(resource.ID))
		encoded.WriteByte('|')
		encoded.WriteString(string(resource.Phase))
		encoded.WriteByte('|')
		encoded.WriteString(string(resource.StopProofDigest))
		encoded.WriteByte('|')
		encoded.WriteString(string(resource.CleanupEvidenceDigest))
		encoded.WriteByte('\n')
	}
	return domain.SumBytes([]byte(encoded.String()))
}

var _ port.DockerSandbox = (*Runner)(nil)
