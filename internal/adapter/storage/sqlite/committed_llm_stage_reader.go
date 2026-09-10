package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// ReadCommittedLLMStage reads only the currently successful stage projection.
// A historical SUCCEEDED attempt alone is insufficient after invalidation.
func (s *Store) ReadCommittedLLMStage(ctx context.Context, runID domain.RunID, stage domain.StageName) (port.CommittedLLMStage, error) {
	return s.readCommittedPrivateStage(ctx, runID, stage, domain.CallLLMGenerate)
}

// ReadCommittedSimilarityStage applies the same current-attempt and occurrence
// proof to the closed Similarity receipt family, without admitting LLM bytes.
func (s *Store) ReadCommittedSimilarityStage(ctx context.Context, runID domain.RunID, stage domain.StageName) (port.CommittedPrivateStage, error) {
	return s.readCommittedPrivateStage(ctx, runID, stage, domain.CallSimilaritySearch)
}

// ReadCommittedSandboxStage returns immutable local publication references
// from the current successful attempt. It grants no writer or Docker authority.
func (s *Store) ReadCommittedSandboxStage(ctx context.Context, runID domain.RunID, stage domain.StageName) (port.CommittedPrivateStage, error) {
	return s.readCommittedPrivateStage(ctx, runID, stage, domain.CallSandboxCompile)
}

