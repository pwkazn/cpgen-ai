PRAGMA defer_foreign_keys = ON;

ALTER TABLE runs ADD COLUMN max_similarity_cost_micro_usd INTEGER NOT NULL DEFAULT 0
    CHECK (max_similarity_cost_micro_usd >= 0);

CREATE TABLE _m4_budget_accounts (
    run_id TEXT NOT NULL,
    request_snapshot_digest TEXT NOT NULL CHECK (length(request_snapshot_digest) = 71),
    dimension TEXT NOT NULL CHECK (dimension IN (
        'LLM_CALLS','LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS','EXTERNAL_COST_MICRO_USD',
        'SIMILARITY_CALLS','SIMILARITY_COST_MICRO_USD','DOCKER_CONTAINER_CREATES',
        'ARTIFACT_PHYSICAL_NEW_BYTES','ACTIVE_TIME_NS'
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

INSERT INTO _m4_budget_accounts(
    run_id, request_snapshot_digest, dimension, limit_value, reserved_value, consumed_value, account_version
)
SELECT run_id, request_snapshot_digest, dimension, limit_value, reserved_value, consumed_value, account_version
FROM budget_accounts;

INSERT INTO _m4_budget_accounts(
    run_id, request_snapshot_digest, dimension, limit_value, reserved_value, consumed_value, account_version
)
SELECT run_id, submitted_request_digest, 'SIMILARITY_COST_MICRO_USD', max_similarity_cost_micro_usd, 0, 0, 1
FROM runs;

CREATE TABLE _m4_budget_reservations (
    reservation_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    call_record_id TEXT NOT NULL,
    attempt_call_id TEXT NOT NULL,
    physical_kind TEXT NOT NULL,
    dimension TEXT NOT NULL CHECK (dimension IN (
        'LLM_CALLS','LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS','EXTERNAL_COST_MICRO_USD',
        'SIMILARITY_CALLS','SIMILARITY_COST_MICRO_USD','DOCKER_CONTAINER_CREATES',
        'ARTIFACT_PHYSICAL_NEW_BYTES','ACTIVE_TIME_NS'
    )),
    subkey TEXT NOT NULL CHECK (subkey <> '' AND length(subkey) <= 128),
    upper_bound INTEGER NOT NULL CHECK (upper_bound > 0),
    settled_value INTEGER CHECK (settled_value IS NULL OR (settled_value >= 0 AND settled_value <= upper_bound)),
    state TEXT NOT NULL CHECK (state IN ('RESERVED','SETTLED','RELEASED')),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    settled_at TEXT CHECK (settled_at IS NULL OR (length(settled_at) = 30 AND julianday(settled_at) IS NOT NULL AND settled_at >= created_at)),
    UNIQUE (attempt_call_id, dimension, subkey),
    UNIQUE (reservation_id, run_id, dimension, subkey),
    FOREIGN KEY (run_id, dimension) REFERENCES _m4_budget_accounts(run_id, dimension) ON DELETE RESTRICT,
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
        (physical_kind = 'SIMILARITY_REQUEST' AND dimension IN ('SIMILARITY_CALLS','SIMILARITY_COST_MICRO_USD','EXTERNAL_COST_MICRO_USD')) OR
        (physical_kind = 'DOCKER_CONTAINER_CREATE' AND dimension = 'DOCKER_CONTAINER_CREATES') OR
        (physical_kind = 'LOCAL_ARTIFACT_WRITE' AND dimension = 'ARTIFACT_PHYSICAL_NEW_BYTES')
    )
) STRICT;

INSERT INTO _m4_budget_reservations(
    reservation_id, run_id, stage_name, attempt_id, call_record_id, attempt_call_id,
    physical_kind, dimension, subkey, upper_bound, settled_value, state, created_at, settled_at
)
SELECT reservation_id, run_id, stage_name, attempt_id, call_record_id, attempt_call_id,
    physical_kind, dimension, subkey, upper_bound, settled_value, state, created_at, settled_at
FROM budget_reservations;

DROP TABLE budget_reservations;
DROP TABLE budget_accounts;
ALTER TABLE _m4_budget_accounts RENAME TO budget_accounts;
ALTER TABLE _m4_budget_reservations RENAME TO budget_reservations;

CREATE INDEX budget_reservations_account ON budget_reservations(run_id, dimension, state);
CREATE UNIQUE INDEX physical_calls_record_retry_ordinal ON physical_calls(call_record_id, retry_ordinal);

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

CREATE TRIGGER budget_accounts_limit_binding_insert
BEFORE INSERT ON budget_accounts
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM runs
        WHERE runs.run_id = NEW.run_id
          AND runs.submitted_request_digest = NEW.request_snapshot_digest
          AND NEW.limit_value = CASE NEW.dimension
              WHEN 'LLM_CALLS' THEN runs.max_llm_calls
              WHEN 'LLM_INPUT_TOKENS' THEN runs.max_llm_input_tokens
              WHEN 'LLM_OUTPUT_TOKENS' THEN runs.max_llm_output_tokens
              WHEN 'EXTERNAL_COST_MICRO_USD' THEN runs.max_llm_cost_micro_usd
              WHEN 'SIMILARITY_CALLS' THEN runs.max_similarity_calls
              WHEN 'SIMILARITY_COST_MICRO_USD' THEN runs.max_similarity_cost_micro_usd
              WHEN 'DOCKER_CONTAINER_CREATES' THEN runs.max_sandbox_creates
              WHEN 'ARTIFACT_PHYSICAL_NEW_BYTES' THEN runs.max_artifact_bytes
              WHEN 'ACTIVE_TIME_NS' THEN runs.max_active_time_ns
          END
    ) THEN RAISE(ABORT, 'budget account limit is not bound to its run dimension') END;
END;

CREATE TRIGGER budget_accounts_limit_binding_update
BEFORE UPDATE ON budget_accounts
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM runs
        WHERE runs.run_id = NEW.run_id
          AND runs.submitted_request_digest = NEW.request_snapshot_digest
          AND NEW.limit_value = CASE NEW.dimension
              WHEN 'LLM_CALLS' THEN runs.max_llm_calls
              WHEN 'LLM_INPUT_TOKENS' THEN runs.max_llm_input_tokens
              WHEN 'LLM_OUTPUT_TOKENS' THEN runs.max_llm_output_tokens
              WHEN 'EXTERNAL_COST_MICRO_USD' THEN runs.max_llm_cost_micro_usd
              WHEN 'SIMILARITY_CALLS' THEN runs.max_similarity_calls
              WHEN 'SIMILARITY_COST_MICRO_USD' THEN runs.max_similarity_cost_micro_usd
              WHEN 'DOCKER_CONTAINER_CREATES' THEN runs.max_sandbox_creates
              WHEN 'ARTIFACT_PHYSICAL_NEW_BYTES' THEN runs.max_artifact_bytes
              WHEN 'ACTIVE_TIME_NS' THEN runs.max_active_time_ns
          END
    ) THEN RAISE(ABORT, 'budget account limit is not bound to its run dimension') END;
