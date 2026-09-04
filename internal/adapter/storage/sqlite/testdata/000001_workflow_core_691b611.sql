CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY CHECK (version > 0),
    name TEXT NOT NULL UNIQUE CHECK (name <> ''),
    sha256 TEXT NOT NULL CHECK (length(sha256) = 71 AND sha256 GLOB 'sha256:[0-9a-f]*'),
    applied_at TEXT NOT NULL CHECK (applied_at <> '')
) STRICT;

CREATE TABLE runs (
    run_id TEXT PRIMARY KEY,
    submitted_request_json BLOB NOT NULL CHECK (length(submitted_request_json) > 0),
    submitted_request_digest TEXT NOT NULL CHECK (length(submitted_request_digest) = 71),
    effective_seed INTEGER NOT NULL,
    redacted_effective_config_json BLOB NOT NULL CHECK (length(redacted_effective_config_json) > 0),
    redacted_effective_config_digest TEXT NOT NULL CHECK (length(redacted_effective_config_digest) = 71),
    workflow_digest TEXT NOT NULL CHECK (length(workflow_digest) = 71),
    workflow_revision TEXT NOT NULL,
    schema_version TEXT NOT NULL,
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
    state TEXT NOT NULL CHECK (state IN ('CREATED','RUNNING','BLOCKED','NEEDS_REVIEW','FAILED','CANCELLED')),
    current_stage TEXT NOT NULL,
    current_stage_ordinal INTEGER NOT NULL CHECK (current_stage_ordinal > 0),
    version INTEGER NOT NULL CHECK (version > 0),
    active_elapsed_ns INTEGER NOT NULL DEFAULT 0 CHECK (active_elapsed_ns >= 0 AND active_elapsed_ns <= max_active_time_ns),
    active_started_at TEXT,
    last_accounting_heartbeat_at TEXT,
    cancel_summary TEXT,
    final_package_occurrence_id TEXT CHECK (final_package_occurrence_id IS NULL),
    create_idempotency_key TEXT NOT NULL UNIQUE,
    create_command_digest TEXT NOT NULL CHECK (length(create_command_digest) = 71),
    create_result_json BLOB NOT NULL CHECK (length(create_result_json) > 0),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((active_started_at IS NULL) = (last_accounting_heartbeat_at IS NULL)),
    CHECK ((state = 'CANCELLED' AND cancel_summary IS NOT NULL) OR (state <> 'CANCELLED' AND cancel_summary IS NULL))
) STRICT;

CREATE TABLE stage_records (
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    state TEXT NOT NULL CHECK (state IN ('PENDING','RUNNING','SUCCEEDED','BLOCKED','NEEDS_REVIEW','FAILED','CANCELLED')),
    version INTEGER NOT NULL CHECK (version > 0),
    input_digest TEXT NOT NULL CHECK (length(input_digest) = 71),
    output_digest TEXT CHECK (output_digest IS NULL OR length(output_digest) = 71),
    attempt_count INTEGER NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    current_attempt_id TEXT,
    logical_idempotency_key TEXT NOT NULL,
    review_evidence_digest TEXT CHECK (review_evidence_digest IS NULL OR length(review_evidence_digest) = 71),
    review_policy_digest TEXT CHECK (review_policy_digest IS NULL OR length(review_policy_digest) = 71),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (run_id, stage_name),
    UNIQUE (run_id, ordinal),
    UNIQUE (run_id, stage_name, current_attempt_id),
    FOREIGN KEY (run_id) REFERENCES runs(run_id) ON DELETE RESTRICT,
    CHECK (
        (state = 'PENDING' AND attempt_count >= 0 AND current_attempt_id IS NULL AND output_digest IS NULL AND review_evidence_digest IS NULL AND review_policy_digest IS NULL) OR
        (state = 'RUNNING' AND attempt_count > 0 AND current_attempt_id IS NOT NULL AND output_digest IS NULL AND review_evidence_digest IS NULL AND review_policy_digest IS NULL) OR
        (state = 'SUCCEEDED' AND attempt_count > 0 AND current_attempt_id IS NULL AND output_digest IS NOT NULL AND review_evidence_digest IS NULL AND review_policy_digest IS NULL) OR
        (state = 'BLOCKED' AND attempt_count > 0 AND current_attempt_id IS NULL AND output_digest IS NULL AND review_evidence_digest IS NULL AND review_policy_digest IS NULL) OR
        (state = 'NEEDS_REVIEW' AND attempt_count > 0 AND current_attempt_id IS NULL AND output_digest IS NULL AND review_evidence_digest IS NOT NULL AND review_policy_digest IS NOT NULL) OR
        (state = 'FAILED' AND attempt_count > 0 AND current_attempt_id IS NULL AND output_digest IS NULL) OR
        (state = 'CANCELLED' AND current_attempt_id IS NULL AND output_digest IS NULL)
    )
) STRICT;

CREATE TABLE stage_attempts (
    attempt_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    state TEXT NOT NULL CHECK (state IN ('RUNNING','SUCCEEDED','BLOCKED','NEEDS_REVIEW','FAILED','CANCELLED','INTERRUPTED')),
    input_digest TEXT NOT NULL CHECK (length(input_digest) = 71),
    output_digest TEXT CHECK (output_digest IS NULL OR length(output_digest) = 71),
    cause TEXT CHECK (cause IS NULL OR cause IN ('user_cancel','revision_invalidated','step_deadline','run_budget_deadline')),
    started_at TEXT NOT NULL,
    finished_at TEXT,
    UNIQUE (run_id, stage_name, ordinal),
    UNIQUE (run_id, stage_name, attempt_id),
    FOREIGN KEY (run_id, stage_name) REFERENCES stage_records(run_id, stage_name) ON DELETE RESTRICT,
    CHECK (
        (state = 'RUNNING' AND output_digest IS NULL AND cause IS NULL AND finished_at IS NULL) OR
        (state = 'SUCCEEDED' AND output_digest IS NOT NULL AND cause IS NULL AND finished_at IS NOT NULL) OR
        (state IN ('BLOCKED','NEEDS_REVIEW','FAILED') AND output_digest IS NULL AND cause IS NULL AND finished_at IS NOT NULL) OR
        (state IN ('CANCELLED','INTERRUPTED') AND output_digest IS NULL AND cause IS NOT NULL AND finished_at IS NOT NULL)
    )
) STRICT;