func (s *Store) readCommittedPrivateStage(ctx context.Context, runID domain.RunID, stage domain.StageName, kind domain.CallKind) (port.CommittedPrivateStage, error) {
	var result port.CommittedPrivateStage
	if err := runID.Validate(); err != nil {
		return result, err
	}
	if err := stage.Validate(); err != nil {
		return result, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	var state string
	var output, current sql.NullString
	var input domain.Digest
	var attemptCount int
	var compatible bool
	err = tx.QueryRowContext(ctx, `SELECT stage.state,stage.version,stage.input_digest,stage.output_digest,stage.current_attempt_id,stage.attempt_count,
		stage.workflow_revision=run.workflow_revision AND stage.schema_version=run.schema_version
		FROM stage_records stage JOIN runs run ON run.run_id=stage.run_id WHERE stage.run_id=? AND stage.stage_name=?`, runID, stage).Scan(&state, &result.StageVersion, &input, &output, &current, &attemptCount, &compatible)
	if errors.Is(err, sql.ErrNoRows) {
		return result, wrap(ErrNotFound, "committed stage does not exist", err)
	}
	if err != nil {
		return result, err
	}
	if state != string(domain.StageSucceeded) || !output.Valid || current.Valid || !compatible || result.StageVersion <= 0 {
		return result, wrap(ErrConsistency, "stage has no compatible current successful output", nil)
	}
	result.Attempt, err = readLatestStageAttempt(ctx, tx, runID, stage)
	if err != nil {
		return result, err
	}
	attempt := result.Attempt
	if attempt.State != domain.StageAttemptSucceeded || attempt.Ordinal != attemptCount || attempt.InputDigest != input || attempt.OutputDigest == nil || string(*attempt.OutputDigest) != output.String {
		return result, wrap(ErrConsistency, "stage output differs from its latest successful attempt", nil)
	}
	rows, err := tx.QueryContext(ctx, `SELECT occurrence_id FROM artifact_occurrences WHERE run_id=? AND stage_name=? AND attempt_id=? ORDER BY occurrence_id`, runID, stage, attempt.AttemptID)
	if err != nil {
		return result, err
	}
	var ids []domain.ArtifactOccurrenceID
	limit := 64
	if kind == domain.CallSandboxCompile {
		limit = 512
	}
	for rows.Next() {
		var id domain.ArtifactOccurrenceID
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return result, err
		}
		ids = append(ids, id)
		if len(ids) > limit {
			_ = rows.Close()
			return result, wrap(ErrConsistency, "committed stage exceeds private receipt bound", nil)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return result, err
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	if len(ids) == 0 {
		return result, wrap(ErrConsistency, "committed stage has no private receipt occurrence", nil)
	}
	for _, id := range ids {
		artifact, err := readCommittedStagePrivateArtifact(ctx, tx, attempt, id, kind)
		if err != nil {
			return result, err
		}
		result.Artifacts = append(result.Artifacts, artifact)
	}
	return result, tx.Commit()
}

func readCommittedStagePrivateArtifact(ctx context.Context, tx *sql.Tx, attempt domain.StageAttempt, id domain.ArtifactOccurrenceID, kind domain.CallKind) (port.CommittedPrivateStageArtifact, error) {
	var result port.CommittedPrivateStageArtifact
	var mediaType, pathPrefix string
	switch kind {
	case domain.CallLLMGenerate:
		mediaType, pathPrefix = "application/vnd.cpgen.llm-response+json", "private/llm/"
	case domain.CallSimilaritySearch:
		mediaType, pathPrefix = "application/vnd.cpgen.similarity-response+json", "private/similarity/"
	case domain.CallSandboxCompile:
		// Sandbox stages retain multiple bounded local artifact roles.
	default:
		return result, errors.New("unsupported private receipt family")
	}
	var sourceStage domain.StageName
	var sourceAttempt domain.AttemptID
	var sourceKind string
	var raw []byte
	var provenanceDigest domain.Digest
	var sameContent, retained, reuseMatches bool
	err := tx.QueryRowContext(ctx, `SELECT current.kind,current.occurrence_id,current.current_call_record_id,
		source.run_id,source.stage_name,source.attempt_id,source.kind,source.current_call_record_id,source.occurrence_id,source.digest,source.size,source.role,source.media_type,source.logical_path,source.provenance_json,source.provenance_digest,
		current.digest=source.digest AND current.size=source.size AND current.role=source.role AND current.media_type=source.media_type AND current.logical_path=source.logical_path AND current.provenance_json=source.provenance_json AND current.provenance_digest=source.provenance_digest,
		EXISTS(SELECT 1 FROM artifact_writer_tokens token JOIN artifact_declarations decl ON decl.declaration_id=token.declaration_id JOIN blobs blob ON blob.digest=token.final_digest
		 WHERE token.writer_token_id=source.writer_token_id AND token.state='FINALIZED' AND token.run_id=source.run_id AND token.final_digest=source.digest AND token.final_size=source.size AND blob.state='READY'
		 AND decl.run_id=source.run_id AND decl.stage_name=source.stage_name AND decl.attempt_id=source.attempt_id AND decl.call_record_id=source.current_call_record_id AND decl.role=source.role AND decl.media_type=source.media_type AND decl.logical_path=source.logical_path AND decl.provenance_json=source.provenance_json),
		current.kind='NEW_WRITE' OR EXISTS(SELECT 1 FROM cache_reuse_records reuse WHERE reuse.cache_reuse_record_id=current.cache_reuse_record_id AND reuse.run_id=current.run_id AND reuse.stage_name=current.stage_name AND reuse.attempt_id=current.attempt_id AND reuse.current_call_record_id=current.current_call_record_id AND reuse.source_call_record_id=source.current_call_record_id AND reuse.source_occurrence_id=source.occurrence_id)
		FROM artifact_occurrences current JOIN artifact_occurrences source ON source.occurrence_id=CASE WHEN current.kind='NEW_WRITE' THEN current.occurrence_id ELSE current.source_occurrence_id END
		WHERE current.occurrence_id=? AND current.run_id=? AND current.stage_name=? AND current.attempt_id=?`, id, attempt.RunID, attempt.StageName, attempt.AttemptID).Scan(
		&result.Kind, &result.OccurrenceID, &result.CurrentCallRecordID, &result.Source.RunID, &sourceStage, &sourceAttempt, &sourceKind, &result.Source.CallRecordID, &result.Source.OccurrenceID,
		&result.Blob.Blob.Digest, &result.Blob.Blob.Size, &result.Blob.Role, &result.Blob.MediaType, &result.Blob.LogicalPath, &raw, &provenanceDigest, &sameContent, &retained, &reuseMatches)
	if err != nil {
		return result, err
	}
	if !result.Kind.Valid() || result.Source.RunID != attempt.RunID || sourceKind != string(domain.PendingOccurrenceNewWrite) || !sameContent || !retained || !reuseMatches || domain.SumBytes(raw) != provenanceDigest {
		return result, wrap(ErrConsistency, "committed private receipt provenance or retention differs", nil)
	}
	if err := json.Unmarshal(raw, &result.Blob.Provenance); err != nil {
		return result, err
	}
	result.Source.Digest, result.Blob.SourceOccurrenceID = result.Blob.Blob.Digest, result.Source.OccurrenceID
	if err := result.Source.Validate(); err != nil {
		return result, err
	}
	if err := result.Blob.Validate(); err != nil {
		return result, err
	}
	if kind != domain.CallSandboxCompile && (result.Blob.Role != domain.ArtifactEvidence || result.Blob.MediaType != mediaType || !strings.HasPrefix(string(result.Blob.LogicalPath), pathPrefix)) {
		return result, wrap(ErrConsistency, "stage occurrence differs from its private receipt family", nil)
	}
	local, err := readCallRecord(ctx, tx, result.Source.CallRecordID)
	if err != nil {
		return result, err
	}
	if kind == domain.CallSandboxCompile {
		if result.Kind != domain.PendingOccurrenceNewWrite || result.OccurrenceID != result.Source.OccurrenceID || result.CurrentCallRecordID != local.ID || sourceStage != attempt.StageName || sourceAttempt != attempt.AttemptID || local.RunID != attempt.RunID || local.StageName != attempt.StageName || local.AttemptID != attempt.AttemptID || (local.Kind != domain.CallSandboxCompile && local.Kind != domain.CallSandboxRun) || local.Provider != "blob" || local.State != domain.CallRecordTerminal || local.DispatchKind == nil || *local.DispatchKind != domain.DispatchDispatched || local.Failure != nil || local.ResultAttemptCallID == nil {
			return result, wrap(ErrConsistency, "sandbox artifact lacks a successful local producer in the current attempt", nil)
		}
		result.ProviderCallRecordID = local.ID
		return result, nil
	}
	parent, ok := privateResponseParent(local)
	result.ProviderCallRecordID = parent
	if !ok || local.RunID != attempt.RunID || local.StageName != sourceStage || local.AttemptID != sourceAttempt || local.Kind != kind || local.State != domain.CallRecordTerminal || local.DispatchKind == nil || *local.DispatchKind != domain.DispatchDispatched || local.Failure != nil {
		return result, wrap(ErrConsistency, "private receipt producer is not a successful terminal local call", nil)
	}
	provider, err := readCallRecord(ctx, tx, result.ProviderCallRecordID)
	if err != nil {
		return result, err
	}
	if provider.RunID != local.RunID || provider.StageName != local.StageName || provider.AttemptID != local.AttemptID || provider.Kind != kind || provider.Provider == "private-blob" || provider.State != domain.CallRecordTerminal {
		return result, wrap(ErrConsistency, "private receipt provider call is not terminal in its producing attempt", nil)
	}
	current, err := readCallRecord(ctx, tx, result.CurrentCallRecordID)
	if err != nil {
		return result, err
	}
	if current.RunID != attempt.RunID || current.StageName != attempt.StageName || current.AttemptID != attempt.AttemptID || current.State != domain.CallRecordTerminal || current.Failure != nil {
		return result, wrap(ErrConsistency, "current receipt call differs from the committed stage attempt", nil)
	}
	if result.Kind == domain.PendingOccurrenceNewWrite {
		if result.OccurrenceID != result.Source.OccurrenceID || current.ID != local.ID {
			return result, wrap(ErrConsistency, "new receipt occurrence changed its original producer", nil)
		}
	} else if current.Kind != domain.CallCacheReuse || current.DispatchKind == nil || *current.DispatchKind != domain.DispatchCacheHit || current.CacheSourceCallRecordID == nil || *current.CacheSourceCallRecordID != local.ID || current.CacheHitCallRecordID == nil || *current.CacheHitCallRecordID != current.ID {
		return result, wrap(ErrConsistency, "cache receipt occurrence differs from its durable hit trace", nil)
	}
	return result, nil
}
