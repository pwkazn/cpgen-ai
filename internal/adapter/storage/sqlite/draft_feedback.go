package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"cpgen/internal/domain"
)

// ReadLatestDraftRetryFeedback returns the most recent automatic retry or
// applied human revision across the run. Older feedback is suppressed when a
// later revision targeted a different producer.
func (s *Store) ReadLatestDraftRetryFeedback(ctx context.Context, runID domain.RunID, target domain.StageName) (*domain.DraftRetryFeedback, error) {
	return s.readDraftRetryFeedback(ctx, runID, target, nil)
}

// ReadDraftRetryFeedbackBefore returns the feedback visible when an attempt
// started. Later retries or reviews must not change reconstruction of an
// already committed request.
func (s *Store) ReadDraftRetryFeedbackBefore(ctx context.Context, runID domain.RunID, target domain.StageName, startedAt time.Time) (*domain.DraftRetryFeedback, error) {
	if startedAt.IsZero() {
		return nil, errors.New("draft feedback lookup requires an attempt start time")
	}
	cutoff := formatTime(startedAt.UTC())
	return s.readDraftRetryFeedback(ctx, runID, target, &cutoff)
}

func (s *Store) readDraftRetryFeedback(ctx context.Context, runID domain.RunID, target domain.StageName, cutoff *string) (*domain.DraftRetryFeedback, error) {
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	if err := target.Validate(); err != nil {
		return nil, err
	}
	var sourceRaw, targetRaw, reason string
	query := `
		SELECT source_stage, target_stage, reason FROM (
			SELECT source_stage, target_stage, reason, created_at AS effective_at, 0 AS priority, ordinal AS sequence, attempt_id AS identity
			FROM content_retries WHERE run_id = ?
			UNION ALL
			SELECT stage_name AS source_stage, revision_target_stage AS target_stage, reason, applied_at AS effective_at, 1 AS priority, run_version AS sequence, review_id AS identity
			FROM review_decisions
			WHERE run_id = ? AND kind = 'REVISE' AND state = 'APPLIED' AND revision_target_stage IS NOT NULL AND applied_at IS NOT NULL
		)`
	args := []any{string(runID), string(runID)}
	if cutoff != nil {
		query += ` WHERE effective_at <= ?`
		args = append(args, *cutoff)
	}
	query += ` ORDER BY effective_at DESC, priority DESC, sequence DESC, identity DESC LIMIT 1`
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&sourceRaw, &targetRaw, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	feedback := domain.DraftRetryFeedback{SourceStage: domain.StageName(sourceRaw), TargetStage: domain.StageName(targetRaw), Reason: reason}
	if err := feedback.Validate(); err != nil {
		return nil, wrap(ErrConsistency, "stored draft retry feedback is invalid", err)
	}
	if feedback.TargetStage != target {
		return nil, nil
	}
	return &feedback, nil
}