CREATE INDEX stage_attempts_run_stage ON stage_attempts(run_id, stage_name, ordinal);

CREATE TABLE run_events (
    run_id TEXT NOT NULL,
    version INTEGER NOT NULL CHECK (version > 0),
    event_type TEXT NOT NULL CHECK (event_type IN ('RUN_CREATED','STAGE_BEGAN','STAGE_FINISHED','STAGE_INTERRUPTED','CANCEL_REQUESTED','ACTIVE_TIME_ACCOUNTED','REVIEW_CREATED','REVIEW_APPLIED')),
    stage_name TEXT,
    idempotency_key TEXT NOT NULL,
    command_digest TEXT NOT NULL CHECK (length(command_digest) = 71),
    result_json BLOB NOT NULL CHECK (length(result_json) > 0),
    occurred_at TEXT NOT NULL,
    PRIMARY KEY (run_id, version),
    UNIQUE (run_id, idempotency_key),
    FOREIGN KEY (run_id) REFERENCES runs(run_id) ON DELETE RESTRICT
) STRICT;

CREATE TABLE control_requests (
    control_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind = 'CANCEL'),
    state TEXT NOT NULL CHECK (state IN ('PENDING','APPLIED','STALE')),
    reason TEXT NOT NULL CHECK (reason <> ''),
    expected_run_version INTEGER NOT NULL CHECK (expected_run_version > 0),
    idempotency_key TEXT NOT NULL,
    command_digest TEXT NOT NULL CHECK (length(command_digest) = 71),
    created_at TEXT NOT NULL,
    applied_at TEXT,
    CHECK ((state = 'PENDING' AND applied_at IS NULL) OR (state <> 'PENDING' AND applied_at IS NOT NULL)),
    UNIQUE (run_id, idempotency_key),
    FOREIGN KEY (run_id) REFERENCES runs(run_id) ON DELETE RESTRICT
) STRICT;

CREATE UNIQUE INDEX one_pending_cancel_per_run ON control_requests(run_id) WHERE state = 'PENDING';

CREATE TABLE review_decisions (
    review_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('REVISE','RETRY','WAIVE','REJECT')),
    state TEXT NOT NULL CHECK (state IN ('PENDING','APPLIED','REJECTED','STALE')),
    expected_run_version INTEGER NOT NULL CHECK (expected_run_version > 0),
    run_version INTEGER NOT NULL CHECK (run_version > 0),
    workflow_revision TEXT NOT NULL CHECK (workflow_revision <> ''),
    stage_name TEXT NOT NULL,
    stage_input_digest TEXT NOT NULL CHECK (length(stage_input_digest) = 71),
    evidence_digest TEXT NOT NULL CHECK (length(evidence_digest) = 71),
    policy_digest TEXT NOT NULL CHECK (length(policy_digest) = 71),
    requested_edits_digest TEXT CHECK (requested_edits_digest IS NULL OR length(requested_edits_digest) = 71),
    waiver_scope_digest TEXT CHECK (waiver_scope_digest IS NULL OR length(waiver_scope_digest) = 71),
    external_condition_digest TEXT CHECK (external_condition_digest IS NULL OR length(external_condition_digest) = 71),
    budget_increase_json BLOB,
    waivable_gate INTEGER NOT NULL CHECK (waivable_gate IN (0,1)),
    reviewer TEXT NOT NULL CHECK (reviewer <> ''),
    reason TEXT NOT NULL CHECK (reason <> ''),
    idempotency_key TEXT NOT NULL,
    command_digest TEXT NOT NULL CHECK (length(command_digest) = 71),
    created_at TEXT NOT NULL,
    applied_at TEXT,
    CHECK ((state = 'PENDING' AND applied_at IS NULL) OR (state <> 'PENDING' AND applied_at IS NOT NULL)),
    CHECK (
        (kind = 'REVISE' AND requested_edits_digest IS NOT NULL AND waiver_scope_digest IS NULL AND external_condition_digest IS NULL AND budget_increase_json IS NULL AND waivable_gate = 0) OR
        (kind = 'RETRY' AND requested_edits_digest IS NULL AND waiver_scope_digest IS NULL AND (external_condition_digest IS NOT NULL OR budget_increase_json IS NOT NULL) AND waivable_gate = 0) OR
        (kind = 'WAIVE' AND requested_edits_digest IS NULL AND waiver_scope_digest IS NOT NULL AND external_condition_digest IS NULL AND budget_increase_json IS NULL AND waivable_gate = 1) OR
        (kind = 'REJECT' AND requested_edits_digest IS NULL AND waiver_scope_digest IS NULL AND external_condition_digest IS NULL AND budget_increase_json IS NULL AND waivable_gate = 0)
    ),
    UNIQUE (run_id, idempotency_key),
    FOREIGN KEY (run_id) REFERENCES runs(run_id) ON DELETE RESTRICT,
    FOREIGN KEY (run_id, stage_name) REFERENCES stage_records(run_id, stage_name) ON DELETE RESTRICT
) STRICT;

CREATE UNIQUE INDEX one_pending_review_per_run ON review_decisions(run_id) WHERE state = 'PENDING';
