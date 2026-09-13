package sqlite

import (
	"context"
	"encoding/json"

	"cpgen/internal/domain"
)

// ReadCommittedLLMArtifact resolves one original private response writer to
// an attached occurrence and terminal local producing call. It grants no write
// and performs no filesystem I/O; callers still verify the complete Blob.
func (s *Store) ReadCommittedLLMArtifact(ctx context.Context, runID domain.RunID, writer domain.ArtifactWriterTokenID) (domain.CacheSource, domain.CacheBlob, error) {
	var source domain.CacheSource
	var item domain.CacheBlob
	if err := runID.Validate(); err != nil {
		return source, item, err
	}
	if err := writer.Validate(); err != nil {
		return source, item, err
	}
	var raw []byte
	var digest domain.Digest
	err := s.db.QueryRowContext(ctx, `SELECT occurrence.run_id,occurrence.current_call_record_id,occurrence.occurrence_id,occurrence.digest,occurrence.size,occurrence.role,occurrence.media_type,occurrence.logical_path,occurrence.provenance_json,occurrence.provenance_digest
		FROM artifact_occurrences occurrence JOIN artifact_writer_tokens token ON token.writer_token_id=occurrence.writer_token_id
		JOIN call_records call ON call.call_record_id=occurrence.current_call_record_id
		WHERE occurrence.run_id=? AND occurrence.writer_token_id=? AND occurrence.kind='NEW_WRITE'
		AND token.state='FINALIZED' AND call.state='TERMINAL' AND call.dispatch_kind='DISPATCHED' AND call.failure_code IS NULL
		AND call.call_kind='LLM_GENERATE' AND call.provider='private-blob' AND call.logical_operation_id LIKE 'llm-response:%'`, runID, writer).Scan(&source.RunID, &source.CallRecordID, &source.OccurrenceID, &item.Blob.Digest, &item.Blob.Size, &item.Role, &item.MediaType, &item.LogicalPath, &raw, &digest)
	if err != nil {
		return source, item, err
	}
	if domain.SumBytes(raw) != digest {
		return source, item, wrap(ErrConsistency, "committed LLM provenance digest differs", nil)
	}
	if err := json.Unmarshal(raw, &item.Provenance); err != nil {
		return source, item, err
	}
	source.Digest, item.SourceOccurrenceID = item.Blob.Digest, source.OccurrenceID
	if err := source.Validate(); err != nil {
		return source, item, err
	}
	return source, item, item.Validate()
}
