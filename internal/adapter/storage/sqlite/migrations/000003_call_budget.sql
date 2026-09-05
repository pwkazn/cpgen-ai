CREATE UNIQUE INDEX runs_request_snapshot_identity
ON runs(run_id, submitted_request_digest);

CREATE TABLE budget_accounts (
    run_id TEXT NOT NULL,
    request_snapshot_digest TEXT NOT NULL CHECK (length(request_snapshot_digest) = 71),
    dimension TEXT NOT NULL CHECK (dimension IN (
        'LLM_CALLS','LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS','EXTERNAL_COST_MICRO_USD',
        'SIMILARITY_CALLS','DOCKER_CONTAINER_CREATES','ARTIFACT_PHYSICAL_NEW_BYTES','ACTIVE_TIME_NS'
    )),
    limit_value INTEGER NOT NULL CHECK (limit_value >= 0),
    reserved_value INTEGER NOT NULL DEFAULT 0 CHECK (reserved_value >= 0),
    consumed_value INTEGER NOT NULL DEFAULT 0 CHECK (consumed_value >= 0),
    account_version INTEGER NOT NULL DEFAULT 1 CHECK (account_version > 0),
    PRIMARY KEY (run_id, dimension),
    FOREIGN KEY (run_id, request_snapshot_digest)
        REFERENCES runs(run_id, submitted_request_digest) ON DELETE RESTRICT,
    CHECK (consumed_value <= limit_value AND reserved_value <= limit_value - consumed_value)
) STRICT;

INSERT INTO budget_accounts(run_id, request_snapshot_digest, dimension, limit_value, reserved_value, consumed_value)
SELECT run_id, submitted_request_digest, 'LLM_CALLS', max_llm_calls, 0, 0 FROM runs
UNION ALL
SELECT run_id, submitted_request_digest, 'LLM_INPUT_TOKENS', max_llm_input_tokens, 0, 0 FROM runs
UNION ALL
SELECT run_id, submitted_request_digest, 'LLM_OUTPUT_TOKENS', max_llm_output_tokens, 0, 0 FROM runs
UNION ALL
SELECT run_id, submitted_request_digest, 'EXTERNAL_COST_MICRO_USD', max_llm_cost_micro_usd, 0, 0 FROM runs
UNION ALL
SELECT run_id, submitted_request_digest, 'SIMILARITY_CALLS', max_similarity_calls, 0, 0 FROM runs
UNION ALL
SELECT run_id, submitted_request_digest, 'DOCKER_CONTAINER_CREATES', max_sandbox_creates, 0, 0 FROM runs
UNION ALL
SELECT run_id, submitted_request_digest, 'ARTIFACT_PHYSICAL_NEW_BYTES', max_artifact_bytes, 0, 0 FROM runs
UNION ALL
SELECT run_id, submitted_request_digest, 'ACTIVE_TIME_NS', max_active_time_ns, 0, active_elapsed_ns FROM runs;

