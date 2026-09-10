-- READY is reachable only through an immutable package from this run and its
-- successful current Quality stage. Historical run identities remain intact.
PRAGMA defer_foreign_keys = ON;
CREATE TABLE verified_packages (
    archive_occurrence_id TEXT PRIMARY KEY REFERENCES artifact_occurrences(occurrence_id) ON DELETE RESTRICT,
    run_id TEXT NOT NULL UNIQUE REFERENCES runs(run_id) ON DELETE RESTRICT,
    attempt_id TEXT NOT NULL REFERENCES stage_attempts(attempt_id) ON DELETE RESTRICT,
    package_id TEXT NOT NULL CHECK (length(package_id) = 71),
    archive_digest TEXT NOT NULL CHECK (length(archive_digest) = 71),
    manifest_digest TEXT NOT NULL CHECK (length(manifest_digest) = 71),
    quality_digest TEXT NOT NULL CHECK (length(quality_digest) = 71),
    status TEXT NOT NULL CHECK (status = 'VERIFIED'),
    created_at TEXT NOT NULL,
    UNIQUE (run_id, archive_occurrence_id)
) STRICT;
CREATE TABLE runs_v26 (
    run_id TEXT PRIMARY KEY,
    submitted_request_json BLOB NOT NULL CHECK (length(submitted_request_json) > 0),
    submitted_request_digest TEXT NOT NULL CHECK (length(submitted_request_digest) = 71),
    effective_seed INTEGER NOT NULL,
    redacted_effective_config_json BLOB NOT NULL CHECK (length(redacted_effective_config_json) > 0),
    redacted_effective_config_digest TEXT NOT NULL CHECK (length(redacted_effective_config_digest) = 71),
    workflow_digest TEXT NOT NULL CHECK (length(workflow_digest) = 71),
    workflow_revision TEXT NOT NULL CHECK (workflow_revision <> ''),
    schema_version TEXT NOT NULL CHECK (schema_version <> ''),
    max_llm_calls INTEGER NOT NULL CHECK (max_llm_calls >= 0),
    max_similarity_calls INTEGER NOT NULL CHECK (max_similarity_calls >= 0),
    max_llm_input_tokens INTEGER NOT NULL CHECK (max_llm_input_tokens >= 0),
    max_llm_output_tokens INTEGER NOT NULL CHECK (max_llm_output_tokens >= 0),
    max_llm_cost_micro_usd INTEGER NOT NULL CHECK (max_llm_cost_micro_usd >= 0),
    max_sandbox_creates INTEGER NOT NULL CHECK (max_sandbox_creates >= 0),
    max_artifact_bytes INTEGER NOT NULL CHECK (max_artifact_bytes >= 0),
    max_package_bytes INTEGER NOT NULL CHECK (max_package_bytes >= 0),
    max_mutations_per_stage INTEGER NOT NULL CHECK (max_mutations_per_stage >= 0),
    max_active_time_ns INTEGER NOT NULL CHECK (max_active_time_ns >= 0),
    state TEXT NOT NULL CHECK (state IN ('CREATED','RUNNING','BLOCKED','NEEDS_REVIEW','FAILED','CANCELLED','READY')),
    current_stage TEXT NOT NULL,
    current_stage_ordinal INTEGER NOT NULL CHECK (current_stage_ordinal > 0),
    version INTEGER NOT NULL CHECK (version > 0),
    active_elapsed_ns INTEGER NOT NULL DEFAULT 0 CHECK (active_elapsed_ns >= 0 AND active_elapsed_ns <= max_active_time_ns),
    active_started_at TEXT,
    last_accounting_heartbeat_at TEXT,
    cancel_summary TEXT,
    final_package_occurrence_id TEXT,
    create_idempotency_key TEXT NOT NULL UNIQUE,
    create_command_digest TEXT NOT NULL CHECK (length(create_command_digest) = 71),
    create_result_json BLOB NOT NULL CHECK (length(create_result_json) > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    max_similarity_cost_micro_usd INTEGER NOT NULL DEFAULT 0 CHECK (max_similarity_cost_micro_usd >= 0),
    UNIQUE (run_id, workflow_revision, schema_version),
    CHECK ((state = 'READY') = (final_package_occurrence_id IS NOT NULL)),
    FOREIGN KEY (run_id, final_package_occurrence_id) REFERENCES verified_packages(run_id, archive_occurrence_id) ON DELETE RESTRICT,
    CHECK ((active_started_at IS NULL) = (last_accounting_heartbeat_at IS NULL)),
    CHECK (active_started_at IS NULL OR state = 'RUNNING'),
    CHECK ((state = 'CANCELLED' AND cancel_summary IS NOT NULL) OR (state <> 'CANCELLED' AND cancel_summary IS NULL))
) STRICT;

INSERT INTO runs_v26 SELECT * FROM runs;
DROP TABLE runs;
PRAGMA legacy_alter_table = ON;
ALTER TABLE runs_v26 RENAME TO runs;
PRAGMA legacy_alter_table = OFF;
CREATE UNIQUE INDEX runs_request_snapshot_identity ON runs(run_id, submitted_request_digest);
CREATE TRIGGER verified_packages_binding_insert
BEFORE INSERT ON verified_packages
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM runs r
        JOIN stage_records p ON p.run_id=r.run_id AND p.stage_name='package'
        JOIN stage_records q ON q.run_id=r.run_id AND q.stage_name='quality'
        JOIN artifact_occurrences a ON a.occurrence_id=NEW.archive_occurrence_id
        WHERE r.run_id=NEW.run_id AND r.state='RUNNING' AND r.current_stage='package'
          AND r.workflow_revision='mvp.idea.statement.similarity.solution.data.judge.package.v1'
          AND p.state='SUCCEEDED' AND p.output_digest=NEW.archive_digest AND p.input_digest=NEW.quality_digest
          AND q.state='SUCCEEDED' AND q.output_digest=NEW.quality_digest AND q.ordinal+1=p.ordinal
          AND a.run_id=r.run_id AND a.stage_name='package' AND a.attempt_id=NEW.attempt_id
          AND a.kind='NEW_WRITE' AND a.digest=NEW.archive_digest AND a.size<=r.max_package_bytes
          AND a.role='OUTPUT' AND a.logical_path='package/problem.zip' AND a.media_type='application/zip'
          AND json_extract(CAST(a.provenance_json AS TEXT),'$.producer')='mvp-package'
          AND json_extract(CAST(a.provenance_json AS TEXT),'$.input_digest')=NEW.quality_digest
    ) THEN RAISE(ABORT, 'verified package lost its current run evidence') END;
