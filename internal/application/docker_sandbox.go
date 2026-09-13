package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"time"

	docker "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

type DockerSandboxStore interface {
	RunLLMStore
	port.SandboxLifecycleRecorder
	port.SandboxLifecycleReader
	port.SandboxCleanupRecorder
	port.SandboxWatchdogReader
}

type DockerSandboxConfig struct {
	Store          DockerSandboxStore
	Blobs          *blob.Store
	Clock          clock.Clock
	Identity       port.SandboxAuthorizationIdentity
	Engine         docker.Engine
	Config         docker.Config
	Lock           toolchain.Lock
	EngineIdentity domain.Digest
	Watchdog       docker.WatchdogController
	Limits         docker.ControlLimits
}

// DockerSandboxSession executes serial operations under the current stage's
// foreground lock. Completed local result receipts replay without Docker I/O.
type DockerSandboxSession struct {
	config    DockerSandboxConfig
	artifacts []domain.PendingArtifact
}

// Artifacts returns the programs, process streams and result receipts to
// attach in the caller's stage transaction. Reading it performs no I/O.
func (s *DockerSandboxSession) Artifacts() []domain.PendingArtifact {
	cloned := append([]domain.PendingArtifact(nil), s.artifacts...)
	for index := range cloned {
		if cloned[index].Provenance.InputDigest != nil {
			digest := *cloned[index].Provenance.InputDigest
			cloned[index].Provenance.InputDigest = &digest
		}
	}
	return cloned
}

func (s *DockerSandboxSession) retainArtifacts(receipt domain.PendingArtifact, value any) {
	pending := []domain.PendingArtifact{receipt}
	add := func(artifact *domain.PendingArtifact) {
		if artifact != nil {
			pending = append(pending, *artifact)
		}
	}
	switch result := value.(type) {
	case port.CompileResult:
		add(result.Program)
		add(result.Stdout)
		add(result.Stderr)
		add(result.Execution)
	case port.RunResult:
		add(result.Stdout)
		add(result.Stderr)
		add(result.Execution)
		pending = append(pending, result.Outputs...)
	}
	for _, artifact := range pending {
		found := false
		for _, existing := range s.artifacts {
			if existing.WriterTokenID == artifact.WriterTokenID {
				found = true
				break
			}
		}
		if !found {
			s.artifacts = append(s.artifacts, artifact)
		}
	}
}

