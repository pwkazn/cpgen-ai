package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
)

// ReadAttemptDependencyCheckpoint derives fresh-dependency admission from the
// immediately preceding attempt, including after a crash before the new call
// opens. The query only admits the exact currently RUNNING attempt. A review
// invalidation or ordinary same-attempt replay does not fabricate a checkpoint.
func (s *Store) ReadAttemptDependencyCheckpoint(ctx context.Context, runID domain.RunID, stage domain.StageName, attempt domain.AttemptID) (*domain.BlockedCheckpoint, error) {
	for _, err := range []error{runID.Validate(), stage.Validate(), attempt.Validate()} {
		if err != nil {
			return nil, err
		}
	}
	var ordinal int64
	var input domain.Digest
	var previousID, state, raw sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT current.ordinal,current.input_digest,previous.attempt_id,previous.state,previous.blocked_binding_json
		FROM stage_attempts current
		JOIN stage_records stage ON stage.run_id=current.run_id AND stage.stage_name=current.stage_name AND stage.current_attempt_id=current.attempt_id
		JOIN runs run ON run.run_id=current.run_id AND run.current_stage=current.stage_name
		LEFT JOIN stage_attempts previous ON previous.run_id=current.run_id AND previous.stage_name=current.stage_name AND previous.ordinal=current.ordinal-1
		WHERE current.run_id=? AND current.stage_name=? AND current.attempt_id=? AND current.state='RUNNING' AND stage.state='RUNNING' AND run.state='RUNNING'`, runID, stage, attempt).Scan(&ordinal, &input, &previousID, &state, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, wrap(ErrNotFound, "dependency checkpoint requires the current running attempt", err)
	}
	if err != nil {
		return nil, err
	}
	if ordinal > 1 && !previousID.Valid {
		return nil, wrap(ErrConsistency, "preceding attempt is missing", nil)
	}
	if !state.Valid || domain.StageAttemptState(state.String) != domain.StageAttemptBlocked {
		return nil, nil
	}
	if !raw.Valid {
		return nil, wrap(ErrConsistency, "blocked predecessor lacks its checkpoint", nil)
	}
	var binding domain.BlockedCheckpoint
	if err := json.Unmarshal([]byte(raw.String), &binding); err != nil {
		return nil, err
	}
	if err := binding.Validate(); err != nil {
		return nil, err
	}
	if binding.RunID != runID || binding.StageName != stage || binding.StageInputDigest != input {
		return nil, wrap(ErrConsistency, "dependency checkpoint input or scope differs", nil)
	}
	return &binding, nil
}
