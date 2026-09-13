-- Record volume creation as its own physical boundary. It consumes no
-- container-create budget; exact plan cardinality and lifecycle bound volumes.
-- Preserve historical physical keys, states, reservations and all dependent FKs.
PRAGMA defer_foreign_keys = ON;

CREATE TABLE physical_calls_v25 (
    attempt_call_id TEXT PRIMARY KEY,
    call_record_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    retry_group TEXT NOT NULL CHECK (retry_group <> '' AND length(retry_group) <= 128),
    retry_ordinal INTEGER NOT NULL CHECK (retry_ordinal > 0),
    physical_kind TEXT NOT NULL CHECK (physical_kind IN (
        'LLM_REQUEST','SIMILARITY_REQUEST','DOCKER_ENGINE_PING','DOCKER_CONTAINER_CREATE','DOCKER_VOLUME_CREATE','LOCAL_ARTIFACT_WRITE'
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

INSERT INTO physical_calls_v25 SELECT * FROM physical_calls;
DROP TABLE physical_calls;
PRAGMA legacy_alter_table = ON;
ALTER TABLE physical_calls_v25 RENAME TO physical_calls;
PRAGMA legacy_alter_table = OFF;

CREATE INDEX physical_calls_record_order ON physical_calls(call_record_id, ordinal);

CREATE UNIQUE INDEX physical_calls_record_retry_ordinal ON physical_calls(call_record_id, retry_ordinal);

CREATE UNIQUE INDEX physical_calls_sandbox_scope
    ON physical_calls(attempt_call_id, run_id, stage_name, attempt_id);

CREATE TRIGGER physical_calls_global_retry_insert
BEFORE INSERT ON physical_calls
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM call_records
        WHERE call_record_id = NEW.call_record_id
          AND NEW.ordinal = NEW.retry_ordinal
          AND NEW.ordinal <= retry_max_attempts
    ) THEN RAISE(ABORT, 'physical call exceeds the global retry plan') END;
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM call_records
        WHERE call_record_id = NEW.call_record_id AND state = 'TERMINAL' AND dispatch_kind = 'CACHE_HIT'
    ) THEN RAISE(ABORT, 'CACHE_HIT cannot have physical calls') END;
END;

CREATE TRIGGER physical_calls_global_retry_update
BEFORE UPDATE OF call_record_id, ordinal, retry_ordinal ON physical_calls
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM call_records
        WHERE call_record_id = NEW.call_record_id
          AND NEW.ordinal = NEW.retry_ordinal
          AND NEW.ordinal <= retry_max_attempts
    ) THEN RAISE(ABORT, 'physical call exceeds the global retry plan') END;
END;

CREATE TRIGGER physical_calls_kind_binding
BEFORE INSERT ON physical_calls
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM call_records call
        WHERE call.call_record_id = NEW.call_record_id
          AND (
            (call.call_kind = 'LLM_GENERATE' AND NEW.physical_kind IN ('LLM_REQUEST','LOCAL_ARTIFACT_WRITE')) OR
            (call.call_kind = 'SIMILARITY_SEARCH' AND NEW.physical_kind IN ('SIMILARITY_REQUEST','LOCAL_ARTIFACT_WRITE')) OR
            (call.call_kind IN ('SANDBOX_COMPILE','SANDBOX_RUN','SANDBOX_PROBE')
                AND NEW.physical_kind IN ('DOCKER_ENGINE_PING','DOCKER_CONTAINER_CREATE','DOCKER_VOLUME_CREATE','LOCAL_ARTIFACT_WRITE'))
          )
    ) THEN RAISE(ABORT, 'physical call kind does not match logical call') END;
END;

CREATE TRIGGER physical_calls_terminal_parent_update
BEFORE UPDATE ON physical_calls
BEGIN
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM call_records
        WHERE call_record_id = NEW.call_record_id
          AND state = 'TERMINAL'
          AND dispatch_kind = 'DISPATCHED'
          AND result_attempt_call_id = NEW.attempt_call_id
          AND NEW.state NOT IN ('COMPLETED','UNKNOWN')
    ) THEN RAISE(ABORT, 'terminal DISPATCHED result must stay terminal') END;
END;