CREATE TABLE call_records (
    call_record_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    logical_operation_id TEXT NOT NULL CHECK (logical_operation_id <> '' AND length(logical_operation_id) <= 256),
    call_kind TEXT NOT NULL CHECK (call_kind IN (
        'LLM_GENERATE','SIMILARITY_SEARCH','SANDBOX_COMPILE','SANDBOX_RUN','SANDBOX_PROBE','CACHE_REUSE'
    )),
    provider TEXT NOT NULL CHECK (provider <> '' AND length(provider) <= 256),
    request_digest TEXT NOT NULL CHECK (length(request_digest) = 71),
    policy_digest TEXT NOT NULL CHECK (length(policy_digest) = 71),
    retry_max_attempts INTEGER NOT NULL CHECK (retry_max_attempts > 0),
    retry_initial_backoff_ns INTEGER NOT NULL CHECK (retry_initial_backoff_ns > 0),
    retry_max_backoff_ns INTEGER NOT NULL CHECK (retry_max_backoff_ns >= retry_initial_backoff_ns),
    retry_jitter_seed_digest TEXT NOT NULL CHECK (length(retry_jitter_seed_digest) = 71),
    state TEXT NOT NULL CHECK (state IN ('OPEN','PREPARED','TERMINAL')),
    dispatch_kind TEXT CHECK (dispatch_kind IS NULL OR dispatch_kind IN ('DISPATCHED','CACHE_HIT','NO_DISPATCH')),
    result_attempt_call_id TEXT,
    cache_source_call_record_id TEXT,
    cache_hit_call_record_id TEXT,
    failure_code TEXT CHECK (failure_code IS NULL OR failure_code IN (
        'transport','rate_limited','unavailable','protocol','capability_missing','version_mismatch',
        'budget_exhausted','policy_rejected','circuit_open','boundary_unknown'
    )),
    failure_class TEXT CHECK (failure_class IS NULL OR failure_class IN ('RETRYABLE','BLOCKED','INCOMPATIBLE','REJECTED','UNKNOWN')),
    failure_json BLOB CHECK (failure_json IS NULL OR (length(failure_json) > 0 AND json_valid(CAST(failure_json AS TEXT)))),
    opened_at TEXT NOT NULL CHECK (length(opened_at) = 30 AND julianday(opened_at) IS NOT NULL),
    prepared_at TEXT CHECK (prepared_at IS NULL OR (length(prepared_at) = 30 AND julianday(prepared_at) IS NOT NULL AND prepared_at >= opened_at)),
    completed_at TEXT CHECK (completed_at IS NULL OR (length(completed_at) = 30 AND julianday(completed_at) IS NOT NULL AND completed_at >= opened_at)),
    open_idempotency_key TEXT NOT NULL,
    open_command_digest TEXT NOT NULL CHECK (length(open_command_digest) = 71),
    prepare_idempotency_key TEXT,
    prepare_command_digest TEXT CHECK (prepare_command_digest IS NULL OR length(prepare_command_digest) = 71),
    finish_idempotency_key TEXT,
    finish_command_digest TEXT CHECK (finish_command_digest IS NULL OR length(finish_command_digest) = 71),
    UNIQUE (run_id, logical_operation_id),
    UNIQUE (run_id, open_idempotency_key),
    UNIQUE (run_id, call_record_id),
    UNIQUE (call_record_id, run_id, stage_name, attempt_id),
    FOREIGN KEY (run_id, stage_name) REFERENCES stage_records(run_id, stage_name) ON DELETE RESTRICT,
    FOREIGN KEY (run_id, stage_name, attempt_id) REFERENCES stage_attempts(run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    FOREIGN KEY (cache_source_call_record_id, run_id, stage_name, attempt_id)
        REFERENCES call_records(call_record_id, run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    FOREIGN KEY (cache_hit_call_record_id, run_id, stage_name, attempt_id)
        REFERENCES call_records(call_record_id, run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    FOREIGN KEY (result_attempt_call_id, call_record_id, run_id, stage_name, attempt_id)
        REFERENCES physical_calls(attempt_call_id, call_record_id, run_id, stage_name, attempt_id)
        ON DELETE RESTRICT DEFERRABLE INITIALLY DEFERRED,
    CHECK ((failure_code IS NULL) = (failure_class IS NULL) AND (failure_code IS NULL) = (failure_json IS NULL)),
    CHECK ((prepare_idempotency_key IS NULL) = (prepare_command_digest IS NULL)),
    CHECK ((finish_idempotency_key IS NULL) = (finish_command_digest IS NULL)),
    CHECK (
        (state = 'OPEN' AND prepared_at IS NULL AND dispatch_kind IS NULL AND result_attempt_call_id IS NULL
            AND cache_source_call_record_id IS NULL AND cache_hit_call_record_id IS NULL
            AND failure_code IS NULL AND completed_at IS NULL AND prepare_idempotency_key IS NULL AND finish_idempotency_key IS NULL) OR
        (state = 'PREPARED' AND prepared_at IS NOT NULL AND dispatch_kind IS NULL AND result_attempt_call_id IS NULL
            AND cache_source_call_record_id IS NULL AND cache_hit_call_record_id IS NULL
            AND failure_code IS NULL AND completed_at IS NULL AND prepare_idempotency_key IS NOT NULL AND finish_idempotency_key IS NULL) OR
        (state = 'TERMINAL' AND dispatch_kind IS NOT NULL AND completed_at IS NOT NULL AND finish_idempotency_key IS NOT NULL AND (
            (dispatch_kind = 'DISPATCHED' AND result_attempt_call_id IS NOT NULL
                AND cache_source_call_record_id IS NULL AND cache_hit_call_record_id IS NULL) OR
            (dispatch_kind = 'CACHE_HIT' AND result_attempt_call_id IS NULL AND failure_code IS NULL
                AND cache_source_call_record_id IS NOT NULL AND cache_hit_call_record_id = call_record_id
                AND cache_source_call_record_id <> cache_hit_call_record_id) OR
            (dispatch_kind = 'NO_DISPATCH' AND result_attempt_call_id IS NULL
                AND cache_source_call_record_id IS NULL AND cache_hit_call_record_id IS NULL AND failure_code IS NOT NULL)
        ))
    )
) STRICT;

CREATE TABLE physical_calls (
    attempt_call_id TEXT PRIMARY KEY,
    call_record_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    retry_group TEXT NOT NULL CHECK (retry_group <> '' AND length(retry_group) <= 128),
    retry_ordinal INTEGER NOT NULL CHECK (retry_ordinal > 0),
    physical_kind TEXT NOT NULL CHECK (physical_kind IN (
        'LLM_REQUEST','SIMILARITY_REQUEST','DOCKER_ENGINE_PING','DOCKER_CONTAINER_CREATE','LOCAL_ARTIFACT_WRITE'
    )),
    provider TEXT NOT NULL CHECK (provider <> '' AND length(provider) <= 256),
    request_digest TEXT NOT NULL CHECK (length(request_digest) = 71),
    idempotency_key TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('PREPARED','DISPATCHING','SENT','COMPLETED','ABORTED_NO_DISPATCH','UNKNOWN')),
    outcome_kind TEXT CHECK (outcome_kind IS NULL OR outcome_kind IN ('SUCCESS','RETRYABLE_FAILURE','PERMANENT_FAILURE','NO_SEND','UNKNOWN_BOUNDARY')),
    failure_code TEXT CHECK (failure_code IS NULL OR failure_code IN (
        'transport','rate_limited','unavailable','protocol','capability_missing','version_mismatch',
        'budget_exhausted','policy_rejected','circuit_open','boundary_unknown'
    )),
    failure_class TEXT CHECK (failure_class IS NULL OR failure_class IN ('RETRYABLE','BLOCKED','INCOMPATIBLE','REJECTED','UNKNOWN')),
    failure_json BLOB CHECK (failure_json IS NULL OR (length(failure_json) > 0 AND json_valid(CAST(failure_json AS TEXT)))),
    provider_request_id TEXT CHECK (provider_request_id IS NULL OR provider_request_id <> ''),
    response_digest TEXT CHECK (response_digest IS NULL OR length(response_digest) = 71),
    prepared_at TEXT NOT NULL CHECK (length(prepared_at) = 30 AND julianday(prepared_at) IS NOT NULL),
    dispatch_started_at TEXT CHECK (dispatch_started_at IS NULL OR (length(dispatch_started_at) = 30 AND julianday(dispatch_started_at) IS NOT NULL AND dispatch_started_at >= prepared_at)),
    sent_at TEXT CHECK (sent_at IS NULL OR (length(sent_at) = 30 AND julianday(sent_at) IS NOT NULL AND sent_at >= dispatch_started_at)),
    completed_at TEXT CHECK (completed_at IS NULL OR (length(completed_at) = 30 AND julianday(completed_at) IS NOT NULL AND completed_at >= prepared_at)),
    begin_idempotency_key TEXT,
    begin_command_digest TEXT CHECK (begin_command_digest IS NULL OR length(begin_command_digest) = 71),
    grant_digest TEXT CHECK (grant_digest IS NULL OR length(grant_digest) = 71),
    complete_idempotency_key TEXT,
    complete_command_digest TEXT CHECK (complete_command_digest IS NULL OR length(complete_command_digest) = 71),
    UNIQUE (call_record_id, ordinal),
    UNIQUE (call_record_id, retry_group, retry_ordinal),
    UNIQUE (run_id, idempotency_key),
    UNIQUE (attempt_call_id, call_record_id),
    UNIQUE (attempt_call_id, call_record_id, run_id, stage_name, attempt_id),
    UNIQUE (attempt_call_id, call_record_id, run_id, stage_name, attempt_id, physical_kind),
    FOREIGN KEY (call_record_id, run_id, stage_name, attempt_id)
        REFERENCES call_records(call_record_id, run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    CHECK ((failure_code IS NULL) = (failure_class IS NULL) AND (failure_code IS NULL) = (failure_json IS NULL)),
    CHECK ((begin_idempotency_key IS NULL) = (begin_command_digest IS NULL)),
    CHECK ((begin_idempotency_key IS NULL) = (grant_digest IS NULL)),
    CHECK ((complete_idempotency_key IS NULL) = (complete_command_digest IS NULL)),
    CHECK (
        (state = 'PREPARED' AND outcome_kind IS NULL AND failure_code IS NULL AND provider_request_id IS NULL
            AND response_digest IS NULL AND dispatch_started_at IS NULL AND sent_at IS NULL AND completed_at IS NULL
            AND begin_idempotency_key IS NULL AND complete_idempotency_key IS NULL) OR
        (state = 'DISPATCHING' AND outcome_kind IS NULL AND failure_code IS NULL AND provider_request_id IS NULL
            AND response_digest IS NULL AND dispatch_started_at IS NOT NULL AND sent_at IS NULL AND completed_at IS NULL
            AND begin_idempotency_key IS NOT NULL AND complete_idempotency_key IS NULL) OR
        (state = 'SENT' AND outcome_kind IS NULL AND failure_code IS NULL AND response_digest IS NULL
            AND dispatch_started_at IS NOT NULL AND sent_at IS NOT NULL AND completed_at IS NULL
            AND begin_idempotency_key IS NOT NULL AND complete_idempotency_key IS NULL) OR
        (state = 'COMPLETED' AND outcome_kind IN ('SUCCESS','RETRYABLE_FAILURE','PERMANENT_FAILURE')
            AND provider_request_id IS NOT NULL AND response_digest IS NOT NULL
            AND dispatch_started_at IS NOT NULL AND sent_at IS NOT NULL AND completed_at IS NOT NULL
            AND begin_idempotency_key IS NOT NULL AND complete_idempotency_key IS NOT NULL
            AND ((outcome_kind = 'SUCCESS' AND failure_code IS NULL)
                OR (outcome_kind = 'RETRYABLE_FAILURE' AND failure_class = 'RETRYABLE')
                OR (outcome_kind = 'PERMANENT_FAILURE' AND failure_code IS NOT NULL AND failure_class NOT IN ('RETRYABLE','UNKNOWN')))) OR
        (state = 'ABORTED_NO_DISPATCH' AND outcome_kind = 'NO_SEND' AND failure_code IS NOT NULL
            AND provider_request_id IS NULL AND response_digest IS NULL AND sent_at IS NULL AND completed_at IS NOT NULL
            AND complete_idempotency_key IS NOT NULL) OR
        (state = 'UNKNOWN' AND outcome_kind = 'UNKNOWN_BOUNDARY' AND failure_code = 'boundary_unknown' AND failure_class = 'UNKNOWN'
            AND response_digest IS NULL AND dispatch_started_at IS NOT NULL AND completed_at IS NOT NULL
            AND begin_idempotency_key IS NOT NULL AND complete_idempotency_key IS NOT NULL)
    )
) STRICT;

CREATE TRIGGER physical_calls_kind_binding
BEFORE INSERT ON physical_calls
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM call_records call
        WHERE call.call_record_id = NEW.call_record_id
          AND (
            (call.call_kind = 'LLM_GENERATE' AND NEW.physical_kind = 'LLM_REQUEST') OR
            (call.call_kind = 'SIMILARITY_SEARCH' AND NEW.physical_kind = 'SIMILARITY_REQUEST') OR
            (call.call_kind IN ('SANDBOX_COMPILE','SANDBOX_RUN','SANDBOX_PROBE')
                AND NEW.physical_kind IN ('DOCKER_ENGINE_PING','DOCKER_CONTAINER_CREATE','LOCAL_ARTIFACT_WRITE'))
          )
    ) THEN RAISE(ABORT, 'physical call kind does not match logical call') END;
END;

CREATE TABLE budget_reservations (
    reservation_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    call_record_id TEXT NOT NULL,
    attempt_call_id TEXT NOT NULL,
    physical_kind TEXT NOT NULL,
    dimension TEXT NOT NULL CHECK (dimension IN (
        'LLM_CALLS','LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS','EXTERNAL_COST_MICRO_USD',
        'SIMILARITY_CALLS','DOCKER_CONTAINER_CREATES','ARTIFACT_PHYSICAL_NEW_BYTES','ACTIVE_TIME_NS'
    )),
    subkey TEXT NOT NULL CHECK (subkey <> '' AND length(subkey) <= 128),
    upper_bound INTEGER NOT NULL CHECK (upper_bound > 0),
    settled_value INTEGER CHECK (settled_value IS NULL OR (settled_value >= 0 AND settled_value <= upper_bound)),
    state TEXT NOT NULL CHECK (state IN ('RESERVED','SETTLED','RELEASED')),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    settled_at TEXT CHECK (settled_at IS NULL OR (length(settled_at) = 30 AND julianday(settled_at) IS NOT NULL AND settled_at >= created_at)),
    UNIQUE (attempt_call_id, dimension, subkey),
    UNIQUE (reservation_id, run_id, dimension, subkey),
    FOREIGN KEY (run_id, dimension) REFERENCES budget_accounts(run_id, dimension) ON DELETE RESTRICT,
    FOREIGN KEY (attempt_call_id, call_record_id, run_id, stage_name, attempt_id, physical_kind)
        REFERENCES physical_calls(attempt_call_id, call_record_id, run_id, stage_name, attempt_id, physical_kind)
        ON DELETE RESTRICT,
    CHECK (
        (state = 'RESERVED' AND settled_value IS NULL AND settled_at IS NULL) OR
        (state = 'SETTLED' AND settled_value IS NOT NULL AND settled_at IS NOT NULL) OR
        (state = 'RELEASED' AND settled_value = 0 AND settled_at IS NOT NULL)
    ),
    CHECK (
        (physical_kind = 'LLM_REQUEST' AND dimension IN ('LLM_CALLS','LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS','EXTERNAL_COST_MICRO_USD')) OR
        (physical_kind = 'SIMILARITY_REQUEST' AND dimension IN ('SIMILARITY_CALLS','EXTERNAL_COST_MICRO_USD')) OR
        (physical_kind = 'DOCKER_CONTAINER_CREATE' AND dimension = 'DOCKER_CONTAINER_CREATES') OR
        (physical_kind = 'LOCAL_ARTIFACT_WRITE' AND dimension = 'ARTIFACT_PHYSICAL_NEW_BYTES')
    )
) STRICT;

CREATE INDEX physical_calls_record_order ON physical_calls(call_record_id, ordinal);
CREATE INDEX budget_reservations_account ON budget_reservations(run_id, dimension, state);
