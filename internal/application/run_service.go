package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

type RunService interface {
	Generate(context.Context, domain.RunRequest) (domain.RunSnapshot, error)
	Resume(context.Context, domain.RunID) (domain.RunSnapshot, error)
	Cancel(context.Context, domain.CancelRequest) (domain.RunSnapshot, error)
}

type SandboxReconciler interface {
	ReconcileRun(context.Context, domain.RunID) (domain.SandboxReconcileReport, error)
}

// RunRecovery is the narrow restart hook used by durable adapters that have
// their own persisted boundary state (for example an artifact writer). It is
// invoked from Resume while the run lock is held, before the interrupted
// stage is reset. Implementations must only replay or settle already durable
// identities; they must never plan new work.
type RunRecovery interface {
	RecoverRun(context.Context, domain.RunID) error
}

type CurrentStageAttemptReader interface {
	CurrentStageAttempt(context.Context, domain.RunID, domain.StageName) (domain.StageAttempt, error)
}

// RunViewProjectionReader is an optional narrow read seam implemented by the
// durable runtime store. It keeps workflow steps from depending on storage
// while ensuring the view contains the authoritative budget and artifact
// projections rather than coordinator-local guesses.
type RunViewProjectionReader interface {
	BudgetSnapshot(context.Context, domain.RunID) (domain.BudgetSnapshot, error)
	CommittedArtifactReferences(context.Context, domain.RunID) ([]domain.CommittedArtifactRef, error)
	RunViewDocuments(context.Context, domain.RunID) ([]byte, []byte, error)
}

type RunServiceConfig struct {
	Runtime             port.RuntimeStore
	Reviews             port.ReviewStore
	Locks               *runlock.Manager
	Pipeline            fake.Pipeline
	Clock               clock.Clock
	ActiveTimeInterval  time.Duration
	ControlPollInterval time.Duration
	Reconciler          SandboxReconciler
	Recovery            RunRecovery
	// EffectiveConfigJSON and EffectiveConfigDigest bind every newly created
	// run to the validated, redacted configuration that composed this service.
	// They are optional for direct unit-test composition; in that case Generate
	// retains its historical request-bound fallback.
	EffectiveConfigJSON   []byte
	EffectiveConfigDigest domain.Digest
	Scenario              string
}

type LocalRunService struct {
	runtime               port.RuntimeStore
	reviews               port.ReviewStore
	locks                 *runlock.Manager
	stages                *fixedStages
	control               *stageControl
	reconciler            SandboxReconciler
	attemptMu             sync.Mutex
	attempts              map[domain.RunID]domain.AttemptID
	archive               PackageArchiveReader
	graph                 *compiledRunGraph
	clock                 clock.Clock
	recovery              RunRecovery
	effectiveConfigJSON   []byte
	effectiveConfigDigest domain.Digest
	scenario              string
}

func NewRunService(config RunServiceConfig) (*LocalRunService, error) {
	return newRunService(config, workflow.FakeRevision, &fixedStages{pipeline: config.Pipeline})
}