END;
CREATE TRIGGER verified_packages_no_update BEFORE UPDATE ON verified_packages
BEGIN SELECT RAISE(ABORT, 'verified packages are immutable'); END;
CREATE TRIGGER verified_packages_no_delete BEFORE DELETE ON verified_packages
BEGIN SELECT RAISE(ABORT, 'verified packages are immutable'); END;
CREATE TRIGGER runs_ready_binding_update BEFORE UPDATE ON runs
WHEN NEW.state='READY'
BEGIN
    SELECT CASE WHEN NEW.current_stage<>'package' OR NOT EXISTS (
        SELECT 1 FROM verified_packages p
        WHERE p.run_id=NEW.run_id AND p.archive_occurrence_id=NEW.final_package_occurrence_id AND p.status='VERIFIED'
    ) THEN RAISE(ABORT, 'READY requires this run verified package') END;
END;
CREATE TRIGGER runs_ready_immutable BEFORE UPDATE ON runs
WHEN OLD.state='READY'
BEGIN SELECT RAISE(ABORT, 'READY runs are immutable'); END;
CREATE TRIGGER runs_budget_limits_immutable
BEFORE UPDATE OF max_llm_calls, max_similarity_calls, max_llm_input_tokens, max_llm_output_tokens,
    max_llm_cost_micro_usd, max_similarity_cost_micro_usd, max_sandbox_creates, max_artifact_bytes,
    max_active_time_ns ON runs
WHEN NEW.max_llm_calls <> OLD.max_llm_calls
    OR NEW.max_similarity_calls <> OLD.max_similarity_calls
    OR NEW.max_llm_input_tokens <> OLD.max_llm_input_tokens
    OR NEW.max_llm_output_tokens <> OLD.max_llm_output_tokens
    OR NEW.max_llm_cost_micro_usd <> OLD.max_llm_cost_micro_usd
    OR NEW.max_similarity_cost_micro_usd <> OLD.max_similarity_cost_micro_usd
    OR NEW.max_sandbox_creates <> OLD.max_sandbox_creates
    OR NEW.max_artifact_bytes <> OLD.max_artifact_bytes
    OR NEW.max_active_time_ns <> OLD.max_active_time_ns
BEGIN
    SELECT RAISE(ABORT, 'run budget limits are immutable');
END;


