package application

import (
	"context"
	"errors"
	"fmt"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func (s *LocalRunService) recover(ctx context.Context, snapshot domain.RunSnapshot, attempt domain.AttemptID, preserveAttempt bool) (domain.RunSnapshot, error) {
	if s.recovery != nil {
		if err := s.recovery.RecoverRun(ctx, snapshot.RunID); err != nil {
			return snapshot, err
		}
	}
	if s.reconciler != nil {
		report, err := s.reconciler.ReconcileRun(ctx, snapshot.RunID)
		if err != nil {
			if cleanupEvidenceMatchesRun(snapshot.RunID, report) {
				return snapshot, fmt.Errorf("%w: %v", sandboxexec.ErrCleanupPending, err)
			}
			return snapshot, err
		}
		if cleanupEvidenceMatchesRun(snapshot.RunID, report) {
			return snapshot, fmt.Errorf("%w: sandbox recovery cleanup is not settled", sandboxexec.ErrCleanupPending)
		}
		if !report.Completed {
			return snapshot, errors.New("sandbox recovery reconciliation incomplete without cleanup evidence")
		}
	}
	if snapshot.ActiveStartedAt != nil {
		if _, err := s.control.active.Recover(ctx, snapshot.RunID, snapshot.Version); err != nil {
			return snapshot, err
		}
		updated, getErr := s.runtime.GetRun(ctx, snapshot.RunID)
		if getErr != nil {
			return snapshot, getErr
		}
		snapshot = updated
		if snapshot.ActiveStartedAt != nil && snapshot.LastAccountingHeartbeatAt != nil {
			bound := snapshot.LastAccountingHeartbeatAt.Add(s.control.active.interval)
			if _, stopErr := s.control.active.stopAt(ctx, snapshot.RunID, snapshot.Version, bound); stopErr != nil {
				return snapshot, stopErr
			}
			snapshot, getErr = s.runtime.GetRun(ctx, snapshot.RunID)
			if getErr != nil {
				return snapshot, getErr
			}
		}
	}
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
	if attempt == "" {
		return snapshot, nil
	}
	if preserveAttempt {
		// Keep the original attempt for all dispatched/uncertain work. Only the
		// known pre-execution failure may retire its fully settled no-send calls
		// and begin an ordinary, budgeted verification attempt on manual resume.
		if store, ok := s.runtime.(port.UnsentSandboxRecoveryStore); ok && snapshot.CurrentStage == "solution_verify" {
			return store.InterruptUnsentSandboxStage(ctx, domain.InterruptStageCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: attempt, Cause: domain.CauseRevisionInvalidated, IdempotencyKey: stableServiceID("interrupt-unsent-sandbox", snapshot.RunID, snapshot.Version), At: s.clock.Now().UTC()})
		}
		return snapshot, nil
	}
	return s.runtime.InterruptStage(ctx, domain.InterruptStageCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: attempt, Cause: domain.CauseRevisionInvalidated, IdempotencyKey: stableServiceID("interrupt", snapshot.RunID, snapshot.Version), At: s.clock.Now().UTC()})
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
		if _, err := s.control.active.Stop(ctx, runID, snapshot.Version); err != nil {
			return snapshot, err
		}
		snapshot, err = s.runtime.GetRun(ctx, runID)
		if err != nil {
			return snapshot, err
		}
	}
	if snapshot.State == domain.RunRunning {
		attemptID := s.attemptID(runID)
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

func (s *LocalRunService) reconcileForTerminal(ctx context.Context, runID domain.RunID) error {
	if s.stages != nil && s.graph.definition.UsesGeneration() {
		snapshot, err := s.runtime.GetRun(ctx, runID)
		if err != nil {
			return err
		}
		if err := s.validateGraphPersistence(ctx, snapshot); err != nil {
			return err
		}
		if err := s.stages.reconcileStage(ctx, snapshot, s.reconciler); err != nil {
			return err
		}
	}
	if s.reconciler == nil {
		return nil
	}
	report, err := s.reconciler.ReconcileRun(ctx, runID)
	if err != nil {
		if cleanupEvidenceMatchesRun(runID, report) {
			return fmt.Errorf("%w: %v", sandboxexec.ErrCleanupPending, err)
		}
		return fmt.Errorf("reconcile sandbox resources: %w", err)
	}
	if cleanupEvidenceMatchesRun(runID, report) {
		return fmt.Errorf("%w: sandbox cleanup is not settled", sandboxexec.ErrCleanupPending)
	}
	if !report.Completed {
		return errors.New("reconcile sandbox resources: incomplete report without cleanup evidence")
	}
	return nil
}

func (s *LocalRunService) applyPendingReview(ctx context.Context, snapshot domain.RunSnapshot) (domain.RunSnapshot, error) {
	if s.reviews == nil {
		return snapshot, nil
	}
	review, err := s.reviews.PendingReview(ctx, snapshot.RunID)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	if review == nil {
		return snapshot, nil
	}
	command := domain.ApplyReviewCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, ReviewDecisionID: review.ID, StageName: review.StageName, StageInputDigest: review.StageInputDigest, EvidenceDigest: review.EvidenceDigest, PolicyDigest: review.PolicyDigest, IdempotencyKey: stableServiceID("review-apply", snapshot.RunID, snapshot.Version), At: s.clock.Now().UTC()}
	return s.reviews.ApplyReview(ctx, command)
}
