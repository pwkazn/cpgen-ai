package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"cpgen/internal/domain"
)

func (s *Store) ReadVerifiedPackage(ctx context.Context, runID domain.RunID) (domain.VerifiedPackageRecord, error) {
	var record domain.VerifiedPackageRecord
	if err := runID.Validate(); err != nil {
		return record, err
	}
	err := s.db.QueryRowContext(ctx, `SELECT p.run_id,p.archive_occurrence_id,p.attempt_id,p.package_id,p.manifest_digest,p.quality_digest,p.archive_digest,a.size
		FROM verified_packages p JOIN runs r ON r.run_id=p.run_id AND r.final_package_occurrence_id=p.archive_occurrence_id
		JOIN artifact_occurrences a ON a.occurrence_id=p.archive_occurrence_id AND a.run_id=p.run_id AND a.attempt_id=p.attempt_id AND a.digest=p.archive_digest
		WHERE p.run_id=? AND p.status='VERIFIED' AND r.state='READY'`, string(runID)).Scan(
		&record.RunID, &record.OccurrenceID, &record.AttemptID, &record.Binding.PackageID, &record.Binding.ManifestDigest, &record.Binding.QualityDigest, &record.Binding.Archive.Digest, &record.Binding.Archive.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return record, wrap(ErrNotFound, "run has no READY verified package", err)
	}
	if err != nil {
		return record, err
	}
	return record, record.Validate()
}

func (s *Store) FinalizeVerifiedPackage(ctx context.Context, command domain.FinalizeVerifiedPackageCommand) (domain.RunSnapshot, error) {
	if err := command.Validate(); err != nil {
		return domain.RunSnapshot{}, err
	}
	digest, _, err := digestJSON(command)
	if err != nil {
		return domain.RunSnapshot{}, err
	}
	return s.finishStage(ctx, command.Finish, digest, nil, &command.Package)
}

func validatePackageCompletionTx(ctx context.Context, tx *immediateTx, run domain.RunSnapshot, command domain.FinishStageCommand, binding domain.VerifiedPackageBinding) error {
	if run.WorkflowRevision != "mvp.idea.statement.similarity.solution.data.judge.package.v1" || run.CurrentStage != "package" || run.CurrentStageOrdinal != 12 {
		return wrap(ErrInvalidTransition, "READY requires the fixed MVP package stage", nil)
	}
	var valid int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM stage_records p JOIN stage_records q ON q.run_id=p.run_id AND q.stage_name='quality'
		WHERE p.run_id=? AND p.stage_name='package' AND p.state='RUNNING' AND p.current_attempt_id=?
		AND p.input_digest=? AND q.output_digest=p.input_digest AND q.state='SUCCEEDED' AND q.ordinal+1=p.ordinal AND p.ordinal=12
		AND (SELECT count(*) FROM stage_records prior WHERE prior.run_id=p.run_id AND prior.ordinal<p.ordinal AND prior.state='SUCCEEDED')=11
		AND NOT EXISTS (SELECT 1 FROM stage_records tail WHERE tail.run_id=p.run_id AND tail.ordinal>p.ordinal)`,
		string(run.RunID), string(command.AttemptID), string(binding.QualityDigest)).Scan(&valid); err != nil {
		return err
	}
	if valid != 1 {
		return wrap(ErrConsistency, "package lost its current successful Quality binding", nil)
	}
	var limit int64
	if err := tx.QueryRowContext(ctx, "SELECT max_package_bytes FROM runs WHERE run_id=?", string(run.RunID)).Scan(&limit); err != nil {
		return err
	}
	if binding.Archive.Size > limit {
		return wrap(ErrConsistency, "package exceeds the run package byte limit", nil)
	}
	var unresolved int
	if err := tx.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM call_records WHERE run_id=? AND state<>'TERMINAL') +
		(SELECT count(*) FROM physical_calls WHERE run_id=? AND state IN ('PREPARED','DISPATCHING','SENT','UNKNOWN')) +
		(SELECT count(*) FROM sandbox_resources WHERE run_id=? AND phase NOT IN ('CLEANED','INTERRUPTED'))`,
		string(run.RunID), string(run.RunID), string(run.RunID)).Scan(&unresolved); err != nil {
		return err
	}
	if unresolved != 0 {
		return wrap(ErrConsistency, "READY requires settled calls and cleaned sandbox resources", nil)
	}
	return nil
}

func insertVerifiedPackageTx(ctx context.Context, tx *immediateTx, command domain.FinishStageCommand, binding domain.VerifiedPackageBinding) (domain.ArtifactOccurrenceID, error) {
	var occurrence domain.ArtifactOccurrenceID
	if len(command.Occurrences) != 1 || command.Occurrences[0].NewWrite == nil {
		return occurrence, errors.New("missing owned package publication")
	}
	if err := tx.QueryRowContext(ctx, `SELECT occurrence_id FROM artifact_occurrences
		WHERE writer_token_id=? AND run_id=? AND stage_name='package' AND attempt_id=? AND digest=? AND size=?`,
		string(command.Occurrences[0].NewWrite.WriterTokenID), string(command.RunID), string(command.AttemptID), string(binding.Archive.Digest), binding.Archive.Size).Scan(&occurrence); err != nil {
		return occurrence, err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO verified_packages(archive_occurrence_id, run_id, attempt_id, package_id, archive_digest, manifest_digest, quality_digest, status, created_at) VALUES(?,?,?,?,?,?,?,'VERIFIED',?)`,
		string(occurrence), string(command.RunID), string(command.AttemptID), string(binding.PackageID), string(binding.Archive.Digest), string(binding.ManifestDigest), string(binding.QualityDigest), formatTime(command.At))
	return occurrence, err
}
