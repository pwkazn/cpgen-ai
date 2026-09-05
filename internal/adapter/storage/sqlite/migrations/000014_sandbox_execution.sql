CREATE TABLE sandbox_executions (
    sandbox_execution_id TEXT PRIMARY KEY CHECK (sandbox_execution_id GLOB 'sandbox_[0-9a-f]*'),
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    logical_operation_id TEXT NOT NULL CHECK (logical_operation_id <> '' AND length(logical_operation_id) <= 256),
    scope_digest TEXT NOT NULL CHECK (length(scope_digest) = 71),
    plan_digest TEXT NOT NULL CHECK (length(plan_digest) = 71),
    engine_identity_digest TEXT NOT NULL CHECK (length(engine_identity_digest) = 71),
    watchdog_control_ref TEXT NOT NULL CHECK (watchdog_control_ref <> ''),
    watchdog_token_digest TEXT NOT NULL CHECK (length(watchdog_token_digest) = 71),
    state TEXT NOT NULL CHECK (state IN ('PLANNED','ARMED','RUNNING','CLEANUP_PENDING','CLEANED','INTERRUPTED')),
    lifecycle_version INTEGER NOT NULL CHECK (lifecycle_version > 0),
    cleanup_version INTEGER NOT NULL DEFAULT 0 CHECK (cleanup_version >= 0),
    safety_deadline_utc TEXT NOT NULL CHECK (length(safety_deadline_utc) = 30 AND julianday(safety_deadline_utc) IS NOT NULL),
    cleanup_deadline_utc TEXT NOT NULL CHECK (length(cleanup_deadline_utc) = 30 AND julianday(cleanup_deadline_utc) IS NOT NULL AND cleanup_deadline_utc >= safety_deadline_utc),
    idempotency_key TEXT NOT NULL,
    command_digest TEXT NOT NULL CHECK (length(command_digest) = 71),
    arm_idempotency_key TEXT,
    arm_command_digest TEXT CHECK (arm_command_digest IS NULL OR length(arm_command_digest) = 71),
    last_cleanup_reason TEXT,
    last_cleanup_idempotency_key TEXT,
    last_cleanup_command_digest TEXT CHECK (last_cleanup_command_digest IS NULL OR length(last_cleanup_command_digest) = 71),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    updated_at TEXT NOT NULL CHECK (length(updated_at) = 30 AND julianday(updated_at) IS NOT NULL AND updated_at >= created_at),
    UNIQUE (run_id, sandbox_execution_id),
    UNIQUE (run_id, stage_name, attempt_id, logical_operation_id),
    FOREIGN KEY (run_id, stage_name, attempt_id) REFERENCES stage_attempts(run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    CHECK (state <> 'CLEANED' OR cleanup_version > 0)
) STRICT;

CREATE INDEX sandbox_executions_run_state ON sandbox_executions(run_id, state);

CREATE TABLE sandbox_resources (
    resource_id TEXT PRIMARY KEY CHECK (resource_id GLOB 'resource_[0-9a-f]*'),
    sandbox_execution_id TEXT NOT NULL,
    plan_ordinal INTEGER NOT NULL CHECK (plan_ordinal >= 0),
    resource_kind TEXT NOT NULL CHECK (resource_kind IN ('CONTAINER','VOLUME','CGROUP')),
    resource_role TEXT NOT NULL CHECK (resource_role IN ('IMPORT','KEEPER','TARGET','EXPORT','INPUT','OUTPUT','RELEASE_PARENT')),
    physical_call_id TEXT,
    deterministic_name TEXT NOT NULL CHECK (deterministic_name <> ''),
    expected_labels_digest TEXT NOT NULL CHECK (length(expected_labels_digest) = 71),
    labels_digest TEXT CHECK (labels_digest IS NULL OR length(labels_digest) = 71),
    engine_resource_id TEXT,
    engine_identity_digest TEXT NOT NULL CHECK (length(engine_identity_digest) = 71),
    cgroup_identity_digest TEXT CHECK (cgroup_identity_digest IS NULL OR length(cgroup_identity_digest) = 71),
    creation_nonce TEXT,
    last_idempotency_key TEXT,
    last_command_digest TEXT CHECK (last_command_digest IS NULL OR length(last_command_digest) = 71),
    phase TEXT NOT NULL CHECK (phase IN ('PLANNED','CREATING','DISPATCHING','SENT','UNKNOWN','COMPLETED','STARTED','CLEANUP_PENDING','STOPPED','CLEANED','INTERRUPTED')),
    version INTEGER NOT NULL CHECK (version > 0),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    updated_at TEXT NOT NULL CHECK (length(updated_at) = 30 AND julianday(updated_at) IS NOT NULL AND updated_at >= created_at),
    UNIQUE (sandbox_execution_id, plan_ordinal),
    UNIQUE (sandbox_execution_id, deterministic_name),
    FOREIGN KEY (sandbox_execution_id) REFERENCES sandbox_executions(sandbox_execution_id) ON DELETE RESTRICT,
    FOREIGN KEY (physical_call_id) REFERENCES physical_calls(attempt_call_id) ON DELETE RESTRICT,
    CHECK ((phase IN ('COMPLETED','STARTED','STOPPED','CLEANED') AND engine_resource_id IS NOT NULL) OR phase NOT IN ('COMPLETED','STARTED','STOPPED','CLEANED')),
    CHECK (resource_kind = 'CONTAINER' OR physical_call_id IS NULL)
) STRICT;

CREATE INDEX sandbox_resources_cleanup ON sandbox_resources(sandbox_execution_id, phase);

CREATE TABLE sandbox_watchdog_controls (
    control_id TEXT PRIMARY KEY,
    sandbox_execution_id TEXT NOT NULL UNIQUE,
    process_record_ref TEXT NOT NULL CHECK (process_record_ref <> ''),
    control_file_digest TEXT NOT NULL CHECK (length(control_file_digest) = 71),
    token_digest TEXT NOT NULL CHECK (length(token_digest) = 71),
    armed_at TEXT NOT NULL CHECK (length(armed_at) = 30 AND julianday(armed_at) IS NOT NULL),
    FOREIGN KEY (sandbox_execution_id) REFERENCES sandbox_executions(sandbox_execution_id) ON DELETE RESTRICT
) STRICT;

CREATE TABLE sandbox_precreate_acks (
    sandbox_execution_id TEXT NOT NULL,
    resource_id TEXT NOT NULL,
    resource_version INTEGER NOT NULL CHECK (resource_version > 0),
    labels_digest TEXT NOT NULL CHECK (length(labels_digest) = 71),
    watchdog_record_ref TEXT NOT NULL CHECK (watchdog_record_ref <> ''),
    idempotency_key TEXT NOT NULL,
    acked_at TEXT NOT NULL CHECK (length(acked_at) = 30 AND julianday(acked_at) IS NOT NULL),
    PRIMARY KEY (sandbox_execution_id, resource_id),
    FOREIGN KEY (sandbox_execution_id) REFERENCES sandbox_executions(sandbox_execution_id) ON DELETE RESTRICT,
    FOREIGN KEY (resource_id) REFERENCES sandbox_resources(resource_id) ON DELETE RESTRICT
) STRICT;

CREATE TRIGGER sandbox_resource_phase_monotone
BEFORE UPDATE OF phase ON sandbox_resources
WHEN NEW.phase <> OLD.phase AND NOT (
    (OLD.phase = 'PLANNED' AND NEW.phase IN ('CREATING','INTERRUPTED')) OR
    (OLD.phase = 'CREATING' AND NEW.phase IN ('DISPATCHING','UNKNOWN','INTERRUPTED','CLEANUP_PENDING')) OR
    (OLD.phase = 'DISPATCHING' AND NEW.phase IN ('SENT','UNKNOWN','INTERRUPTED','CLEANUP_PENDING')) OR
    (OLD.phase = 'SENT' AND NEW.phase IN ('COMPLETED','UNKNOWN','INTERRUPTED','CLEANUP_PENDING')) OR
    (OLD.phase = 'UNKNOWN' AND NEW.phase IN ('COMPLETED','CLEANUP_PENDING','STOPPED','INTERRUPTED')) OR
    (OLD.phase = 'COMPLETED' AND NEW.phase IN ('STARTED','CLEANUP_PENDING','STOPPED','INTERRUPTED')) OR
    (OLD.phase = 'STARTED' AND NEW.phase IN ('CLEANUP_PENDING','STOPPED','INTERRUPTED')) OR
    (OLD.phase = 'CLEANUP_PENDING' AND NEW.phase IN ('STOPPED','CLEANED','INTERRUPTED')) OR
    (OLD.phase = 'STOPPED' AND NEW.phase IN ('CLEANED','INTERRUPTED')) OR
    (OLD.phase = 'INTERRUPTED' AND NEW.phase = 'CLEANUP_PENDING')
)
BEGIN
    SELECT RAISE(ABORT, 'sandbox resource phase transition is not monotone');
END;

CREATE TRIGGER sandbox_execution_state_guard
BEFORE UPDATE OF state ON sandbox_executions
WHEN NEW.state <> OLD.state AND NOT (
    (OLD.state = 'PLANNED' AND NEW.state IN ('ARMED','CLEANUP_PENDING','INTERRUPTED')) OR
    (OLD.state = 'ARMED' AND NEW.state IN ('RUNNING','CLEANUP_PENDING','INTERRUPTED')) OR
    (OLD.state = 'RUNNING' AND NEW.state IN ('CLEANUP_PENDING','INTERRUPTED')) OR
    (OLD.state = 'CLEANUP_PENDING' AND NEW.state IN ('CLEANED','INTERRUPTED')) OR
    (OLD.state = 'INTERRUPTED' AND NEW.state = 'CLEANUP_PENDING')
)
BEGIN
    SELECT RAISE(ABORT, 'sandbox execution state transition is not permitted');
END;

CREATE TRIGGER sandbox_execution_version_guard
BEFORE UPDATE OF lifecycle_version ON sandbox_executions
WHEN NEW.lifecycle_version <= OLD.lifecycle_version
BEGIN
    SELECT RAISE(ABORT, 'sandbox execution lifecycle version must advance');
END;

CREATE TRIGGER sandbox_resource_version_guard
BEFORE UPDATE OF phase, version ON sandbox_resources
WHEN NEW.version <= OLD.version
BEGIN
    SELECT RAISE(ABORT, 'sandbox resource version must advance');
END;

CREATE TRIGGER sandbox_precreate_ack_identity_guard
BEFORE INSERT ON sandbox_precreate_acks
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM sandbox_resources resource
        WHERE resource.resource_id = NEW.resource_id
          AND resource.sandbox_execution_id = NEW.sandbox_execution_id
          AND resource.phase IN ('CREATING','DISPATCHING','SENT','UNKNOWN','COMPLETED','STARTED','CLEANUP_PENDING')
          AND resource.version = NEW.resource_version
    ) THEN RAISE(ABORT, 'sandbox pre-create ACK does not match resource identity') END;
