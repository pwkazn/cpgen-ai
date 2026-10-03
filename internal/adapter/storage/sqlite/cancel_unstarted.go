package sqlite

import (
	"context"

	"cpgen/internal/domain"
)

// TryFinalizeUnstartedCancel is deliberately narrower than FinalizeCancel:
// no external reconciliation is necessary only when every durable execution
// ledger is empty. Checking and committing under one write transaction also
// prevents a concurrent BeginStage from invalidating that proof.
func (s *Store) TryFinalizeUnstartedCancel(ctx context.Context, command domain.CancelUnstartedCommand) (domain.RunSnapshot, bool, error) {
	if err := command.Validate(); err != nil {
		return domain.RunSnapshot{}, false, err
	}
	commandDigest, _, err := digestJSON(command)
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	var result domain.RunSnapshot
	var handled bool
	err = s.immediate(ctx, func(tx *immediateTx) error {
		replayed, err := replayEvent(ctx, tx, command.RunID, command.IdempotencyKey, commandDigest, &result)
		if err != nil || replayed {
			handled = replayed
			return err
		}
		run, err := readRun(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		result = run
		if run.Version != command.ExpectedRunVersion {
			return wrap(ErrVersionConflict, "unstarted cancellation expected version does not match", nil)
		}
		pending, err := pendingCancelTx(ctx, tx, command.RunID)
		if err != nil {
			return err
		}
		if pending == nil || pending.ID != command.ControlRequestID {
			return wrap(ErrConsistency, "unstarted cancellation does not match the active control request", nil)
		}
		if run.State != domain.RunCreated || run.CurrentStageOrdinal != 1 || run.ActiveStartedAt != nil || run.ActiveElapsed != 0 {
			return nil
		}
		var hasWork bool
		if err := tx.QueryRowContext(ctx, `SELECT
			EXISTS (SELECT 1 FROM stage_records WHERE run_id=?1 AND
				(state <> 'PENDING' OR attempt_count <> 0 OR current_attempt_id IS NOT NULL)) OR
			EXISTS (SELECT 1 FROM stage_attempts WHERE run_id=?1) OR
			EXISTS (SELECT 1 FROM call_records WHERE run_id=?1) OR
			EXISTS (SELECT 1 FROM physical_calls WHERE run_id=?1) OR
			EXISTS (SELECT 1 FROM budget_reservations WHERE run_id=?1) OR
			EXISTS (SELECT 1 FROM budget_accounts WHERE run_id=?1 AND (reserved_value <> 0 OR consumed_value <> 0)) OR
			EXISTS (SELECT 1 FROM artifact_declarations WHERE run_id=?1) OR
			EXISTS (SELECT 1 FROM artifact_writer_tokens WHERE run_id=?1) OR
			EXISTS (SELECT 1 FROM sandbox_executions WHERE run_id=?1) OR
			EXISTS (SELECT 1 FROM mutation_claims WHERE run_id=?1)`, command.RunID).Scan(&hasWork); err != nil {
			return err
		}
		if hasWork {
			return nil
		}
		// Resource, pin, and publication ownership rows reference the checked
		// sandbox executions, declarations, and writer tokens through foreign
		// keys; zero parent rows also proves those cleanup obligations absent.
		result, err = commitCancellationTx(ctx, tx, run, pending, command.IdempotencyKey, commandDigest, command.At)
		handled = err == nil
		return err
	})
	return result, handled && err == nil, err
}