func newRunService(config RunServiceConfig, revision string, stages *fixedStages) (*LocalRunService, error) {
	if config.Runtime == nil || config.Locks == nil || config.Clock == nil {
		return nil, errors.New("run service runtime, locks, and clock are required")
	}
	if stages == nil {
		return nil, errors.New("compiled stages are required")
	}
	compiled, err := newCompiledRunGraph(revision)
	if err != nil {
		return nil, err
	}
	if !compiled.definition.UsesGeneration() {
		if err := stages.pipeline.Validate(); err != nil {
			return nil, err
		}
	}
	if config.ActiveTimeInterval <= 0 {
		config.ActiveTimeInterval = time.Second
	}
	if config.ControlPollInterval <= 0 {
		config.ControlPollInterval = 100 * time.Millisecond
	}
	active, err := NewActiveTime(config.Runtime, config.Clock, config.ActiveTimeInterval)
	if err != nil {
		return nil, err
	}
	control := &stageControl{runtime: config.Runtime, clock: config.Clock, active: active, controlPollInterval: config.ControlPollInterval}
	service := &LocalRunService{runtime: config.Runtime, reviews: config.Reviews, locks: config.Locks, stages: stages, graph: compiled, control: control, reconciler: config.Reconciler, attempts: make(map[domain.RunID]domain.AttemptID), clock: config.Clock, recovery: config.Recovery, scenario: config.Scenario}

	if len(config.EffectiveConfigJSON) != 0 || config.EffectiveConfigDigest != "" {
		if len(config.EffectiveConfigJSON) == 0 || config.EffectiveConfigDigest == "" {
			return nil, errors.New("effective config JSON and digest must be supplied together")
		}
		if err := config.EffectiveConfigDigest.Validate(); err != nil {
			return nil, fmt.Errorf("effective config digest: %w", err)
		}
		if domain.SumBytes(config.EffectiveConfigJSON) != config.EffectiveConfigDigest {
			return nil, errors.New("effective config digest does not match JSON")
		}
		service.effectiveConfigJSON = append([]byte(nil), config.EffectiveConfigJSON...)
		service.effectiveConfigDigest = config.EffectiveConfigDigest
	}
	return service, nil
}

func NewLocalRunService(config RunServiceConfig) (*LocalRunService, error) {
	return NewRunService(config)
}

func (s *LocalRunService) Generate(ctx context.Context, request domain.RunRequest) (domain.RunSnapshot, error) {
	if s.graph.definition.UsesGeneration() {
		return s.generateGenerationRun(ctx, request)
	}
	if err := request.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	if request.SchemaVersion != domain.RequestSchemaV1 {
		return domain.RunSnapshot{}, errors.New("request schema is incompatible with compiled workflow")
	}
	runIDRaw, err := domain.NewID("run")
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	requestJSON, err := canonicalJSON(request)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	requestDigest := domain.SumBytes(requestJSON)
	workflowDigest := domain.SumBytes([]byte(workflow.FakeRevision))
	configJSON := append([]byte(nil), requestJSON...)
	configDigest := domain.SumBytes(configJSON)
	if len(s.effectiveConfigJSON) != 0 {
		configJSON = append([]byte(nil), s.effectiveConfigJSON...)
		configDigest = s.effectiveConfigDigest
	}
	seed := int64(0)
	if request.Seed != nil {
		seed = *request.Seed
	} else {
		seed = int64(len(request.Brief))
	}
	now := s.clock.Now().UTC()
	create := domain.CreateRunRequest{RunID: domain.RunID(runIDRaw), SubmittedRequestJSON: requestJSON, SubmittedRequestDigest: requestDigest, EffectiveSeed: seed, RedactedEffectiveConfigJSON: configJSON, RedactedEffectiveConfigDigest: configDigest, WorkflowRevision: workflow.FakeRevision, SchemaVersion: domain.SchemaVersion(request.SchemaVersion), WorkflowDigest: workflowDigest, BudgetLimits: request.BudgetLimits, StageSequence: append([]domain.StageName(nil), s.graph.stages...), CreatedAt: now, IdempotencyKey: stableServiceID("create", domain.RunID(runIDRaw), 1)}
	if err := create.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	scenario := request.Mode
	if s.scenario != "" {
		scenario = s.scenario
	}
	input := domain.FakeInput{Brief: request.Brief, RequestDigest: requestDigest, ConfigDigest: create.RedactedEffectiveConfigDigest, Scenario: scenario}
	if err := input.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	return s.execute(ctx, create.RunID, &create, input)
}

func (s *LocalRunService) Resume(ctx context.Context, runID domain.RunID) (domain.RunSnapshot, error) {
	return s.resume(ctx, runID, false, nil)
}

// ResumeImmediate checks the cross-process run lock once and returns a conflict
// when another executor owns it. Web admission uses this to avoid hidden queues.
func (s *LocalRunService) ResumeImmediate(ctx context.Context, runID domain.RunID) (domain.RunSnapshot, error) {
	return s.resume(ctx, runID, true, nil)
}

