-- A cache source belongs to the same private run, but may come from an
-- earlier committed stage/attempt. Keep the current hit's full attempt binding.
-- Historical migrations are immutable; preserve all rows and dependent FKs.
PRAGMA defer_foreign_keys = ON;
CREATE TABLE call_records_v21 (
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
    FOREIGN KEY (cache_source_call_record_id, run_id)
        REFERENCES call_records(call_record_id, run_id) ON DELETE RESTRICT,
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

INSERT INTO call_records_v21 SELECT * FROM call_records;
DROP TABLE call_records;
PRAGMA legacy_alter_table = ON;
ALTER TABLE call_records_v21 RENAME TO call_records;
PRAGMA legacy_alter_table = OFF;

CREATE TRIGGER call_records_terminal_matrix_update
BEFORE UPDATE ON call_records
WHEN NEW.state = 'TERMINAL'
BEGIN
    SELECT CASE WHEN NEW.dispatch_kind = 'DISPATCHED' AND NOT EXISTS (
        SELECT 1 FROM physical_calls
        WHERE attempt_call_id = NEW.result_attempt_call_id
          AND call_record_id = NEW.call_record_id
          AND run_id = NEW.run_id
          AND stage_name = NEW.stage_name
          AND attempt_id = NEW.attempt_id
          AND state IN ('COMPLETED','UNKNOWN')
    ) THEN RAISE(ABORT, 'DISPATCHED result physical call is not terminal') END;
    SELECT CASE WHEN NEW.dispatch_kind = 'CACHE_HIT' AND EXISTS (
        SELECT 1 FROM physical_calls WHERE call_record_id = NEW.call_record_id
    ) THEN RAISE(ABORT, 'CACHE_HIT cannot have physical calls') END;
END;

CREATE TRIGGER call_records_terminal_matrix_insert
BEFORE INSERT ON call_records
WHEN NEW.state = 'TERMINAL'
BEGIN
    SELECT CASE WHEN NEW.dispatch_kind = 'DISPATCHED' AND NOT EXISTS (
        SELECT 1 FROM physical_calls
        WHERE attempt_call_id = NEW.result_attempt_call_id
          AND call_record_id = NEW.call_record_id
          AND run_id = NEW.run_id
          AND stage_name = NEW.stage_name
          AND attempt_id = NEW.attempt_id
          AND state IN ('COMPLETED','UNKNOWN')
    ) THEN RAISE(ABORT, 'DISPATCHED result physical call is not terminal') END;
    SELECT CASE WHEN NEW.dispatch_kind = 'CACHE_HIT' AND EXISTS (
        SELECT 1 FROM physical_calls WHERE call_record_id = NEW.call_record_id
    ) THEN RAISE(ABORT, 'CACHE_HIT cannot have physical calls') END;
END;

CREATE TEMP TABLE cpgen_m21_foreign_key_guard (
    violation_count INTEGER NOT NULL CHECK (violation_count = 0)
);
INSERT INTO cpgen_m21_foreign_key_guard SELECT count(*) FROM pragma_foreign_key_check;
DROP TABLE cpgen_m21_foreign_key_guard;
PRAGMA defer_foreign_keys = OFF;
