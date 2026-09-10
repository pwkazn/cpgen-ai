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

	"cpgen/internal/adapter/storage/sqlite"
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

// ErrCleanupPending means the exact external sandbox resources are still
// unresolved. It is deliberately separate from a generic host failure so the
// CLI can report exit code 10 while preserving the RUNNING projection.
var ErrCleanupPending = errors.New("sandbox cleanup is pending")

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
	Runtime            port.RuntimeStore
	Reviews            port.ReviewStore
	Locks              *runlock.Manager
	Pipeline           workflow.Slice1Pipeline
	Clock              clock.Clock
	ActiveTimeInterval time.Duration
	Reconciler         SandboxReconciler
	Recovery           RunRecovery
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
	pipeline              workflow.Slice1Pipeline
	generation            *GenerationExecutor
	similarity            *SimilarityExecutor
	solution              *SolutionExecutor
	data                  *DataExecutor
	quality               *QualityExecutor
	packages              *PackageExecutor
	solutionSandbox       *DockerSandboxConfig
	graph                 *compiledRunGraph
	clock                 clock.Clock
	active                *ActiveTime
	reconciler            SandboxReconciler
	recovery              RunRecovery
	effectiveConfigJSON   []byte
	effectiveConfigDigest domain.Digest
	scenario              string
	mu                    sync.Mutex
	attempts              map[domain.RunID]domain.AttemptID
	closeSandbox          func() error
	closeOnce             sync.Once
	closeErr              error
}

func (s *LocalRunService) Close() error {
	s.closeOnce.Do(func() {
		if s.closeSandbox != nil {
			s.closeErr = s.closeSandbox()
		}
	})
	return s.closeErr
}

func NewRunService(config RunServiceConfig) (*LocalRunService, error) {
	return newRunService(config, nil, nil)
}