var ErrRunVersionConflict = errors.New("run version conflicts with expected version")

func (s *LocalRunService) ResumeExpectedImmediate(ctx context.Context, runID domain.RunID, expectedVersion int64) (domain.RunSnapshot, error) {
	return s.resume(ctx, runID, true, &expectedVersion)
}

func (s *LocalRunService) resume(ctx context.Context, runID domain.RunID, immediate bool, expectedVersion *int64) (domain.RunSnapshot, error) {
	if ctx == nil {
		return domain.RunSnapshot{}, errors.New("resume context is nil")
	}
	if err := ctx.Err(); err != nil {
		return domain.RunSnapshot{}, err
	}
	if err := runID.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	var err error
	var runGuard *runlock.Guard
	if immediate {
		runGuard, err = s.locks.TryAcquireRun(runID, runlock.Exclusive)
	} else {
		runGuard, err = s.locks.AcquireRun(ctx, runID, runlock.Exclusive)
	}
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	defer runGuard.Close()
	artifactGuard, err := s.locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	defer artifactGuard.Close()
	snapshot, err := s.runtime.GetRun(ctx, runID)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	if expectedVersion != nil && snapshot.Version != *expectedVersion {
		return snapshot, ErrRunVersionConflict
	}
	// Reject incompatible selectors before recovery, review application or any
	// other durable mutation. Old runs are never silently assigned a new graph.
	if err := s.validateGraphPersistence(ctx, snapshot); err != nil {
		return snapshot, err
	}
	if snapshot.State == domain.RunCancelled || snapshot.State == domain.RunFailed || snapshot.State == domain.RunReady {
		return snapshot, nil
	}
	if snapshot.State == domain.RunNeedsReview || snapshot.State == domain.RunBlocked {
		pending, err := s.runtime.PendingCancel(ctx, runID)
		if err != nil {
			return snapshot, err
		}
		if pending != nil {
			return s.finishCancellation(ctx, runID)
		}
	}
	if snapshot.State == domain.RunNeedsReview {
		snapshot, err = s.applyPendingReview(ctx, snapshot)
		if err != nil || snapshot.State == domain.RunNeedsReview {
			return snapshot, err
		}
	}
	if snapshot.State == domain.RunRunning {
		snapshot, err = s.recover(ctx, snapshot, s.attemptID(snapshot.RunID), s.graph.definition.PreservesAttempt())
		if err != nil {
			return domain.RunSnapshot{}, err
		}
	}
	// A cancellation request is durable and may have been written by a second
	// handle just before the owner died. Reconcile it before starting a fresh
	// stage attempt; otherwise a resumed custom/slow step could run forever
	// while a terminal control request is already waiting.
	if snapshot.State == domain.RunCreated || snapshot.State == domain.RunRunning {
		if pending, pendingErr := s.runtime.PendingCancel(ctx, runID); pendingErr != nil {
			return domain.RunSnapshot{}, pendingErr
		} else if pending != nil {
			return s.finishCancellation(ctx, runID)
		}
	}
	if snapshot.State == domain.RunCancelled || snapshot.State == domain.RunNeedsReview || snapshot.State == domain.RunFailed {
		return snapshot, nil
	}
	input := s.replayInput(snapshot)
	return s.executeExisting(ctx, snapshot, input)
}

func (s *LocalRunService) Cancel(ctx context.Context, request domain.CancelRequest) (domain.RunSnapshot, error) {
	if err := request.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	if s.graph.definition.UsesGeneration() {
		current, err := s.runtime.GetRun(ctx, request.RunID)
		if err != nil {
			return domain.RunSnapshot{}, err
		}
		if err := s.validateGraphPersistence(ctx, current); err != nil {
			return current, err
		}
	}
	if _, err := s.runtime.RequestCancel(ctx, request); err != nil {
		return domain.RunSnapshot{}, err
	}
	guard, err := s.locks.TryAcquireRun(request.RunID, runlock.Exclusive)
	if errors.Is(err, runlock.ErrBusy) {
		return s.runtime.GetRun(ctx, request.RunID)
	}
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	defer guard.Close()
	artifactGuard, err := s.locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	defer artifactGuard.Close()
	return s.finishCancellation(ctx, request.RunID)
}

