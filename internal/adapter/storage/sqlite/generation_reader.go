package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
)

// ReadGenerationSnapshot reconstructs the submitted request and the effective
// seed chosen at creation. It performs one read, preserves submitted bytes and
// starts no stage, provider call or artifact write.
func (s *Store) ReadGenerationSnapshot(ctx context.Context, runID domain.RunID) (domain.GenerationRequestSnapshotV1, error) {
	var empty domain.GenerationRequestSnapshotV1
	if err := runID.Validate(); err != nil {
		return empty, err
	}
	var raw []byte
	var digest domain.Digest
	var schema string
	var seed int64
	err := s.db.QueryRowContext(ctx, `SELECT submitted_request_json,submitted_request_digest,effective_seed,schema_version FROM runs WHERE run_id=?`, runID).Scan(&raw, &digest, &seed, &schema)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, wrap(ErrNotFound, "generation run does not exist", err)
	}
	if err != nil {
		return empty, err
	}
	if schema != domain.RequestSchemaV1 || domain.SumBytes(raw) != digest {
		return empty, wrap(ErrConsistency, "submitted generation request schema or digest differs", nil)
	}
	var request domain.GenerationRequestV1
	if err := json.Unmarshal(raw, &request); err != nil {
		return empty, wrap(ErrConsistency, "persisted request is not an admitted generation request", err)
	}
	canonical, err := request.CanonicalJSON()
	if err != nil || !bytes.Equal(canonical, raw) {
		return empty, wrap(ErrConsistency, "submitted generation request is not canonical", err)
	}
	snapshot, err := domain.NewGenerationRequestSnapshotV1(request, seed)
	if err != nil {
		return empty, wrap(ErrConsistency, "persisted generation seed or snapshot is invalid", err)
	}
	if snapshot.RequestDigest != digest {
		return empty, wrap(ErrConsistency, "generation snapshot changed the submitted request digest", nil)
	}
	return snapshot, nil
}

// ReadAttemptLLMCalls supplies immutable call identities for reconstructing a
// committed source's original/repair relationship. It grants no dispatch and
// does not select a stage to execute.
func (s *Store) ReadAttemptLLMCalls(ctx context.Context, runID domain.RunID, stage domain.StageName, attempt domain.AttemptID) ([]domain.CallRecord, error) {
	for _, err := range []error{runID.Validate(), stage.Validate(), attempt.Validate()} {
		if err != nil {
			return nil, err
		}
	}
	rows, err := s.db.QueryContext(ctx, callRecordSelect+` WHERE run_id=? AND stage_name=? AND attempt_id=? AND call_kind='LLM_GENERATE' AND provider<>'private-blob' ORDER BY opened_at,call_record_id LIMIT 65`, runID, stage, attempt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var calls []domain.CallRecord
	for rows.Next() {
		call, err := scanCallRecord(rows)
		if err != nil {
			return nil, err
		}
		calls = append(calls, call)
		if len(calls) > 64 {
			return nil, wrap(ErrConsistency, "stage LLM history exceeds reconstruction bound", nil)
		}
	}
	return calls, rows.Err()
}

func (s *Store) ReadStageInputDigest(ctx context.Context, runID domain.RunID, stage domain.StageName) (domain.Digest, error) {
	if err := runID.Validate(); err != nil {
		return "", err
	}
	if err := stage.Validate(); err != nil {
		return "", err
	}
	var digest domain.Digest
	err := s.db.QueryRowContext(ctx, `SELECT stage.input_digest FROM stage_records stage JOIN runs run ON run.run_id=stage.run_id WHERE stage.run_id=? AND stage.stage_name=? AND stage.workflow_revision=run.workflow_revision AND stage.schema_version=run.schema_version`, runID, stage).Scan(&digest)
	if errors.Is(err, sql.ErrNoRows) {
		return "", wrap(ErrNotFound, "compatible stage input does not exist", err)
	}
	if err != nil {
		return "", err
	}
	return digest, digest.Validate()
}
