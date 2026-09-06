package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

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

type CurrentStageAttemptReader interface {
	CurrentStageAttempt(context.Context, domain.RunID, domain.StageName) (domain.StageAttempt, error)
}

type RunServiceConfig struct {
	Runtime            port.RuntimeStore
	Reviews            port.ReviewStore
	Locks              *runlock.Manager
	Pipeline           workflow.Slice1Pipeline
	Clock              clock.Clock
	ActiveTimeInterval time.Duration
	Reconciler         SandboxReconciler
}

type LocalRunService struct {
	runtime    port.RuntimeStore
	reviews    port.ReviewStore
	locks      *runlock.Manager
	pipeline   workflow.Slice1Pipeline
	clock      clock.Clock
	active     *ActiveTime
	reconciler SandboxReconciler
	mu         sync.Mutex
	attempts   map[domain.RunID]domain.AttemptID
}

func NewRunService(config RunServiceConfig) (*LocalRunService, error) {
	if config.Runtime == nil || config.Locks == nil || config.Clock == nil {
		return nil, errors.New("run service runtime, locks, and clock are required")
	}
	if err := config.Pipeline.Validate(); err != nil {
		return nil, err
	}
	if config.ActiveTimeInterval <= 0 {
		config.ActiveTimeInterval = time.Second
	}
	active, err := NewActiveTime(config.Runtime, config.Clock, config.ActiveTimeInterval)
	if err != nil {
		return nil, err
	}
	return &LocalRunService{runtime: config.Runtime, reviews: config.Reviews, locks: config.Locks, pipeline: config.Pipeline, clock: config.Clock, active: active, reconciler: config.Reconciler, attempts: make(map[domain.RunID]domain.AttemptID)}, nil
}

func NewLocalRunService(config RunServiceConfig) (*LocalRunService, error) {
	return NewRunService(config)
}