func (s *LocalRunService) execute(ctx context.Context, runID domain.RunID, create *domain.CreateRunRequest, input domain.FakeInput) (domain.RunSnapshot, error) {
	runGuard, err := s.locks.AcquireRun(ctx, runID, runlock.Exclusive)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	defer runGuard.Close()
	artifactGuard, err := s.locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	defer artifactGuard.Close()
	snapshot, err := s.runtime.CreateRun(ctx, *create)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	return s.executeExisting(ctx, snapshot, input)
}

func (s *LocalRunService) executeExisting(ctx context.Context, snapshot domain.RunSnapshot, input domain.FakeInput) (domain.RunSnapshot, error) {
	if err := s.validateGraphPersistence(ctx, snapshot); err != nil {
		return snapshot, err
	}
	return s.graph.run(ctx, snapshot, func(ctx context.Context, current domain.RunSnapshot) (domain.RunSnapshot, error) {
		if s.graph.definition.UsesGeneration() {
			next, err := s.stages.readGenerationInput(ctx, current)
			if err != nil {
				return current, err
			}
			return s.executeStage(ctx, current, next)
		}
		var next any
		switch current.CurrentStage {
		case "prepare":
			next = input
		case "exercise":
			next = s.replayPrepared(current)
		case "checkpoint":
			next = s.replayEvidence(current)
		default:
			return current, fmt.Errorf("unsupported slice1 stage %q", current.CurrentStage)
		}
		return s.executeStage(ctx, current, next)
	})
}

func (s *LocalRunService) validateGraphPersistence(ctx context.Context, snapshot domain.RunSnapshot) error {
	return validateRunPersistence(ctx, s.runtime, s.graph, s.effectiveConfigDigest, snapshot)
}

func validateRunPersistence(ctx context.Context, store port.RuntimeStore, graph *compiledRunGraph, configDigest domain.Digest, snapshot domain.RunSnapshot) error {
	if err := graph.validateSnapshot(snapshot); err != nil {
		return err
	}
	if graph.definition.UsesGeneration() && snapshot.ConfigDigest != configDigest {
		return errors.New("live run configuration differs from its frozen execution settings")
	}
	sequence, err := store.StageSequence(ctx, snapshot.RunID)
	if err != nil {
		return err
	}
	if !slices.Equal(sequence, graph.stages) {
		return errors.New("persisted stage sequence differs from compiled workflow")
	}
	return nil
}

func convertResult[O any](result domain.AgentResult[O]) domain.AgentResult[any] {
	if result.Value != nil {
		value := any(*result.Value)
		return domain.Success(value)
	}
	if result.Retryable != nil {
		return domain.Retry[any](*result.Retryable)
	}
	if result.Blocked != nil {
		return domain.Blocked[any](*result.Blocked)
	}
	if result.Review != nil {
		return domain.Review[any](*result.Review)
	}
	if result.Failure != nil {
		return domain.Failure[any](*result.Failure)
	}
	if result.Cancellation != nil {
		return domain.Cancelled[any](*result.Cancellation)
	}
	return domain.AgentResult[any]{}
}

// Executed runs must cross reconcileForTerminal before cancellation or budget
// exhaustion is committed. Only TryCancelUnstarted's transactional proof of no
// execution permits cancellation without reconciliation. A non-complete report
// means exact external ownership remains unresolved and the run non-terminal.

// cleanupEvidenceMatchesRun is deliberately conservative. Exit code 10 is
// reserved for a report that names this run and contains durable unresolved
// sandbox evidence. A zero-value report is commonly returned alongside an
// inspection/engine error; treating its Completed bit as cleanup evidence
// would hide the original failure behind a misleading cleanup-pending state.
func cleanupEvidenceMatchesRun(runID domain.RunID, report domain.SandboxReconcileReport) bool {
	if runID == "" || report.RunID != runID {
		return false
	}
	if report.Pending > 0 && len(report.Executions) == 0 && len(report.Resources) == 0 && len(report.ManualCleanup) == 0 {
		return false
	}
	if report.Pending <= 0 && len(report.ManualCleanup) == 0 {
		return false
	}
	for _, blocker := range report.ManualCleanup {
		if !blocker.Manual || blocker.ExecutionID == "" || strings.TrimSpace(blocker.Reason) == "" {
			return false
		}
	}
	return true
}

