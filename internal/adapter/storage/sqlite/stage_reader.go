package sqlite

import (
	"context"
	"database/sql"

	"cpgen/internal/domain"
)

// CurrentStageAttempt is a read-only recovery seam for the foreground
// coordinator. It does not select arbitrary graph nodes: callers must provide
// the persisted current stage from RunSnapshot.
func (s *Store) CurrentStageAttempt(ctx context.Context, runID domain.RunID, stage domain.StageName) (domain.StageAttempt, error) {
	if err := runID.Validate(); err != nil {
		return domain.StageAttempt{}, err
	}
	if err := stage.Validate(); err != nil {
		return domain.StageAttempt{}, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return domain.StageAttempt{}, err
	}
	defer connection.Close()
	var attempt domain.StageAttempt
	var state, input, started string
	var output, cause, finished sql.NullString
	err = connection.QueryRowContext(ctx, `
		SELECT attempt_id, run_id, stage_name, ordinal, state, input_digest, output_digest, cause, started_at, finished_at
		FROM stage_attempts WHERE run_id = ? AND stage_name = ? ORDER BY ordinal DESC LIMIT 1`, string(runID), string(stage)).Scan(
		&attempt.AttemptID, &attempt.RunID, &attempt.StageName, &attempt.Ordinal, &state, &input, &output, &cause, &started, &finished)
	if err != nil {
		return domain.StageAttempt{}, err
	}
	attempt.State, attempt.InputDigest = domain.StageAttemptState(state), domain.Digest(input)
	if output.Valid {
		value := domain.Digest(output.String)
		attempt.OutputDigest = &value
	}
	if cause.Valid {
		value := domain.ExecutionCause(cause.String)
		attempt.Cause = &value
	}
	if attempt.StartedAt, err = parseTime(started); err != nil {
		return domain.StageAttempt{}, err
	}
	if finished.Valid {
		value, parseErr := parseTime(finished.String)
		if parseErr != nil {
			return domain.StageAttempt{}, parseErr
		}
		attempt.FinishedAt = &value
	}
	if err := attempt.Validate(); err != nil {
		return domain.StageAttempt{}, err
	}
	return attempt, nil
}