END;

-- Once a plan is sealed, its authorization and cleanup identity are evidence,
-- not mutable ownership metadata. Lifecycle methods may only change the
-- explicitly versioned phase/state columns.
CREATE TRIGGER sandbox_execution_identity_immutable
BEFORE UPDATE OF run_id, stage_name, attempt_id, logical_operation_id,
    scope_digest, plan_digest, engine_identity_digest, watchdog_control_ref,
    watchdog_token_digest ON sandbox_executions
WHEN NEW.run_id <> OLD.run_id OR NEW.stage_name <> OLD.stage_name OR
    NEW.attempt_id <> OLD.attempt_id OR NEW.logical_operation_id <> OLD.logical_operation_id OR
    NEW.scope_digest <> OLD.scope_digest OR NEW.plan_digest <> OLD.plan_digest OR
    NEW.engine_identity_digest <> OLD.engine_identity_digest OR
    NEW.watchdog_control_ref <> OLD.watchdog_control_ref OR
    NEW.watchdog_token_digest <> OLD.watchdog_token_digest
BEGIN
    SELECT RAISE(ABORT, 'sandbox execution identity is immutable');
END;

CREATE TRIGGER sandbox_resource_identity_immutable
BEFORE UPDATE OF sandbox_execution_id, plan_ordinal, resource_kind, resource_role,
    physical_call_id, deterministic_name, expected_labels_digest,
    creation_nonce, engine_identity_digest, cgroup_identity_digest ON sandbox_resources
