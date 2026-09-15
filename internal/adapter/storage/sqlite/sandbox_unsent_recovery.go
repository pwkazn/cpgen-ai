package sqlite

import (
	"context"

	"cpgen/internal/domain"
)

// This deliberately covers the first solution verification create failing
// before PrepareExecution. Once any execution exists, even CLEANED, only the
// existing receipt recovery may continue it. Source publication is local and
// may already have completed; its evidence and byte charges are retained.
func unsentSandboxAttempt(ctx context.Context, tx *immediateTx, command domain.InterruptStageCommand) (bool, error) {
	if command.StageName != "solution_verify" {
		return false, nil
	}
	var eligible bool
	err := tx.QueryRowContext(ctx, `
		WITH calls AS (
			SELECT * FROM call_records WHERE run_id=? AND stage_name=? AND attempt_id=?
		), physical AS (
			SELECT p.* FROM physical_calls p JOIN calls c ON c.call_record_id=p.call_record_id
		), reservations AS (
			SELECT r.* FROM budget_reservations r WHERE run_id=? AND stage_name=? AND attempt_id=?
		)
		SELECT
			NOT EXISTS (SELECT 1 FROM sandbox_executions WHERE run_id=? AND stage_name=? AND attempt_id=?)
			AND EXISTS (SELECT 1 FROM calls WHERE provider='docker')
			AND NOT EXISTS (SELECT 1 FROM calls c WHERE
				c.state!='TERMINAL' OR c.call_kind NOT IN ('SANDBOX_COMPILE','SANDBOX_RUN')
				OR c.provider NOT IN ('docker','blob')
				OR (SELECT count(*) FROM physical p WHERE p.call_record_id=c.call_record_id)!=1
				OR (c.provider='docker' AND (c.dispatch_kind!='NO_DISPATCH' OR c.result_attempt_call_id IS NOT NULL)))
			AND NOT EXISTS (SELECT 1 FROM physical p JOIN calls c ON c.call_record_id=p.call_record_id WHERE
				p.provider!=c.provider
				OR (c.provider='docker' AND (p.physical_kind NOT IN ('DOCKER_CONTAINER_CREATE','DOCKER_VOLUME_CREATE')
					OR p.state!='ABORTED_NO_DISPATCH' OR p.outcome_kind!='NO_SEND'
					OR p.dispatch_started_at IS NOT NULL OR p.sent_at IS NOT NULL))
				OR (c.provider='blob' AND (p.physical_kind!='LOCAL_ARTIFACT_WRITE'
					OR p.state NOT IN ('COMPLETED','ABORTED_NO_DISPATCH'))))
			AND NOT EXISTS (SELECT 1 FROM reservations r WHERE r.state='RESERVED'
				OR (r.call_record_id IN (SELECT call_record_id FROM calls WHERE provider='docker') AND r.settled_value!=0))`,
		command.RunID, command.StageName, command.AttemptID,
		command.RunID, command.StageName, command.AttemptID,
		command.RunID, command.StageName, command.AttemptID).Scan(&eligible)
	return eligible, err
}
