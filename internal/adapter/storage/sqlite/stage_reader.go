package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
)

// StageSequence reads the immutable compiled selectors for compatibility
// checking before the coordinator starts or recovers any stage work.
func (s *Store) StageSequence(ctx context.Context, runID domain.RunID) ([]domain.StageName, error) {
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return nil, err
	}
	defer connection.Close()
	rows, err := connection.QueryContext(ctx, `SELECT s.stage_name, s.ordinal,
		s.workflow_revision = r.workflow_revision AND s.schema_version = r.schema_version
		FROM stage_records s JOIN runs r ON r.run_id = s.run_id
		WHERE s.run_id = ? ORDER BY s.ordinal`, string(runID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sequence []domain.StageName
	for rows.Next() {
		var name domain.StageName
		var ordinal int
		var matches bool
		if err := rows.Scan(&name, &ordinal, &matches); err != nil {
			return nil, err
		}
		if err := name.Validate(); err != nil {
			return nil, err
		}
		if ordinal != len(sequence)+1 || !matches {
			return nil, errors.New("persisted stage sequence or revision binding is inconsistent")
		}
		sequence = append(sequence, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(sequence) == 0 {
		return nil, sql.ErrNoRows
	}
	return sequence, nil
}

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
	return readLatestStageAttempt(ctx, connection, runID, stage)
}

func readLatestStageAttempt(ctx context.Context, queryer rowQuerier, runID domain.RunID, stage domain.StageName) (domain.StageAttempt, error) {
	return scanStageAttempt(queryer.QueryRowContext(ctx, `
		SELECT attempt_id, run_id, stage_name, ordinal, state, input_digest, output_digest, cause, blocked_binding_json, started_at, finished_at
		FROM stage_attempts WHERE run_id = ? AND stage_name = ? ORDER BY ordinal DESC LIMIT 1`, string(runID), string(stage)))
}

// ReadStageAttempt reads the exact persisted attempt, including historical
// attempts superseded by content retries or reviews. It grants no write access.
func (s *Store) ReadStageAttempt(ctx context.Context, runID domain.RunID, stage domain.StageName, attemptID domain.AttemptID) (domain.StageAttempt, error) {
	for _, err := range []error{runID.Validate(), stage.Validate(), attemptID.Validate()} {
		if err != nil {
			return domain.StageAttempt{}, err
		}
	}
	connection, err := s.connection(ctx)
	if err != nil {
		return domain.StageAttempt{}, err
	}
	defer connection.Close()
	return scanStageAttempt(connection.QueryRowContext(ctx, `
		SELECT attempt_id, run_id, stage_name, ordinal, state, input_digest, output_digest, cause, blocked_binding_json, started_at, finished_at
		FROM stage_attempts WHERE run_id = ? AND stage_name = ? AND attempt_id = ?`, string(runID), string(stage), string(attemptID)))
}

func scanStageAttempt(row *sql.Row) (domain.StageAttempt, error) {
	var attempt domain.StageAttempt
	var state, input, started string
	var output, cause, blockedBinding, finished sql.NullString
	err := row.Scan(
		&attempt.AttemptID, &attempt.RunID, &attempt.StageName, &attempt.Ordinal, &state, &input, &output, &cause, &blockedBinding, &started, &finished)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.StageAttempt{}, wrap(ErrNotFound, "stage attempt not found", err)
	}
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
	if blockedBinding.Valid {
		var binding domain.BlockedCheckpoint
		if err := json.Unmarshal([]byte(blockedBinding.String), &binding); err != nil {
			return domain.StageAttempt{}, err
		}
		attempt.BlockedBinding = &binding
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