WHEN NEW.sandbox_execution_id <> OLD.sandbox_execution_id OR
    NEW.plan_ordinal <> OLD.plan_ordinal OR NEW.resource_kind <> OLD.resource_kind OR
    NEW.resource_role <> OLD.resource_role OR
    (COALESCE(NEW.physical_call_id, '') <> COALESCE(OLD.physical_call_id, '') AND
        NOT (OLD.physical_call_id IS NULL AND NEW.physical_call_id IS NOT NULL)) OR
    NEW.deterministic_name <> OLD.deterministic_name OR
    NEW.expected_labels_digest <> OLD.expected_labels_digest OR
    COALESCE(NEW.creation_nonce, '') <> COALESCE(OLD.creation_nonce, '') OR
    NEW.engine_identity_digest <> OLD.engine_identity_digest OR
    COALESCE(NEW.cgroup_identity_digest, '') <> COALESCE(OLD.cgroup_identity_digest, '')
BEGIN
    SELECT RAISE(ABORT, 'sandbox resource identity is immutable');
END;

CREATE TRIGGER sandbox_precreate_ack_immutable
BEFORE UPDATE ON sandbox_precreate_acks
BEGIN
    SELECT RAISE(ABORT, 'sandbox pre-create ACK is append-only');
END;

CREATE TRIGGER sandbox_precreate_ack_no_delete
BEFORE DELETE ON sandbox_precreate_acks
BEGIN
    SELECT RAISE(ABORT, 'sandbox pre-create ACK is append-only');
END;
