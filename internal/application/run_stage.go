package application

import (
	"context"
	"errors"
	"fmt"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

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
	s.attemptMu.Lock()
	s.attempts[snapshot.RunID] = attempt.AttemptID
	s.attemptMu.Unlock()
	snapshot, err = s.runtime.GetRun(ctx, snapshot.RunID)
	if err != nil {
		return snapshot, err
	}
	if wasBlocked && !s.graph.definition.UsesGeneration() {
		view, viewErr := s.view(ctx, snapshot, attempt.AttemptID)
		if viewErr != nil {
			return snapshot, viewErr
		}
		ok, revalidateErr := s.stages.revalidate(ctx, view, snapshot.CurrentStage, *blockedBinding)
		if revalidateErr != nil || !ok {
			result := domain.Blocked[any](*blockedBinding)
			if revalidateErr != nil {
				return s.finishOutcome(ctx, snapshot, attempt, inputDigest, result)
			}
			return s.finishOutcome(ctx, snapshot, attempt, inputDigest, result)
		}
	}
	active, activeErr := s.control.active.Start(ctx, snapshot.RunID, snapshot.Version)
	if activeErr != nil {
		return snapshot, activeErr
	}
	snapshot, err = s.runtime.GetRun(ctx, snapshot.RunID)
	if err != nil {
		return snapshot, err
	}
	if active.Exhausted {
		result := domain.Review[any](domain.ReviewRequest{EvidenceDigest: domain.SumBytes([]byte("active-time exhausted")), PolicyDigest: snapshot.ConfigDigest, Reason: "active_time_exhausted"})
		if _, stopErr := s.control.active.Stop(ctx, snapshot.RunID, snapshot.Version); stopErr != nil {
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
	watch := s.control.watch(ctx, snapshot.RunID)
	view, viewErr := s.view(watch.ctx, snapshot, attempt.AttemptID)
	var execution stageExecution
	runErr := viewErr
	if viewErr == nil {
		execution, runErr = s.stages.runStage(watch.ctx, view, input)
	}
	result := execution.result
	watch.join()
	current, getErr := s.runtime.GetRun(ctx, snapshot.RunID)
	if getErr != nil {
		return snapshot, getErr
	}
	exhausted := false
	select {
	case exhausted = <-watch.exhausted:
	default:
	}
	if current.ActiveStartedAt != nil {
		stopResult, stopErr := s.control.active.Stop(ctx, current.RunID, current.Version)
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
	case accountingErr = <-watch.errors:
	default:
	}
	if accountingErr != nil {
		return current, accountingErr
	}
	interruptedCause := domain.ExecutionCause("")
	if interrupted, ok := context.Cause(watch.ctx).(domain.ExecutionInterrupted); ok {
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
			if !s.graph.definition.ProducesPackage() || snapshot.CurrentStage != "package" || inputDigest != binding.QualityDigest {
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
		var next domain.StageName
		if snapshot.CurrentStageOrdinal > 0 && snapshot.CurrentStageOrdinal < len(s.graph.stages) {
			next = s.graph.stages[snapshot.CurrentStageOrdinal]
		}
		if next == "" {
			return snapshot, errors.New("final stage must return an explicit pause or verified package")
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
	if result.Value != nil && s.graph.definition.UsesGeneration() {
		if err := s.stages.bindCommit(ctx, snapshot, *result.Value, &command); err != nil {
			return snapshot, err
		}
	}
	if result.Value != nil {
		command.Occurrences = execution.occurrences
	}
	var finished domain.RunSnapshot
	var err error
	if result.Review != nil && workflow.ContentRetryTarget(snapshot.WorkflowRevision, snapshot.CurrentStage, result.Review.Reason) != "" {
		store, ok := s.runtime.(port.ContentRetryStore)
		if !ok {
			return snapshot, errors.New("content retry workflow requires atomic retry storage")
		}
		finished, err = store.FinishContentRetry(ctx, domain.FinishContentRetryCommand{Finish: command, Reason: result.Review.Reason})
	} else {
		finished, err = s.runtime.FinishStage(ctx, command)
	}
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

func (s *LocalRunService) hasCancel(ctx context.Context, runID domain.RunID) bool {
	pending, err := s.runtime.PendingCancel(ctx, runID)
	return err == nil && pending != nil
}

func (s *LocalRunService) beginOrResumeStage(ctx context.Context, snapshot domain.RunSnapshot, input domain.Digest) (domain.StageAttempt, error) {
	if s.graph.definition.PreservesAttempt() && snapshot.State == domain.RunRunning {
		reader, ok := s.runtime.(CurrentStageAttemptReader)
		if !ok {
			return domain.StageAttempt{}, errors.New("durable workflow requires a current attempt reader")
		}
		attempt, err := reader.CurrentStageAttempt(ctx, snapshot.RunID, snapshot.CurrentStage)
		if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
			return attempt, err
		}
		if err == nil && attempt.State == domain.StageAttemptRunning {
			if attempt.InputDigest != input {
				return domain.StageAttempt{}, errors.New("interrupted attempt input differs from verified content")
			}
			return attempt, nil
		}
	}
	id, err := domain.NewID("attempt")
	if err != nil {
		return domain.StageAttempt{}, err
	}
	return s.runtime.BeginStage(ctx, domain.BeginStageCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: domain.AttemptID(id), InputDigest: input, IdempotencyKey: stableServiceID("begin", snapshot.RunID, snapshot.Version), At: s.clock.Now().UTC()})
}
