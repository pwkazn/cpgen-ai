package sqlite

import (
	"context"
	"cpgen/internal/domain"
	"database/sql"
	"errors"
)

func (s *Store) LookupWorkbenchResume(ctx context.Context, id domain.RunID, key string, version int64) (bool, error) {
	var previous int64
	err := s.db.QueryRowContext(ctx, `SELECT expected_run_version FROM workbench_resumes WHERE run_id=? AND operation_key=?`, string(id), key).Scan(&previous)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if previous != version {
		return false, wrap(ErrVersionConflict, "operation identity has a different version", nil)
	}
	return true, nil
}
func (s *Store) ClaimWorkbenchResume(ctx context.Context, id domain.RunID, key string, version int64) (bool, error) {
	var replay bool
	err := s.immediate(ctx, func(tx *immediateTx) error {
		var previous int64
		err := tx.QueryRowContext(ctx, `SELECT expected_run_version FROM workbench_resumes WHERE run_id=? AND operation_key=?`, string(id), key).Scan(&previous)
		if err == nil {
			if previous != version {
				return wrap(ErrVersionConflict, "operation identity has a different version", nil)
			}
			replay = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		r, err := readRun(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.Version != version {
			return wrap(ErrVersionConflict, "resume expected version does not match", nil)
		}
		if r.State == domain.RunReady || r.State == domain.RunFailed || r.State == domain.RunCancelled {
			return wrap(ErrInvalidTransition, "terminal run cannot resume", nil)
		}
		if r.State == domain.RunNeedsReview {
			cancel, err := pendingCancelTx(ctx, tx, id)
			if err != nil {
				return err
			}
			review, err := pendingReviewTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if review == nil && cancel == nil {
				return wrap(ErrInvalidTransition, "review decision required", nil)
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO workbench_resumes(run_id,operation_key,expected_run_version) VALUES(?,?,?)`, string(id), key, version)
		return err
	})
	return replay, err
}
func (s *Store) WorkbenchReview(ctx context.Context, id domain.RunID, decision domain.ReviewDecisionID) (domain.ReviewDecision, error) {
	return reviewByIDTx(ctx, s.db, id, decision)
}
func (s *Store) WorkbenchReviewBinding(ctx context.Context, id domain.RunID, stage domain.StageName) (domain.Digest, domain.Digest, domain.Digest, error) {
	return s.ReadReviewBinding(ctx, id, stage)
}

func (s *Store) ReadReviewBinding(ctx context.Context, id domain.RunID, stage domain.StageName) (domain.Digest, domain.Digest, domain.Digest, error) {
	var input, evidence, policy domain.Digest
	err := s.db.QueryRowContext(ctx, `SELECT input_digest,review_evidence_digest,review_policy_digest FROM stage_records WHERE run_id=? AND stage_name=? AND state='NEEDS_REVIEW'`, string(id), string(stage)).Scan(&input, &evidence, &policy)
	return input, evidence, policy, err
}
func (s *Store) WorkbenchCancel(ctx context.Context, id domain.RunID, key string) (domain.CancelRequest, error) {
	var r domain.CancelRequest
	var at string
	err := s.db.QueryRowContext(ctx, `SELECT control_id,expected_run_version,reason,created_at FROM control_requests WHERE run_id=? AND idempotency_key=?`, string(id), key).Scan(&r.ID, &r.ExpectedRunVersion, &r.Reason, &at)
	if err != nil {
		return r, err
	}
	r.RunID = id
	r.IdempotencyKey = key
	r.At, err = parseTime(at)
	return r, err
}
