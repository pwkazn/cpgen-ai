-- Separate machine recovery evidence from human review decisions. Attempts,
-- artifacts and metering remain immutable when the current suffix is reset.
CREATE TABLE content_retries (
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    ordinal INTEGER NOT NULL CHECK (ordinal BETWEEN 1 AND 2),
    attempt_id TEXT NOT NULL UNIQUE REFERENCES stage_attempts(attempt_id) ON DELETE RESTRICT,
    source_stage TEXT NOT NULL,
    target_stage TEXT NOT NULL,
    reason TEXT NOT NULL CHECK (length(reason) BETWEEN 1 AND 512),
    evidence_digest TEXT NOT NULL CHECK (length(evidence_digest) = 71),
    policy_digest TEXT NOT NULL CHECK (length(policy_digest) = 71),
    config_digest TEXT NOT NULL CHECK (length(config_digest) = 71),
    created_at TEXT NOT NULL,
    PRIMARY KEY (run_id, ordinal),
    FOREIGN KEY (run_id, source_stage) REFERENCES stage_records(run_id, stage_name) ON DELETE RESTRICT,
    FOREIGN KEY (run_id, target_stage) REFERENCES stage_records(run_id, stage_name) ON DELETE RESTRICT
) STRICT;
CREATE TRIGGER content_retries_no_update BEFORE UPDATE ON content_retries BEGIN
    SELECT RAISE(ABORT, 'content retry evidence is immutable');
END;

-- Preserve the original package proof checks while admitting the new fixed
-- workflow. M26 bytes and its migration hash remain unchanged.
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
          AND r.workflow_revision IN ('mvp.idea.statement.similarity.solution.data.judge.package.v1', 'mvp.idea.statement.similarity.solution.data.judge.package.v2')
          AND p.state='SUCCEEDED' AND p.output_digest=NEW.archive_digest AND p.input_digest=NEW.quality_digest
          AND q.state='SUCCEEDED' AND q.output_digest=NEW.quality_digest AND q.ordinal+1=p.ordinal
          AND a.run_id=r.run_id AND a.stage_name='package' AND a.attempt_id=NEW.attempt_id
          AND a.kind='NEW_WRITE' AND a.digest=NEW.archive_digest AND a.size<=r.max_package_bytes
          AND a.role='OUTPUT' AND a.logical_path='package/problem.zip' AND a.media_type='application/zip'
          AND json_extract(CAST(a.provenance_json AS TEXT),'$.producer')='mvp-package'
          AND json_extract(CAST(a.provenance_json AS TEXT),'$.input_digest')=NEW.quality_digest
    ) THEN RAISE(ABORT, 'verified package lost its current run evidence') END;
END;
CREATE TRIGGER content_retries_no_delete BEFORE DELETE ON content_retries BEGIN
    SELECT RAISE(ABORT, 'content retry evidence is immutable');
END;
