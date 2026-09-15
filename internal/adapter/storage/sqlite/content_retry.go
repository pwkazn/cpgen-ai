package sqlite

import (
	"context"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

var _ port.ContentRetryStore = (*Store)(nil)

func (s *Store) FinishContentRetry(ctx context.Context, command domain.FinishContentRetryCommand) (domain.RunSnapshot, error) {
	if err := command.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	return s.finishStage(ctx, command.Finish, digest, nil, nil, &command)
}

// Called in the same transaction as attempt completion. Crash recovery cannot
// lose a retry allowance between recording failure and invalidating outputs.
func scheduleContentRetryTx(ctx context.Context, tx *immediateTx, run domain.RunSnapshot, command domain.FinishContentRetryCommand) (domain.StageName, int, error) {
	finish := command.Finish
	target := workflow.ContentRetryTarget(run.WorkflowRevision, finish.StageName, command.Reason)
	if target == "" {
		return "", 0, wrap(ErrConsistency, "failure has no compiled content retry route", nil)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM content_retries WHERE run_id=?`, run.RunID).Scan(&count); err != nil {
		return "", 0, err
	}
	if count >= workflow.ContentRetryLimit {
		return "", 0, nil
	}
	var ordinal int
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT ordinal,state FROM stage_records WHERE run_id=? AND stage_name=?`, run.RunID, target).Scan(&ordinal, &state); err != nil {
		return "", 0, err
	}
	if ordinal > run.CurrentStageOrdinal || (target != finish.StageName && state != string(domain.StageSucceeded)) {
		return "", 0, wrap(ErrConsistency, "content retry target is not a current producer", nil)
	}
	if pending, err := pendingReviewTx(ctx, tx, run.RunID); err != nil {
		return "", 0, err
	} else if pending != nil {
		return "", 0, wrap(ErrReviewPending, "content retry cannot supersede human review", nil)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO content_retries(run_id,ordinal,attempt_id,source_stage,target_stage,reason,evidence_digest,policy_digest,config_digest,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		run.RunID, count+1, finish.AttemptID, finish.StageName, target, command.Reason, *finish.ReviewEvidenceDigest, *finish.ReviewPolicyDigest, run.ConfigDigest, formatTime(finish.At)); err != nil {
		return "", 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE stage_records SET state='PENDING',version=version+1,output_digest=NULL,current_attempt_id=NULL,review_evidence_digest=NULL,review_policy_digest=NULL,review_waivable=NULL,updated_at=? WHERE run_id=? AND ordinal>=?`, formatTime(finish.At), run.RunID, ordinal); err != nil {
		return "", 0, err
	}
	return target, ordinal, nil
}