END;

CREATE TRIGGER budget_accounts_no_delete
BEFORE DELETE ON budget_accounts
BEGIN
    SELECT RAISE(ABORT, 'budget accounts cannot be deleted');
END;

CREATE TRIGGER budget_reservations_new_kind_binding
BEFORE INSERT ON budget_reservations
BEGIN
    SELECT CASE WHEN NOT (
        (NEW.physical_kind = 'LLM_REQUEST' AND NEW.dimension IN ('LLM_CALLS','LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS','EXTERNAL_COST_MICRO_USD')) OR
        (NEW.physical_kind = 'SIMILARITY_REQUEST' AND NEW.dimension IN ('SIMILARITY_CALLS','SIMILARITY_COST_MICRO_USD')) OR
        (NEW.physical_kind = 'DOCKER_CONTAINER_CREATE' AND NEW.dimension = 'DOCKER_CONTAINER_CREATES') OR
        (NEW.physical_kind = 'LOCAL_ARTIFACT_WRITE' AND NEW.dimension = 'ARTIFACT_PHYSICAL_NEW_BYTES')
    ) THEN RAISE(ABORT, 'reservation dimension does not match physical kind') END;
END;

CREATE TRIGGER budget_reservations_identity_binding_update
BEFORE UPDATE OF run_id, stage_name, attempt_id, call_record_id, attempt_call_id, physical_kind, dimension, subkey
ON budget_reservations
BEGIN
    SELECT CASE WHEN NOT (
        (NEW.physical_kind = 'LLM_REQUEST' AND NEW.dimension IN ('LLM_CALLS','LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS','EXTERNAL_COST_MICRO_USD')) OR
        (NEW.physical_kind = 'SIMILARITY_REQUEST' AND NEW.dimension IN ('SIMILARITY_CALLS','SIMILARITY_COST_MICRO_USD')) OR
        (NEW.physical_kind = 'DOCKER_CONTAINER_CREATE' AND NEW.dimension = 'DOCKER_CONTAINER_CREATES') OR
        (NEW.physical_kind = 'LOCAL_ARTIFACT_WRITE' AND NEW.dimension = 'ARTIFACT_PHYSICAL_NEW_BYTES')
    ) THEN RAISE(ABORT, 'reservation dimension does not match physical kind') END;
END;

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