func (s *LocalRunService) Generate(ctx context.Context, request domain.RunRequest) (domain.RunSnapshot, error) {
	if err := request.Validate(); err != nil {
		return domain.RunSnapshot{}, err
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
	seed := int64(0)
	if request.Seed != nil {
		seed = *request.Seed
	} else {
		seed = int64(len(request.Brief))
	}
	now := s.clock.Now().UTC()
	create := domain.CreateRunRequest{RunID: domain.RunID(runIDRaw), SubmittedRequestJSON: requestJSON, SubmittedRequestDigest: requestDigest, EffectiveSeed: seed, RedactedEffectiveConfigJSON: configJSON, RedactedEffectiveConfigDigest: domain.SumBytes(configJSON), WorkflowRevision: workflow.Slice1WorkflowRevision, SchemaVersion: domain.SchemaVersion(request.SchemaVersion), WorkflowDigest: workflowDigest, BudgetLimits: request.BudgetLimits, StageSequence: []domain.StageName{"prepare", "exercise", "checkpoint"}, CreatedAt: now, IdempotencyKey: stableServiceID("create", domain.RunID(runIDRaw), 1)}
	if err := create.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	return s.execute(ctx, create.RunID, &create, domain.Slice1Input{Brief: request.Brief, RequestDigest: requestDigest, ConfigDigest: create.RedactedEffectiveConfigDigest, Scenario: request.Mode})
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
	for snapshot.State == domain.RunCreated || snapshot.State == domain.RunBlocked || snapshot.State == domain.RunRunning {
		if err := ctx.Err(); err != nil {
			return snapshot, err
		}
		var next any
		switch snapshot.CurrentStage {
		case "prepare":
			next = input
		case "exercise":
			next = s.replayPrepared(snapshot)
		case "checkpoint":
			next = s.replayEvidence(snapshot)
		default:
			return snapshot, fmt.Errorf("unsupported slice1 stage %q", snapshot.CurrentStage)
		}
		updated, outcomeErr := s.executeStage(ctx, snapshot, next)
		if outcomeErr != nil {
			return updated, outcomeErr
		}
		snapshot = updated
		if snapshot.State != domain.RunRunning {
			return snapshot, nil
		}
	}
	return snapshot, nil
}

func (s *LocalRunService) executeStage(ctx context.Context, snapshot domain.RunSnapshot, input any) (domain.RunSnapshot, error) {
	wasBlocked := snapshot.State == domain.RunBlocked
	attemptRaw, err := domain.NewID("attempt")
	if err != nil {
		return snapshot, err
	}
	attemptID := domain.AttemptID(attemptRaw)
	inputDigest, err := stageInputDigest(input)
	if err != nil {
		return snapshot, err
	}
	at := s.clock.Now().UTC()
	attempt, err := s.runtime.BeginStage(ctx, domain.BeginStageCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: attemptID, InputDigest: inputDigest, IdempotencyKey: stableServiceID("begin", snapshot.RunID, snapshot.Version), At: at})
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
	if wasBlocked {
		view, viewErr := s.view(snapshot)
		if viewErr != nil {
			return snapshot, viewErr
		}
		ok, revalidateErr := s.pipeline.Revalidate(ctx, view, snapshot.CurrentStage, inputDigest)
		if revalidateErr != nil || !ok {
			result := domain.Blocked[any](domain.BlockedCheckpoint{RunID: snapshot.RunID, StageName: snapshot.CurrentStage, StageInputDigest: inputDigest, DependencyID: "slice1-provider", DependencyDigest: domain.SumBytes([]byte("slice1 dependency")), PolicyDigest: snapshot.ConfigDigest, ErrorDigest: domain.SumBytes([]byte("dependency unavailable")), RetryAfter: s.clock.Now().UTC(), CreatedAt: s.clock.Now().UTC()})
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
		return s.finishOutcome(ctx, snapshot, attempt, inputDigest, result)
	}
	stageCtx, cancel := context.WithCancel(ctx)
	pollDone := make(chan struct{})
	accountDone := make(chan struct{})
	accountExhausted := make(chan bool, 1)
	go s.cancelPoller(stageCtx, snapshot.RunID, cancel, pollDone)
	go s.accountingPoller(stageCtx, snapshot.RunID, cancel, accountDone, accountExhausted)
	result, runErr := s.runTyped(stageCtx, snapshot, input)
	cancel()
	<-pollDone
	<-accountDone
	if runErr != nil && !errors.Is(runErr, context.Canceled) {
		current, getErr := s.runtime.GetRun(ctx, snapshot.RunID)
		if getErr == nil && current.ActiveStartedAt != nil {
			_, _ = s.active.Stop(ctx, current.RunID, current.Version)
		}
		return snapshot, runErr
	}
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
	if result.Cancellation != nil || errors.Is(runErr, context.Canceled) || s.hasCancel(ctx, current.RunID) {
		return s.finishCancellation(ctx, current.RunID)
	}
	if exhausted {
		result = domain.Review[any](domain.ReviewRequest{EvidenceDigest: domain.SumBytes([]byte("active-time exhausted")), PolicyDigest: current.ConfigDigest, Reason: "active_time_exhausted"})
	}
	return s.finishOutcome(ctx, current, attempt, inputDigest, result)
}

func (s *LocalRunService) accountingPoller(ctx context.Context, runID domain.RunID, cancel context.CancelFunc, done chan<- struct{}, exhausted chan<- bool) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(s.active.interval):
			snapshot, err := s.runtime.GetRun(context.Background(), runID)
			if err != nil || snapshot.ActiveStartedAt == nil {
				continue
			}
			result, err := s.active.Heartbeat(context.Background(), runID, snapshot.Version)
			if err != nil {
				continue
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
	view, err := s.view(viewSnapshot)
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

func (s *LocalRunService) view(snapshot domain.RunSnapshot) (domain.RunView, error) {
	return domain.NewRunView(domain.RunViewData{RunID: snapshot.RunID, WorkflowRevision: snapshot.WorkflowRevision, SchemaVersion: snapshot.SchemaVersion, RequestDigest: snapshot.RequestDigest, ConfigDigest: snapshot.ConfigDigest, WorkflowDigest: snapshot.WorkflowDigest, State: snapshot.State, CurrentStage: snapshot.CurrentStage, Version: snapshot.Version, Budget: domain.BudgetSnapshot{Limits: domain.BudgetLimits{}}})
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
	if err := result.Validate(); err != nil {
		return snapshot, err
	}
	at := s.clock.Now().UTC()
	command := domain.FinishStageCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: attempt.AttemptID, IdempotencyKey: stableServiceID("finish", snapshot.RunID, snapshot.Version), At: at}
	switch {
	case result.Value != nil:
		output, err := stageOutputDigest(*result.Value)
		if err != nil {
			return snapshot, err
		}
		command.AttemptState, command.RunState, command.OutputDigest = domain.StageAttemptSucceeded, domain.RunRunning, &output
		next := nextStage(snapshot.CurrentStage)
		if next == "" {
			return snapshot, errors.New("slice1 terminal stage must return review")
		}
		command.NextStage, command.NextInputDigest = next, &output
	case result.Blocked != nil, result.Retryable != nil:
		command.AttemptState, command.RunState = domain.StageAttemptBlocked, domain.RunBlocked
	case result.Review != nil:
		command.AttemptState, command.RunState = domain.StageAttemptNeedsReview, domain.RunNeedsReview
		command.ReviewEvidenceDigest, command.ReviewPolicyDigest = &result.Review.EvidenceDigest, &result.Review.PolicyDigest
	case result.Failure != nil:
		command.AttemptState, command.RunState = domain.StageAttemptFailed, domain.RunFailed
	default:
		return snapshot, errors.New("unsupported stage result")
	}
	return s.runtime.FinishStage(ctx, command)
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
		if attemptID == "" {
			return snapshot, errors.New("running cancellation lacks current attempt")
		}
		if _, err := s.runtime.FinishStage(ctx, domain.FinishStageCommand{RunID: runID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: attemptID, AttemptState: domain.StageAttemptCancelled, RunState: domain.RunCancelled, Cause: causePointer(domain.CauseUserCancel), IdempotencyKey: stableServiceID("cancel-finish", runID, snapshot.Version), At: s.clock.Now().UTC()}); err == nil {
			return s.runtime.GetRun(ctx, runID)
		} else {
			return snapshot, err
		}
	}
	return s.runtime.FinalizeCancel(ctx, domain.FinalizeCancelCommand{RunID: runID, ExpectedRunVersion: snapshot.Version, ControlRequestID: pending.ID, ReconciliationDigest: domain.SumBytes([]byte("slice1 cancellation reconciliation")), IdempotencyKey: stableServiceID("cancel-finalize", runID, snapshot.Version), At: s.clock.Now().UTC()})
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
	if s.reconciler != nil {
		if _, err := s.reconciler.ReconcileRun(ctx, snapshot.RunID); err != nil {
			return snapshot, err
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
		if readerErr == nil {
			attempt = persisted.AttemptID
		}
	}
	if attempt == "" {
		return snapshot, nil
	}
	return s.runtime.InterruptStage(ctx, domain.InterruptStageCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: attempt, Cause: domain.CauseRevisionInvalidated, IdempotencyKey: stableServiceID("interrupt", snapshot.RunID, snapshot.Version), At: s.clock.Now().UTC()})
}

func (s *LocalRunService) applyReview(ctx context.Context, snapshot domain.RunSnapshot, review domain.ReviewDecision) (domain.RunSnapshot, error) {
	command := domain.ApplyReviewCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, ReviewDecisionID: review.ID, StageName: review.StageName, StageInputDigest: review.StageInputDigest, EvidenceDigest: review.EvidenceDigest, PolicyDigest: review.PolicyDigest, IdempotencyKey: stableServiceID("review-apply", snapshot.RunID, snapshot.Version), At: s.clock.Now().UTC()}
	return s.reviews.ApplyReview(ctx, command)
}

func (s *LocalRunService) replayInput(snapshot domain.RunSnapshot) domain.Slice1Input {
	return domain.Slice1Input{Brief: "resume", RequestDigest: snapshot.RequestDigest, ConfigDigest: snapshot.ConfigDigest}
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
	return prefix + "_" + string(digest[len("sha256:"):len("sha256:")+32])
}