func causePointer(cause domain.ExecutionCause) *domain.ExecutionCause { return &cause }
func (s *LocalRunService) replayInput(snapshot domain.RunSnapshot) domain.FakeInput {
	input := domain.FakeInput{Brief: "resume", RequestDigest: snapshot.RequestDigest, ConfigDigest: snapshot.ConfigDigest}
	if s.scenario != "" {
		input.Scenario = s.scenario
	}
	return input
}
func (s *LocalRunService) replayPrepared(snapshot domain.RunSnapshot) domain.FakePrepared {
	digest := domain.SumBytes([]byte(fmt.Sprintf("slice1.prepare/v1\x00%s\x00%s\x00%s", snapshot.RequestDigest, snapshot.ConfigDigest, snapshot.WorkflowDigest)))
	return domain.FakePrepared{Digest: digest, Summary: "slice1 prepared"}
}
func (s *LocalRunService) replayEvidence(snapshot domain.RunSnapshot) domain.FakeEvidence {
	prepared := s.replayPrepared(snapshot)
	digest := domain.SumBytes([]byte(fmt.Sprintf("slice1.exercise/v1\x00%s\x00%s", prepared.Digest, snapshot.ConfigDigest)))
	return domain.FakeEvidence{Digest: digest, PreparedDigest: prepared.Digest}
}
func stableValueDigest(value any) (domain.Digest, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return domain.SumBytes(encoded), nil
}

func stageInputDigest(value any) (domain.Digest, error) {
	switch typed := value.(type) {
	case domain.FakeInput:
		return typed.RequestDigest, nil
	case domain.FakePrepared:
		return typed.Digest, nil
	case domain.FakeEvidence:
		return typed.Digest, nil
	case domain.GenerationRequestSnapshotV1:
		return typed.Digest()
	case domain.StatementInput:
		return typed.Digest()
	case domain.SimilarityInputV1:
		return typed.Digest()
	case SimilarityContent:
		return typed.Evidence.EvidenceDigest, typed.Decision.Validate()
	case domain.SolutionDraftInputV1:
		return typed.Digest()
	case domain.DataDraftInputV1:
		return typed.Digest()
	case domain.DataContent:
		return typed.ContentDigest, typed.Validate()
	case domain.SolutionContent:
		return typed.ContentDigest, typed.Validate()
	default:
		return stableValueDigest(value)
	}
}

func stageOutputDigest(value any) (domain.Digest, error) {
	switch typed := value.(type) {
	case domain.FakePrepared:
		return typed.Digest, nil
	case domain.FakeEvidence:
		return typed.Digest, nil
	case domain.FakeCheckpoint:
		return typed.Digest, nil
	default:
		return stableValueDigest(value)
	}
}

func canonicalJSON(value any) ([]byte, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}
func stableServiceID(prefix string, runID domain.RunID, version int64) string {
	digest := domain.SumBytes([]byte(fmt.Sprintf("cpgen.service/v1:%s:%s:%d", prefix, runID, version)))
	// Mutation idempotency keys share the domain ID grammar. Keep the semantic
	// prefix in the digest, but normalize its presentation so a hyphenated
	// operation name (for example, cancel-finalize) cannot produce an invalid
	// ControlRequestID at the terminal projection boundary.
	prefix = strings.ReplaceAll(prefix, "-", "_")
	return prefix + "_" + string(digest[len("sha256:"):len("sha256:")+32])
}

func (s *LocalRunService) attemptID(run domain.RunID) domain.AttemptID {
	s.attemptMu.Lock()
	defer s.attemptMu.Unlock()
	return s.attempts[run]
}