func newRunService(config RunServiceConfig, generation *GenerationExecutor, similarity *SimilarityExecutor) (*LocalRunService, error) {
	if config.Runtime == nil || config.Locks == nil || config.Clock == nil {
		return nil, errors.New("run service runtime, locks, and clock are required")
	}
	revision := workflow.Slice1WorkflowRevision
	if generation == nil {
		if err := config.Pipeline.Validate(); err != nil {
			return nil, err
		}
	} else {
		if similarity == nil {
			return nil, errors.New("live run service requires its similarity executor")
		}
		revision = similarity.config.WorkflowRevision
	}
	compiled, err := newCompiledRunGraph(revision)
	if err != nil {
		return nil, err
	}
	if config.ActiveTimeInterval <= 0 {
		config.ActiveTimeInterval = time.Second
	}
	active, err := NewActiveTime(config.Runtime, config.Clock, config.ActiveTimeInterval)
	if err != nil {
		return nil, err
	}
	service := &LocalRunService{runtime: config.Runtime, reviews: config.Reviews, locks: config.Locks, pipeline: config.Pipeline, clock: config.Clock, active: active, reconciler: config.Reconciler, recovery: config.Recovery, attempts: make(map[domain.RunID]domain.AttemptID), scenario: config.Scenario}
	service.graph = compiled
	service.generation, service.similarity = generation, similarity
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
	if s.generation != nil {
		return s.generateSlice2(ctx, request)
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
	workflowDigest := domain.SumBytes([]byte(workflow.Slice1WorkflowRevision))
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
	create := domain.CreateRunRequest{RunID: domain.RunID(runIDRaw), SubmittedRequestJSON: requestJSON, SubmittedRequestDigest: requestDigest, EffectiveSeed: seed, RedactedEffectiveConfigJSON: configJSON, RedactedEffectiveConfigDigest: configDigest, WorkflowRevision: workflow.Slice1WorkflowRevision, SchemaVersion: domain.SchemaVersion(request.SchemaVersion), WorkflowDigest: workflowDigest, BudgetLimits: request.BudgetLimits, StageSequence: []domain.StageName{"prepare", "exercise", "checkpoint"}, CreatedAt: now, IdempotencyKey: stableServiceID("create", domain.RunID(runIDRaw), 1)}
	if err := create.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	scenario := request.Mode
	if s.scenario != "" {
		scenario = s.scenario
	}
	input := domain.Slice1Input{Brief: request.Brief, RequestDigest: requestDigest, ConfigDigest: create.RedactedEffectiveConfigDigest, Scenario: scenario}
	if err := input.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	return s.execute(ctx, create.RunID, &create, input)
}

func (s *LocalRunService) Resume(ctx context.Context, runID domain.RunID) (domain.RunSnapshot, error) {
	if err := runID.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
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
	snapshot, err := s.runtime.GetRun(ctx, runID)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	// Reject incompatible selectors before recovery, review application or any
	// other durable mutation. Old runs are never silently assigned a new graph.
	if err := s.validateGraphPersistence(ctx, snapshot); err != nil {
		return snapshot, err
	}
	if snapshot.State == domain.RunCancelled || snapshot.State == domain.RunFailed || snapshot.State == domain.RunReady {
		return snapshot, nil
	}
	if snapshot.State == domain.RunNeedsReview {
		if s.reviews != nil {
			if review, reviewErr := s.reviews.PendingReview(ctx, runID); reviewErr != nil {
				return domain.RunSnapshot{}, reviewErr
			} else if review != nil {
				snapshot, err = s.applyReview(ctx, snapshot, *review)
			}
		}
		if err != nil || snapshot.State == domain.RunNeedsReview {
			return snapshot, err
		}
	}
	if snapshot.State == domain.RunRunning {
		snapshot, err = s.recoverRunning(ctx, snapshot)
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
	if s.generation != nil {
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

func (s *LocalRunService) execute(ctx context.Context, runID domain.RunID, create *domain.CreateRunRequest, input domain.Slice1Input) (domain.RunSnapshot, error) {
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

func (s *LocalRunService) executeExisting(ctx context.Context, snapshot domain.RunSnapshot, input domain.Slice1Input) (domain.RunSnapshot, error) {
	if err := s.validateGraphPersistence(ctx, snapshot); err != nil {
		return snapshot, err
	}
	return s.graph.run(ctx, snapshot, func(ctx context.Context, current domain.RunSnapshot) (domain.RunSnapshot, error) {
		if s.generation != nil {
			next, err := s.readSlice2Input(ctx, current)
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
	if err := s.graph.validateSnapshot(snapshot); err != nil {
		return err
	}
	if s.generation != nil && snapshot.ConfigDigest != s.effectiveConfigDigest {
		return errors.New("live run configuration differs from its frozen execution settings")
	}
	sequence, err := s.runtime.StageSequence(ctx, snapshot.RunID)
	if err != nil {
		return err
	}
	if !slices.Equal(sequence, s.graph.stages) {
		return errors.New("persisted stage sequence differs from compiled workflow")
	}
	return nil
}

func (s *LocalRunService) executeStage(ctx context.Context, snapshot domain.RunSnapshot, input any) (domain.RunSnapshot, error) {
	wasBlocked := snapshot.State == domain.RunBlocked
	var blockedBinding *domain.BlockedCheckpoint
	if wasBlocked {
		reader, ok := s.runtime.(CurrentStageAttemptReader)
		if !ok {
			return snapshot, errors.New("blocked resume requires persisted attempt reader")
		}
		persisted, readErr := reader.CurrentStageAttempt(ctx, snapshot.RunID, snapshot.CurrentStage)
		if readErr != nil {
			return snapshot, fmt.Errorf("read blocked checkpoint: %w", readErr)
		}
		if persisted.State != domain.StageAttemptBlocked || persisted.BlockedBinding == nil {
			return snapshot, errors.New("blocked resume lacks exact persisted checkpoint binding")
		}
		binding := *persisted.BlockedBinding
		blockedBinding = &binding
	}
	inputDigest, err := stageInputDigest(input)
	if err != nil {
		return snapshot, err
	}
	if blockedBinding != nil {
		if inputDigest != blockedBinding.StageInputDigest {
			return snapshot, errors.New("blocked resume input differs from persisted checkpoint")
		}
		inputDigest = blockedBinding.StageInputDigest
	}
	attempt, err := s.beginOrResumeStage(ctx, snapshot, inputDigest)
	if err != nil {
		return snapshot, err
	}
	s.mu.Lock()
	s.attempts[snapshot.RunID] = attempt.AttemptID
	s.mu.Unlock()
	snapshot, err = s.runtime.GetRun(ctx, snapshot.RunID)
	if err != nil {
		return snapshot, err
	}
	if wasBlocked && s.generation == nil {
		view, viewErr := s.view(ctx, snapshot, attempt.AttemptID)
		if viewErr != nil {
			return snapshot, viewErr
		}
		ok, revalidateErr := s.pipeline.Revalidate(ctx, view, snapshot.CurrentStage, *blockedBinding)
		if revalidateErr != nil || !ok {
			result := domain.Blocked[any](*blockedBinding)
			if revalidateErr != nil {
				return s.finishOutcome(ctx, snapshot, attempt, inputDigest, result)
			}
			return s.finishOutcome(ctx, snapshot, attempt, inputDigest, result)
		}
	}
	active, activeErr := s.active.Start(ctx, snapshot.RunID, snapshot.Version)
	if activeErr != nil {
		return snapshot, activeErr
	}
	snapshot, err = s.runtime.GetRun(ctx, snapshot.RunID)
	if err != nil {
		return snapshot, err
	}
	if active.Exhausted {
		result := domain.Review[any](domain.ReviewRequest{EvidenceDigest: domain.SumBytes([]byte("active-time exhausted")), PolicyDigest: snapshot.ConfigDigest, Reason: "active_time_exhausted"})
		if _, stopErr := s.active.Stop(ctx, snapshot.RunID, snapshot.Version); stopErr != nil {
			return snapshot, stopErr
		}
		snapshot, err = s.runtime.GetRun(ctx, snapshot.RunID)
		if err != nil {
			return snapshot, err
		}
		if reconcileErr := s.reconcileForTerminal(ctx, snapshot.RunID); reconcileErr != nil {
			return snapshot, reconcileErr
		}
		snapshot, err = s.runtime.GetRun(ctx, snapshot.RunID)
		if err != nil {
			return snapshot, err
		}
		return s.finishOutcome(ctx, snapshot, attempt, inputDigest, result)
	}
	stageCtx, cancelCause := context.WithCancelCause(ctx)
	pollDone := make(chan struct{})
	accountDone := make(chan struct{})
	accountExhausted := make(chan bool, 1)
	accountErrors := make(chan error, 1)
	go s.cancelPoller(stageCtx, snapshot.RunID, func() { cancelCause(domain.ExecutionInterrupted{Cause: domain.CauseUserCancel}) }, pollDone)
	go s.accountingPoller(stageCtx, snapshot.RunID, func() { cancelCause(domain.ExecutionInterrupted{Cause: domain.CauseRunBudgetDeadline}) }, accountDone, accountExhausted, accountErrors)
	execution, runErr := s.runStage(stageCtx, snapshot, input)
	result := execution.result
	cancelCause(nil)
	<-pollDone
	<-accountDone
	current, getErr := s.runtime.GetRun(ctx, snapshot.RunID)
	if getErr != nil {
		return snapshot, getErr
	}
	exhausted := false
	select {
	case exhausted = <-accountExhausted:
	default:
	}
	if current.ActiveStartedAt != nil {
		stopResult, stopErr := s.active.Stop(ctx, current.RunID, current.Version)
		if stopErr != nil {
			return current, stopErr
		}
		exhausted = stopResult.Exhausted
	}
	current, err = s.runtime.GetRun(ctx, snapshot.RunID)
	if err != nil {
		return snapshot, err
	}
	var accountingErr error
	select {
	case accountingErr = <-accountErrors:
	default:
	}
	if accountingErr != nil {
		return current, accountingErr
	}
	interruptedCause := domain.ExecutionCause("")
	if interrupted, ok := context.Cause(stageCtx).(domain.ExecutionInterrupted); ok {
		interruptedCause = interrupted.Cause
	}
	userCancelled := (result.Cancellation != nil && result.Cancellation.Cause == domain.CauseUserCancel) || interruptedCause == domain.CauseUserCancel
	if userCancelled || s.hasCancel(ctx, current.RunID) {
		return s.finishCancellation(ctx, current.RunID)
	}
	budgetExhausted := exhausted || (result.Cancellation != nil && result.Cancellation.Cause == domain.CauseRunBudgetDeadline) || interruptedCause == domain.CauseRunBudgetDeadline
	if budgetExhausted {
		result = domain.Review[any](domain.ReviewRequest{EvidenceDigest: domain.SumBytes([]byte("active-time exhausted")), PolicyDigest: current.ConfigDigest, Reason: "active_time_exhausted"})
		if reconcileErr := s.reconcileForTerminal(ctx, current.RunID); reconcileErr != nil {
			return current, reconcileErr
		}
		current, err = s.runtime.GetRun(ctx, current.RunID)
		if err != nil {
			return current, err
		}
		execution = stageExecution{result: result}
	} else if runErr != nil {
		return current, runErr
	}
	return s.finishExecution(ctx, current, attempt, inputDigest, execution)
}

func (s *LocalRunService) accountingPoller(ctx context.Context, runID domain.RunID, cancel func(), done chan<- struct{}, exhausted chan<- bool, errorsOut chan<- error) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(s.active.interval):
			snapshot, err := s.runtime.GetRun(context.Background(), runID)
			if err != nil || snapshot.ActiveStartedAt == nil {
				if err != nil {
					select {
					case errorsOut <- err:
					default:
					}
					cancel()
					return
				}
				continue
			}
			result, err := s.active.Heartbeat(context.Background(), runID, snapshot.Version)
			if err != nil {
				// Provider settlement or a second handle's control request may
				// advance the version after this read. Retry accounting on the
				// next bounded tick; this conflict is not a budget cancellation.
				if errors.Is(err, sqlite.ErrVersionConflict) {
					continue
				}
				select {
				case errorsOut <- err:
				default:
				}
				cancel()
				return
			}
			if result.Exhausted {
				select {
				case exhausted <- true:
				default:
				}
				cancel()
				return
			}
		}
	}
}

func (s *LocalRunService) runTyped(ctx context.Context, viewSnapshot domain.RunSnapshot, input any) (domain.AgentResult[any], error) {
	view, err := s.view(ctx, viewSnapshot, s.currentAttempt(viewSnapshot.RunID))
	if err != nil {
		return domain.AgentResult[any]{}, err
	}
	switch typed := input.(type) {
	case domain.Slice1Input:
		result, err := s.pipeline.RunPrepare(ctx, view, typed)
		return convertResult(result), err
	case domain.Slice1Prepared:
		result, err := s.pipeline.RunExercise(ctx, view, typed)
		return convertResult(result), err
	case domain.Slice1Evidence:
		result, err := s.pipeline.RunCheckpoint(ctx, view, typed)
		return convertResult(result), err
	default:
		return domain.AgentResult[any]{}, errors.New("unsupported typed stage input")
	}
}

func (s *LocalRunService) view(ctx context.Context, snapshot domain.RunSnapshot, attemptID domain.AttemptID) (domain.RunView, error) {
	data := domain.RunViewData{RunID: snapshot.RunID, AttemptID: attemptID, WorkflowRevision: snapshot.WorkflowRevision, SchemaVersion: snapshot.SchemaVersion, RequestDigest: snapshot.RequestDigest, ConfigDigest: snapshot.ConfigDigest, WorkflowDigest: snapshot.WorkflowDigest, State: snapshot.State, CurrentStage: snapshot.CurrentStage, Version: snapshot.Version, Budget: domain.BudgetSnapshot{Limits: domain.BudgetLimits{}}}
	if reader, ok := s.runtime.(RunViewProjectionReader); ok {
		budget, err := reader.BudgetSnapshot(ctx, snapshot.RunID)
		if err != nil {
			return domain.RunView{}, err
		}
		artifacts, err := reader.CommittedArtifactReferences(ctx, snapshot.RunID)
		if err != nil {
			return domain.RunView{}, err
		}
		requestJSON, configJSON, err := reader.RunViewDocuments(ctx, snapshot.RunID)
		if err != nil {
			return domain.RunView{}, err
		}
		data.Budget, data.CommittedArtifacts = budget, artifacts
		data.RequestJSON, data.ConfigJSON = requestJSON, configJSON
	}
	return domain.NewRunView(data)
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

func (s *LocalRunService) finishOutcome(ctx context.Context, snapshot domain.RunSnapshot, attempt domain.StageAttempt, inputDigest domain.Digest, result domain.AgentResult[any]) (domain.RunSnapshot, error) {
	return s.finishExecution(ctx, snapshot, attempt, inputDigest, stageExecution{result: result})
}

func (s *LocalRunService) finishExecution(ctx context.Context, snapshot domain.RunSnapshot, attempt domain.StageAttempt, inputDigest domain.Digest, execution stageExecution) (domain.RunSnapshot, error) {
	result := execution.result
	if err := result.Validate(); err != nil {
		return snapshot, err
	}
	at := s.clock.Now().UTC()
	command := domain.FinishStageCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: attempt.AttemptID, IdempotencyKey: stableServiceID("finish", snapshot.RunID, snapshot.Version), At: at}
	if result.Value != nil {
		if binding, ok := (*result.Value).(domain.VerifiedPackageBinding); ok {
			if s.packages == nil || snapshot.CurrentStage != "package" || inputDigest != binding.QualityDigest {
				return snapshot, errors.New("verified package differs from current stage input")
			}
			store, ok := s.runtime.(interface {
				FinalizeVerifiedPackage(context.Context, domain.FinalizeVerifiedPackageCommand) (domain.RunSnapshot, error)
			})
			if !ok {
				return snapshot, errors.New("runtime lacks atomic verified package completion")
			}
			command.AttemptState, command.RunState = domain.StageAttemptSucceeded, domain.RunRunning
			command.OutputDigest, command.NextInputDigest = &binding.Archive.Digest, &binding.Archive.Digest
			command.NextStage, command.Occurrences = "package", execution.occurrences
			finished, err := store.FinalizeVerifiedPackage(ctx, domain.FinalizeVerifiedPackageCommand{Finish: command, Package: binding})
			if err != nil {
				return snapshot, err
			}
			return finished, nil
		}
	}
	switch {
	case result.Value != nil:
		output, err := stageOutputDigest(*result.Value)
		if err != nil {
			return snapshot, err
		}
		command.AttemptState, command.RunState, command.OutputDigest = domain.StageAttemptSucceeded, domain.RunRunning, &output
		next := nextStage(snapshot.CurrentStage)
		if s.generation != nil {
			next = ""
			if snapshot.CurrentStageOrdinal > 0 && snapshot.CurrentStageOrdinal < len(s.graph.stages) {
				next = s.graph.stages[snapshot.CurrentStageOrdinal]
			}
		}
		if next == "" {
			return snapshot, errors.New("slice1 terminal stage must return review")
		}
		command.NextStage, command.NextInputDigest = next, &output
	case result.Blocked != nil, result.Retryable != nil:
		command.AttemptState, command.RunState = domain.StageAttemptBlocked, domain.RunBlocked
		if result.Blocked != nil {
			command.BlockedBinding = result.Blocked
		} else {
			// Retryable failures share the durable BLOCKED stage state. Give
			// them an explicit input/policy checkpoint as well, so restart does
			// not silently retry against an unbound dependency.
			now := s.clock.Now().UTC()
			binding := domain.BlockedCheckpoint{RunID: snapshot.RunID, StageName: snapshot.CurrentStage, StageInputDigest: inputDigest, DependencyID: "slice1-retry", DependencyDigest: inputDigest, PolicyDigest: snapshot.ConfigDigest, ErrorDigest: result.Retryable.Evidence, RetryAfter: now, CreatedAt: now}
			command.BlockedBinding = &binding
		}
	case result.Review != nil:
		command.AttemptState, command.RunState = domain.StageAttemptNeedsReview, domain.RunNeedsReview
		command.ReviewEvidenceDigest, command.ReviewPolicyDigest = &result.Review.EvidenceDigest, &result.Review.PolicyDigest
	case result.Failure != nil:
		command.AttemptState, command.RunState = domain.StageAttemptFailed, domain.RunFailed
	default:
		return snapshot, errors.New("unsupported stage result")
	}
	if result.Value != nil && s.generation != nil {
		if err := s.bindSlice2Commit(ctx, snapshot, *result.Value, &command); err != nil {
			return snapshot, err
		}
	}
	if result.Value != nil {
		command.Occurrences = execution.occurrences
	}
	finished, err := s.runtime.FinishStage(ctx, command)
	if err != nil {
		return snapshot, err
	}
	// Cache publication is an optional index. A failed index cannot undo an
	// authoritative committed stage or prevent the graph observing its advance.
	if result.Value != nil && execution.publishCache != nil {
		_ = execution.publishCache(ctx)
	}
	return finished, nil
}

func (s *LocalRunService) finishCancellation(ctx context.Context, runID domain.RunID) (domain.RunSnapshot, error) {
	snapshot, err := s.runtime.GetRun(ctx, runID)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	if snapshot.State == domain.RunCancelled {
		return snapshot, nil
	}
	pending, err := s.runtime.PendingCancel(ctx, runID)
	if err != nil {
		return snapshot, err
	}
	if pending == nil {
		return snapshot, errors.New("cancellation has no pending control request")
	}
	if reconcileErr := s.reconcileForTerminal(ctx, runID); reconcileErr != nil {
		return snapshot, reconcileErr
	}
	snapshot, err = s.runtime.GetRun(ctx, runID)
	if err != nil {
		return snapshot, err
	}
	if snapshot.ActiveStartedAt != nil {
		if _, err := s.active.Stop(ctx, runID, snapshot.Version); err != nil {
			return snapshot, err
		}
		snapshot, err = s.runtime.GetRun(ctx, runID)
		if err != nil {
			return snapshot, err
		}
	}
	if snapshot.State == domain.RunRunning {
		attemptID := s.currentAttempt(runID)
		if reader, ok := s.runtime.(CurrentStageAttemptReader); ok {
			persisted, readErr := reader.CurrentStageAttempt(ctx, runID, snapshot.CurrentStage)
			if readErr != nil && !errors.Is(readErr, sqlite.ErrNotFound) {
				return snapshot, readErr
			}
			attemptID = ""
			if readErr == nil && persisted.State == domain.StageAttemptRunning {
				attemptID = persisted.AttemptID
			}
		}
		if attemptID != "" {
			if _, err := s.runtime.FinishStage(ctx, domain.FinishStageCommand{RunID: runID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: attemptID, AttemptState: domain.StageAttemptCancelled, RunState: domain.RunCancelled, Cause: causePointer(domain.CauseUserCancel), IdempotencyKey: stableServiceID("cancel-finish", runID, snapshot.Version), At: s.clock.Now().UTC()}); err == nil {
				return s.runtime.GetRun(ctx, runID)
			} else {
				return snapshot, err
			}
		}
	}
	return s.runtime.FinalizeCancel(ctx, domain.FinalizeCancelCommand{RunID: runID, ExpectedRunVersion: snapshot.Version, ControlRequestID: pending.ID, ReconciliationDigest: domain.SumBytes([]byte("slice1 cancellation reconciliation")), IdempotencyKey: stableServiceID("cancel-finalize", runID, snapshot.Version), At: s.clock.Now().UTC()})
}

// reconcileForTerminal is the sole boundary before a cancellation or budget
// exhaustion projection is committed. A non-complete report means exact
// external ownership is still unresolved, so the run remains non-terminal.
func (s *LocalRunService) reconcileForTerminal(ctx context.Context, runID domain.RunID) error {
	if s.generation != nil {
		if err := s.reconcileSlice2(ctx, runID); err != nil {
			return err
		}
	}
	if s.reconciler == nil {
		return nil
	}
	report, err := s.reconciler.ReconcileRun(ctx, runID)
	if err != nil {
		if cleanupEvidenceMatchesRun(runID, report) {
			return fmt.Errorf("%w: %v", ErrCleanupPending, err)
		}
		return fmt.Errorf("reconcile sandbox resources: %w", err)
	}
	if cleanupEvidenceMatchesRun(runID, report) {
		return fmt.Errorf("%w: sandbox cleanup is not settled", ErrCleanupPending)
	}
	if !report.Completed {
		return errors.New("reconcile sandbox resources: incomplete report without cleanup evidence")
	}
	return nil
}

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
func (s *LocalRunService) hasCancel(ctx context.Context, runID domain.RunID) bool {
	pending, err := s.runtime.PendingCancel(ctx, runID)
	return err == nil && pending != nil
}
func (s *LocalRunService) currentAttempt(runID domain.RunID) domain.AttemptID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[runID]
}
func (s *LocalRunService) cancelPoller(ctx context.Context, runID domain.RunID, cancel context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(10 * time.Millisecond):
			if s.hasCancel(context.Background(), runID) {
				cancel()
				return
			}
		}
	}
}

func (s *LocalRunService) recoverRunning(ctx context.Context, snapshot domain.RunSnapshot) (domain.RunSnapshot, error) {
	if s.recovery != nil {
		if err := s.recovery.RecoverRun(ctx, snapshot.RunID); err != nil {
			return snapshot, err
		}
	}
	if s.reconciler != nil {
		report, err := s.reconciler.ReconcileRun(ctx, snapshot.RunID)
		if err != nil {
			if cleanupEvidenceMatchesRun(snapshot.RunID, report) {
				return snapshot, fmt.Errorf("%w: %v", ErrCleanupPending, err)
			}
			return snapshot, err
		}
		if cleanupEvidenceMatchesRun(snapshot.RunID, report) {
			return snapshot, fmt.Errorf("%w: sandbox recovery cleanup is not settled", ErrCleanupPending)
		}
		if !report.Completed {
			return snapshot, errors.New("sandbox recovery reconciliation incomplete without cleanup evidence")
		}
	}
	if snapshot.ActiveStartedAt != nil {
		if _, err := s.active.Recover(ctx, snapshot.RunID, snapshot.Version); err != nil {
			return snapshot, err
		}
		updated, getErr := s.runtime.GetRun(ctx, snapshot.RunID)
		if getErr != nil {
			return snapshot, getErr
		}
		snapshot = updated
		if snapshot.ActiveStartedAt != nil && snapshot.LastAccountingHeartbeatAt != nil {
			bound := snapshot.LastAccountingHeartbeatAt.Add(s.active.interval)
			if _, stopErr := s.active.stopAt(ctx, snapshot.RunID, snapshot.Version, bound); stopErr != nil {
				return snapshot, stopErr
			}
			snapshot, getErr = s.runtime.GetRun(ctx, snapshot.RunID)
			if getErr != nil {
				return snapshot, getErr
			}
		}
	}
	attempt := s.currentAttempt(snapshot.RunID)
	if reader, ok := s.runtime.(CurrentStageAttemptReader); ok {
		persisted, readerErr := reader.CurrentStageAttempt(ctx, snapshot.RunID, snapshot.CurrentStage)
		if readerErr != nil && !errors.Is(readerErr, sqlite.ErrNotFound) {
			return snapshot, readerErr
		}
		attempt = ""
		if readerErr == nil && persisted.State == domain.StageAttemptRunning {
			attempt = persisted.AttemptID
		}
	}
	if attempt == "" || s.generation != nil {
		return snapshot, nil
	}
	return s.runtime.InterruptStage(ctx, domain.InterruptStageCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: attempt, Cause: domain.CauseRevisionInvalidated, IdempotencyKey: stableServiceID("interrupt", snapshot.RunID, snapshot.Version), At: s.clock.Now().UTC()})
}

func (s *LocalRunService) applyReview(ctx context.Context, snapshot domain.RunSnapshot, review domain.ReviewDecision) (domain.RunSnapshot, error) {
	command := domain.ApplyReviewCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, ReviewDecisionID: review.ID, StageName: review.StageName, StageInputDigest: review.StageInputDigest, EvidenceDigest: review.EvidenceDigest, PolicyDigest: review.PolicyDigest, IdempotencyKey: stableServiceID("review-apply", snapshot.RunID, snapshot.Version), At: s.clock.Now().UTC()}
	return s.reviews.ApplyReview(ctx, command)
}

