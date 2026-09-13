package sqlite

import (
	"context"
	"cpgen/internal/domain"
)

// ReadAttemptSandboxCalls supports settlement of already-admitted local and
// Docker calls. It neither plans operations nor changes dispatch authority.
func (s *Store) ReadAttemptSandboxCalls(ctx context.Context, runID domain.RunID, stage domain.StageName, attempt domain.AttemptID) ([]domain.CallRecord, error) {
	for _, err := range []error{runID.Validate(), stage.Validate(), attempt.Validate()} {
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, callRecordSelect+` WHERE run_id=? AND stage_name=? AND attempt_id=? AND call_kind IN ('SANDBOX_COMPILE','SANDBOX_RUN') ORDER BY opened_at,call_record_id LIMIT 4097`, runID, stage, attempt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.CallRecord
	for rows.Next() {
		call, err := scanCallRecord(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, call)
		if len(result) > 4096 {
			return nil, wrap(ErrConsistency, "sandbox call history exceeds settlement bound", nil)
		}
	}
	return result, rows.Err()
}
