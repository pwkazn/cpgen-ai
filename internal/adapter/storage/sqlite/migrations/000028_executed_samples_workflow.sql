-- Admit the executed-sample workflow as a new immutable identity. Existing
-- v1/v2 package proof and migration bytes remain unchanged.
DROP TRIGGER verified_packages_binding_insert;
CREATE TRIGGER verified_packages_binding_insert
BEFORE INSERT ON verified_packages
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM runs r
        JOIN stage_records p ON p.run_id=r.run_id AND p.stage_name='package'
        JOIN stage_records q ON q.run_id=r.run_id AND q.stage_name='quality'
        JOIN artifact_occurrences a ON a.occurrence_id=NEW.archive_occurrence_id
        WHERE r.run_id=NEW.run_id AND r.state='RUNNING' AND r.current_stage='package'
          AND r.workflow_revision IN ('mvp.idea.statement.similarity.solution.data.judge.package.v1', 'mvp.idea.statement.similarity.solution.data.judge.package.v2', 'mvp.idea.statement.similarity.solution.data.judge.package.v3')
          AND p.state='SUCCEEDED' AND p.output_digest=NEW.archive_digest AND p.input_digest=NEW.quality_digest
          AND q.state='SUCCEEDED' AND q.output_digest=NEW.quality_digest AND q.ordinal+1=p.ordinal
          AND a.run_id=r.run_id AND a.stage_name='package' AND a.attempt_id=NEW.attempt_id
          AND a.kind='NEW_WRITE' AND a.digest=NEW.archive_digest AND a.size<=r.max_package_bytes
          AND a.role='OUTPUT' AND a.logical_path='package/problem.zip' AND a.media_type='application/zip'
          AND json_extract(CAST(a.provenance_json AS TEXT),'$.producer')='mvp-package'
          AND json_extract(CAST(a.provenance_json AS TEXT),'$.input_digest')=NEW.quality_digest
    ) THEN RAISE(ABORT, 'verified package lost its current run evidence') END;
END;