func (s *LocalRunService) replayInput(snapshot domain.RunSnapshot) domain.Slice1Input {
	input := domain.Slice1Input{Brief: "resume", RequestDigest: snapshot.RequestDigest, ConfigDigest: snapshot.ConfigDigest}
	if s.scenario != "" {
		input.Scenario = s.scenario
	}
	return input
}
func (s *LocalRunService) replayPrepared(snapshot domain.RunSnapshot) domain.Slice1Prepared {
	digest := domain.SumBytes([]byte(fmt.Sprintf("slice1.prepare/v1\x00%s\x00%s\x00%s", snapshot.RequestDigest, snapshot.ConfigDigest, snapshot.WorkflowDigest)))
	return domain.Slice1Prepared{Digest: digest, Summary: "slice1 prepared"}
}
func (s *LocalRunService) replayEvidence(snapshot domain.RunSnapshot) domain.Slice1Evidence {
	prepared := s.replayPrepared(snapshot)
	digest := domain.SumBytes([]byte(fmt.Sprintf("slice1.exercise/v1\x00%s\x00%s", prepared.Digest, snapshot.ConfigDigest)))
	return domain.Slice1Evidence{Digest: digest, PreparedDigest: prepared.Digest}
}
func nextStage(stage domain.StageName) domain.StageName {
	switch stage {
	case "prepare":
		return "exercise"
	case "exercise":
		return "checkpoint"
	case "idea":
		return "statement"
	case "statement":
		return "similarity"
	case "similarity":
		return "slice2_checkpoint"
	default:
		return ""
	}
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
	case domain.Slice1Input:
		return typed.RequestDigest, nil
	case domain.Slice1Prepared:
		return typed.Digest, nil
	case domain.Slice1Evidence:
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
	case domain.Slice1Prepared:
		return typed.Digest, nil
	case domain.Slice1Evidence:
		return typed.Digest, nil
	case domain.Slice1Checkpoint:
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