func NewDockerSandboxSession(config DockerSandboxConfig) (*DockerSandboxSession, error) {
	if err := validateDockerSandboxDependencies(config); err != nil {
		return nil, err
	}
	if err := config.Identity.Validate(); err != nil {
		return nil, err
	}
	encoded, err := config.Lock.MarshalIndent()
	if err != nil {
		return nil, err
	}
	config.Lock, err = toolchain.LoadLock(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	return &DockerSandboxSession{config: config}, nil
}

func validateDockerSandboxDependencies(config DockerSandboxConfig) error {
	if config.Store == nil || config.Blobs == nil || config.Clock == nil || config.Engine == nil || config.Watchdog == nil {
		return errors.New("durable Docker sandbox dependencies are required")
	}
	if err := config.Lock.Validate(); err != nil {
		return err
	}
	if err := config.EngineIdentity.Validate(); err != nil {
		return err
	}
	if err := config.Limits.Validate(); err != nil {
		return err
	}
	if _, err := config.Config.Validate(runtime.GOOS); err != nil {
		return err
	}
	if config.Config.BuilderImage != string(config.Lock.Builder.ImageID) || config.Config.RuntimeImage != string(config.Lock.Runtime.ImageID) || config.Config.TransferImage != string(config.Lock.Transfer.ImageID) {
		return errors.New("sandbox images differ from the frozen toolchain lock")
	}
	if _, ok := config.Watchdog.(docker.WatchdogPreparer); !ok {
		return errors.New("durable sandbox requires a prepared detached watchdog")
	}
	if err := config.Watchdog.TokenDigest().Validate(); err != nil {
		return err
	}
	return nil
}

func (s *DockerSandboxSession) Compile(ctx context.Context, request port.CompileRequest) (domain.MeteredOutcome[port.CompileResult], error) {
	if err := request.Validate(); err != nil {
		return domain.MeteredOutcome[port.CompileResult]{}, err
	}
	return executeSandbox(ctx, s, domain.CallSandboxCompile, request,
		func(i docker.PlanIdentity) (port.ContainerPlan, error) {
			return docker.BuildCompilePlan(request, s.config.Lock, i)
		},
		func(r *docker.Runner, a port.SandboxDispatchAuthorization) (port.CompileResult, error) {
			return r.Compile(ctx, a, request)
		},
		func(r port.CompileResult) domain.CallTrace { return r.CallTrace })
}

func (s *DockerSandboxSession) Run(ctx context.Context, request port.RunRequest) (domain.MeteredOutcome[port.RunResult], error) {
	if err := request.Validate(); err != nil {
		return domain.MeteredOutcome[port.RunResult]{}, err
	}
	return executeSandbox(ctx, s, domain.CallSandboxRun, request,
		func(i docker.PlanIdentity) (port.ContainerPlan, error) {
			return docker.BuildRunPlan(request, s.config.Lock, i)
		},
		func(r *docker.Runner, a port.SandboxDispatchAuthorization) (port.RunResult, error) {
			return r.Run(ctx, a, request)
		},
		func(r port.RunResult) domain.CallTrace { return r.CallTrace })
}

type sandboxResultReceipt[T any] struct {
	Schema string        `json:"schema"`
	Scope  domain.Digest `json:"scope"`
	Plan   domain.Digest `json:"plan"`
	Result T             `json:"result"`
}

func executeSandbox[T interface{ Validate() error }](ctx context.Context, session *DockerSandboxSession, kind domain.CallKind, request any, build func(docker.PlanIdentity) (port.ContainerPlan, error), invoke func(*docker.Runner, port.SandboxDispatchAuthorization) (T, error), trace func(T) domain.CallTrace) (domain.MeteredOutcome[T], error) {
	var empty domain.MeteredOutcome[T]
	c := session.config
	identity, planIdentity, err := sandboxOperationIdentity(c, kind, request)
	if err != nil {
		return empty, err
	}
	scope := identity.ScopeDigest
	plan, err := build(planIdentity)
	if err != nil {
		return empty, err
	}
	sink, err := NewSandboxArtifactSink(c.Store, c.Blobs, c.Clock, identity)
	if err != nil {
		return empty, err
	}
	ledger, err := NewRunBoundLLMLedger(c.Store, identity.RunID, identity.StageName, identity.AttemptID)
	if err != nil {
		return empty, err
	}
	prefix := domain.SafeRelPath("sandbox/" + string(identity.SandboxExecutionID))
	resultDecl := port.ArtifactDeclaration{MediaType: "application/vnd.cpgen.sandbox-result+json", Role: domain.ArtifactEvidence, LogicalPath: domain.SafeRelPath(string(prefix) + "/result.json"), MaxBytes: 1 << 20, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.sandbox-result/v1", Producer: "docker", InputDigest: &scope}}
	publishResult := func(result T) (domain.MeteredOutcome[T], error) {
		encoded, err := json.Marshal(sandboxResultReceipt[T]{Schema: "cpgen.sandbox-result/v1", Scope: scope, Plan: plan.PlanDigest, Result: result})
		if err != nil {
			return empty, err
		}
		pending, err := sink.Publish(ctx, resultDecl, encoded)
		if err != nil {
			return empty, err
		}
		session.retainArtifacts(pending, result)
		return domain.MeteredOutcome[T]{Value: &result, CallTrace: trace(result)}, nil
	}
	if pending, found, err := sink.Read(ctx, resultDecl); err != nil {
		return empty, err
	} else if found {
		reader, err := c.Blobs.OpenVerified(ctx, pending.Blob)
		if err != nil {
			return empty, err
		}
		encoded, readErr := io.ReadAll(io.LimitReader(reader, resultDecl.MaxBytes+1))
		if err := errors.Join(readErr, reader.Close()); err != nil {
			return empty, err
		}
		var receipt sandboxResultReceipt[T]
		if err := json.Unmarshal(encoded, &receipt); err != nil {
			return empty, err
		}
		if receipt.Schema != "cpgen.sandbox-result/v1" || receipt.Scope != scope || receipt.Plan != plan.PlanDigest {
			return empty, errors.New("sandbox result receipt binding differs")
		}
		if err := receipt.Result.Validate(); err != nil {
			return empty, err
		}
		if err := verifySandboxCleaned(ctx, c.Store, identity, plan); err != nil {
			return empty, err
		}
		session.retainArtifacts(pending, receipt.Result)
		return domain.MeteredOutcome[T]{Value: &receipt.Result, CallTrace: trace(receipt.Result)}, nil
	}
	if restored, found, err := recoverSandboxResult(ctx, session, identity, plan, sink, request); err != nil {
		return empty, err
	} else if found {
		result, ok := restored.(T)
		if !ok {
			return empty, errors.New("recovered sandbox result type differs")
		}
		return publishResult(result)
	}
	bindings, err := prepareSandboxResourceCalls(ctx, ledger, c.Clock, identity, plan)
	if err != nil {
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.Limits.CleanupTimeout)
		defer cancel()
		return empty, errors.Join(err, finishSandboxResourceCalls(settleCtx, ledger, c.Clock, identity, bindings))
	}
	auth, err := port.NewPreparedSandboxAuthorization(ctx, identity, plan, bindings, ledger, c.Clock.Now)
	if err != nil {
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.Limits.CleanupTimeout)
		defer cancel()
		return empty, errors.Join(fmt.Errorf("sandbox requires receipt recovery before another create: %w", err), finishSandboxResourceCalls(settleCtx, ledger, c.Clock, identity, bindings))
	}
	runner, err := docker.NewRunner(docker.RunnerOptions{Engine: c.Engine, Config: c.Config, Lock: c.Lock, EngineIdentityDigest: c.EngineIdentity, Blobs: c.Blobs, Artifacts: sink, ArtifactPrefix: prefix, Watchdog: c.Watchdog, Limits: c.Limits, Lifecycle: c.Store, CallLedger: ledger, Clock: c.Clock})
	if err != nil {
		settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.Limits.CleanupTimeout)
		defer cancel()
		return empty, errors.Join(err, auth.AbortRemaining(settleCtx), finishSandboxResourceCalls(settleCtx, ledger, c.Clock, identity, bindings))
	}
	result, runErr := invoke(runner, auth)
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.Limits.CleanupTimeout)
	defer cancel()
	settleErr := errors.Join(auth.AbortRemaining(settleCtx), finishSandboxResourceCalls(settleCtx, ledger, c.Clock, identity, bindings))
	if err := errors.Join(runErr, settleErr); err != nil {
		return empty, err
	}
	if err := result.Validate(); err != nil {
		return empty, err
	}
	if err := verifySandboxCleaned(ctx, c.Store, identity, plan); err != nil {
		return empty, err
	}
	return publishResult(result)
}

func prepareSandboxResourceCalls(ctx context.Context, ledger *RunBoundLLMLedger, source clock.Clock, identity port.SandboxAuthorizationIdentity, plan port.ContainerPlan) ([]port.SandboxResourceCall, error) {
	var bindings []port.SandboxResourceCall
	for _, resource := range plan.Resources {
		if resource.Kind == port.ResourceCgroup {
			continue
		}
		digest, err := port.SandboxResourceRequestDigest(identity, plan, resource.Ordinal)
		if err != nil {
			return bindings, err
		}
		id := domain.CallRecordID(coordinatorMutationID("callrec", identity.SandboxExecutionID, resource.Ordinal))
		physicalID := domain.AttemptCallID(coordinatorMutationID("call", id))
		at := source.Now().UTC()
		if previous, err := ledger.ReadLogicalCall(ctx, id); err == nil {
			at = previous.OpenedAt
		} else if !errors.Is(err, sqlite.ErrNotFound) {
			return bindings, err
		}
		open := domain.OpenCallRequest{ID: id, RunID: identity.RunID, ExpectedRunVersion: identity.ExpectedRunVersion, StageName: identity.StageName, AttemptID: identity.AttemptID, LogicalOperationID: port.SandboxResourceLogicalOperation(identity, resource.Ordinal), Kind: identity.Kind, Provider: "docker", RequestDigest: digest, PolicyDigest: identity.ScopeDigest, RetryPolicy: domain.RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond, JitterSeedDigest: identity.ScopeDigest}, IdempotencyKey: coordinatorMutationID("open", id), At: at}
		record, err := ledger.OpenCall(ctx, open)
		if err != nil {
			return bindings, err
		}
		binding := port.SandboxResourceCall{ResourceOrdinal: resource.Ordinal, CallRecordID: id, AttemptCallID: physicalID}
		if record.State == domain.CallRecordOpen {
			physical := domain.PhysicalCallPlan{ID: physicalID, Ordinal: 1, RetryGroup: "resource", RetryOrdinal: 1, Kind: domain.PhysicalDockerVolumeCreate, Provider: "docker", RequestDigest: digest, IdempotencyKey: coordinatorMutationID("physical", physicalID)}
			if resource.Kind == port.ResourceContainer {
				physical.Kind = domain.PhysicalDockerContainerCreate
				physical.Reservations = []domain.ReservationPlan{{ID: domain.ReservationID(coordinatorMutationID("res", physicalID)), Dimension: domain.BudgetDockerContainerCreates, Subkey: "create", UpperBound: 1}}
			}
			p, err := ledger.PrepareCalls(ctx, domain.PrepareCallsRequest{RunID: identity.RunID, ExpectedRunVersion: identity.ExpectedRunVersion, StageName: identity.StageName, AttemptID: identity.AttemptID, CallRecordID: id, PlanDigest: plan.PlanDigest, Calls: []domain.PhysicalCallPlan{physical}, IdempotencyKey: coordinatorMutationID("prepare", id), At: source.Now().UTC()})
			if err != nil {
				return bindings, err
			}
			if p.Failure != nil {
				return bindings, fmt.Errorf("sandbox resource reservation refused: %s/%s", p.Failure.Code, p.Failure.Class)
			}
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

func finishSandboxResourceCalls(ctx context.Context, ledger *RunBoundLLMLedger, source clock.Clock, identity port.SandboxAuthorizationIdentity, bindings []port.SandboxResourceCall) error {
	var errs []error
	for _, binding := range bindings {
		p, err := ledger.LoadCall(ctx, binding.CallRecordID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if p.Call.State == domain.CallRecordTerminal {
			continue
		}
		if len(p.PhysicalCalls) != 1 || p.PhysicalCalls[0].ID != binding.AttemptCallID {
			errs = append(errs, errors.New("sandbox resource physical binding changed"))
			continue
		}
		physical := p.PhysicalCalls[0]
		failure := physical.Failure
		if physical.State == domain.PhysicalPrepared {
			failure = &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
			err = ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: identity.RunID, ExpectedRunVersion: identity.ExpectedRunVersion, StageName: identity.StageName, AttemptID: identity.AttemptID, CallRecordID: binding.CallRecordID, AttemptCallID: binding.AttemptCallID, State: domain.PhysicalAbortedNoDispatch, Outcome: domain.PhysicalOutcomeNoSend, Failure: failure, IdempotencyKey: coordinatorMutationID("abort", binding.AttemptCallID), At: source.Now().UTC()})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			physical.State = domain.PhysicalAbortedNoDispatch
		}
		finish := domain.FinishCallRequest{RunID: identity.RunID, ExpectedRunVersion: identity.ExpectedRunVersion, StageName: identity.StageName, AttemptID: identity.AttemptID, CallRecordID: binding.CallRecordID, Failure: failure, IdempotencyKey: coordinatorMutationID("finish", binding.CallRecordID), At: source.Now().UTC()}
		switch physical.State {
		case domain.PhysicalCompleted, domain.PhysicalUnknown:
			finish.DispatchKind = domain.DispatchDispatched
			finish.ResultAttemptCallID = &binding.AttemptCallID
		case domain.PhysicalAbortedNoDispatch:
			finish.DispatchKind = domain.DispatchNone
		default:
			errs = append(errs, errors.New("sandbox physical boundary still requires reconciliation"))
			continue
		}
		_, err = ledger.FinishCall(ctx, finish)
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func verifySandboxCleaned(ctx context.Context, store port.SandboxLifecycleReader, identity port.SandboxAuthorizationIdentity, plan port.ContainerPlan) error {
	execution, err := store.GetSandboxExecution(ctx, identity.SandboxExecutionID)
	if err != nil {
		return err
	}
	if execution.RunID != identity.RunID || execution.StageName != identity.StageName || execution.AttemptID != identity.AttemptID || execution.LogicalOperationID != identity.LogicalOperationID || execution.ScopeDigest != identity.ScopeDigest || execution.PlanDigest != plan.PlanDigest || execution.State != domain.SandboxExecutionCleaned {
		return errors.New("sandbox result lacks matching completed cleanup evidence")
	}
	return nil
}

var _ port.MeteredSandbox = (*DockerSandboxSession)(nil)

func sandboxOperationIdentity(c DockerSandboxConfig, kind domain.CallKind, request any) (port.SandboxAuthorizationIdentity, docker.PlanIdentity, error) {
	return sandboxReadOperationIdentity(c.ReadPolicy(), kind, request)
}

func sandboxReadOperationIdentity(c SandboxReadPolicy, kind domain.CallKind, request any) (port.SandboxAuthorizationIdentity, docker.PlanIdentity, error) {
	parent := c.Identity
	parent.ExpectedRunVersion = 0
	lockDigest, err := c.Lock.Digest()
	if err != nil {
		return port.SandboxAuthorizationIdentity{}, docker.PlanIdentity{}, err
	}
	raw, err := json.Marshal(struct {
		Schema       string
		Parent       port.SandboxAuthorizationIdentity
		Kind         domain.CallKind
		Request      any
		Config       docker.Config
		Lock, Engine domain.Digest
		Limits       docker.ControlLimits
	}{"cpgen.sandbox-operation/v1", parent, kind, request, c.Config, lockDigest, c.EngineIdentity, c.Limits})
	if err != nil {
		return port.SandboxAuthorizationIdentity{}, docker.PlanIdentity{}, err
	}
	scope := domain.SumBytes(raw)
	identity := c.Identity
	identity.Kind, identity.ScopeDigest = kind, scope
	identity.SandboxExecutionID = domain.SandboxExecutionID(coordinatorMutationID("sandbox", scope))
	identity.LogicalOperationID = "sandbox:" + string(identity.SandboxExecutionID)
	planIdentity := docker.PlanIdentity{RunID: identity.RunID, AttemptID: identity.AttemptID, SandboxExecutionID: identity.SandboxExecutionID, LogicalOperationID: identity.LogicalOperationID, OperationNonce: string(identity.SandboxExecutionID)[len("sandbox_"):], EngineIdentityDigest: c.EngineIdentity}
	return identity, planIdentity, nil
}

// SandboxReadPolicy contains only the immutable inputs used to reconstruct a
// committed execution plan. Readers cannot acquire an Engine, grant or writer
// through this value.
type SandboxReadPolicy struct {
	Identity       port.SandboxAuthorizationIdentity
	Config         docker.Config
	Lock           toolchain.Lock
	EngineIdentity domain.Digest
	Limits         docker.ControlLimits
}

func (c DockerSandboxConfig) ReadPolicy() SandboxReadPolicy {
	return SandboxReadPolicy{c.Identity, c.Config, c.Lock, c.EngineIdentity, c.Limits}
}
