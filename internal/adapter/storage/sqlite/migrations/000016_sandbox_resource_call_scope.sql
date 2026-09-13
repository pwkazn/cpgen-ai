-- Migration 16 binds every externally-created sandbox resource to the exact
-- physical call and execution scope that authorized it. M1-M15 are historical
-- bytes and must remain unchanged.
--
-- SQLite cannot add a composite foreign key or remove the old non-container
-- CHECK in place, so rebuild the two mutually-referencing sandbox tables while
-- retaining all rows and cleanup evidence.
DROP TRIGGER IF EXISTS sandbox_resource_phase_monotone;
DROP TRIGGER IF EXISTS sandbox_resource_version_guard;
DROP TRIGGER IF EXISTS sandbox_precreate_ack_identity_guard;
DROP TRIGGER IF EXISTS sandbox_resource_identity_immutable;
DROP TRIGGER IF EXISTS sandbox_precreate_ack_immutable;
DROP TRIGGER IF EXISTS sandbox_precreate_ack_no_delete;
DROP TRIGGER IF EXISTS sandbox_resource_cleanup_evidence_immutable;
DROP TRIGGER IF EXISTS sandbox_resource_engine_labels_immutable;
DROP TRIGGER IF EXISTS sandbox_resource_stopped_requires_proof;
DROP TRIGGER IF EXISTS sandbox_resource_cleaned_requires_evidence;

DROP INDEX IF EXISTS sandbox_precreate_ack_idempotency;

ALTER TABLE sandbox_precreate_acks RENAME TO sandbox_precreate_acks_v15;
ALTER TABLE sandbox_resources RENAME TO sandbox_resources_v15;

CREATE UNIQUE INDEX physical_calls_sandbox_scope
    ON physical_calls(attempt_call_id, run_id, stage_name, attempt_id);
CREATE UNIQUE INDEX sandbox_executions_scope
    ON sandbox_executions(sandbox_execution_id, run_id, stage_name, attempt_id);

CREATE TABLE sandbox_resources (
    resource_id TEXT PRIMARY KEY CHECK (resource_id GLOB 'resource_[0-9a-f]*'),
    sandbox_execution_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
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
    stop_proof_digest TEXT CHECK (stop_proof_digest IS NULL OR length(stop_proof_digest) = 71),
    stop_proof_kind TEXT,
    stop_proof_at TEXT CHECK (stop_proof_at IS NULL OR (length(stop_proof_at) = 30 AND julianday(stop_proof_at) IS NOT NULL)),
    cleanup_evidence_digest TEXT CHECK (cleanup_evidence_digest IS NULL OR length(cleanup_evidence_digest) = 71),
    UNIQUE (sandbox_execution_id, plan_ordinal),
    UNIQUE (sandbox_execution_id, deterministic_name),
    FOREIGN KEY (sandbox_execution_id, run_id, stage_name, attempt_id)
        REFERENCES sandbox_executions(sandbox_execution_id, run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    FOREIGN KEY (physical_call_id, run_id, stage_name, attempt_id)
        REFERENCES physical_calls(attempt_call_id, run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    CHECK ((phase IN ('COMPLETED','STARTED','STOPPED','CLEANED') AND engine_resource_id IS NOT NULL) OR phase NOT IN ('COMPLETED','STARTED','STOPPED','CLEANED')),
    -- CREATING may be entered by a legacy caller before it has attached its
    -- optional call claim. The external boundary starts at DISPATCHING, and
    -- every state from there onward requires the exact scoped call identity.
    CHECK (physical_call_id IS NOT NULL OR phase IN ('PLANNED','CREATING','INTERRUPTED'))
) STRICT;

INSERT INTO sandbox_resources(
    resource_id, sandbox_execution_id, run_id, stage_name, attempt_id, plan_ordinal,
    resource_kind, resource_role, physical_call_id, deterministic_name,
    expected_labels_digest, labels_digest, engine_resource_id, engine_identity_digest,
    cgroup_identity_digest, creation_nonce, last_idempotency_key, last_command_digest,
    phase, version, created_at, updated_at, stop_proof_digest, stop_proof_kind,
    stop_proof_at, cleanup_evidence_digest
)
SELECT resource.resource_id, resource.sandbox_execution_id, execution.run_id,
       execution.stage_name, execution.attempt_id, resource.plan_ordinal,
       resource.resource_kind, resource.resource_role, resource.physical_call_id,
       resource.deterministic_name, resource.expected_labels_digest,
       resource.labels_digest, resource.engine_resource_id,
       resource.engine_identity_digest, resource.cgroup_identity_digest,
       resource.creation_nonce, resource.last_idempotency_key,
       resource.last_command_digest, resource.phase, resource.version,
       resource.created_at, resource.updated_at, resource.stop_proof_digest,
       resource.stop_proof_kind, resource.stop_proof_at,
       resource.cleanup_evidence_digest
FROM sandbox_resources_v15 AS resource
JOIN sandbox_executions AS execution
  ON execution.sandbox_execution_id = resource.sandbox_execution_id;

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

INSERT INTO sandbox_precreate_acks(
    sandbox_execution_id, resource_id, resource_version, labels_digest,
    watchdog_record_ref, idempotency_key, acked_at
)
SELECT sandbox_execution_id, resource_id, resource_version, labels_digest,
       watchdog_record_ref, idempotency_key, acked_at
FROM sandbox_precreate_acks_v15;

DROP TABLE sandbox_precreate_acks_v15;
DROP TABLE sandbox_resources_v15;

CREATE INDEX sandbox_resources_cleanup ON sandbox_resources(sandbox_execution_id, phase);
CREATE UNIQUE INDEX sandbox_precreate_ack_idempotency
    ON sandbox_precreate_acks(sandbox_execution_id, idempotency_key);

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

CREATE TRIGGER sandbox_resource_identity_immutable
BEFORE UPDATE OF sandbox_execution_id, run_id, stage_name, attempt_id,
    plan_ordinal, resource_kind, resource_role, physical_call_id,
    deterministic_name, expected_labels_digest, creation_nonce,
    engine_identity_digest, cgroup_identity_digest ON sandbox_resources
WHEN NEW.sandbox_execution_id <> OLD.sandbox_execution_id OR
    NEW.run_id <> OLD.run_id OR NEW.stage_name <> OLD.stage_name OR
    NEW.attempt_id <> OLD.attempt_id OR NEW.plan_ordinal <> OLD.plan_ordinal OR
    NEW.resource_kind <> OLD.resource_kind OR NEW.resource_role <> OLD.resource_role OR
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

CREATE TRIGGER sandbox_resource_cleanup_evidence_immutable
BEFORE UPDATE OF stop_proof_digest, stop_proof_kind, stop_proof_at,
    cleanup_evidence_digest ON sandbox_resources
WHEN (OLD.stop_proof_digest IS NOT NULL AND COALESCE(NEW.stop_proof_digest, '') <> OLD.stop_proof_digest) OR
    (OLD.stop_proof_kind IS NOT NULL AND COALESCE(NEW.stop_proof_kind, '') <> OLD.stop_proof_kind) OR
    (OLD.stop_proof_at IS NOT NULL AND COALESCE(NEW.stop_proof_at, '') <> OLD.stop_proof_at) OR
    (OLD.cleanup_evidence_digest IS NOT NULL AND COALESCE(NEW.cleanup_evidence_digest, '') <> OLD.cleanup_evidence_digest)
BEGIN
    SELECT RAISE(ABORT, 'sandbox cleanup evidence is immutable');
END;

CREATE TRIGGER sandbox_resource_engine_labels_immutable
BEFORE UPDATE OF engine_resource_id, labels_digest ON sandbox_resources
WHEN (OLD.engine_resource_id IS NOT NULL AND COALESCE(NEW.engine_resource_id, '') <> OLD.engine_resource_id) OR
    (OLD.labels_digest IS NOT NULL AND COALESCE(NEW.labels_digest, '') <> OLD.labels_digest)
BEGIN
    SELECT RAISE(ABORT, 'sandbox resource engine or labels identity is immutable');
END;

CREATE TRIGGER sandbox_resource_stopped_requires_proof
BEFORE UPDATE OF phase ON sandbox_resources
WHEN NEW.phase IN ('STOPPED','CLEANED','INTERRUPTED') AND
    NEW.phase <> OLD.phase AND
    (NEW.stop_proof_digest IS NULL OR NEW.stop_proof_kind IS NULL OR NEW.stop_proof_at IS NULL)
BEGIN
    SELECT RAISE(ABORT, 'sandbox cleanup transition requires persisted stop proof');
END;

CREATE TRIGGER sandbox_resource_cleaned_requires_evidence
BEFORE UPDATE OF phase ON sandbox_resources
WHEN NEW.phase = 'CLEANED' AND NEW.phase <> OLD.phase AND
    NEW.cleanup_evidence_digest IS NULL
BEGIN
    SELECT RAISE(ABORT, 'sandbox CLEANED transition requires persisted cleanup evidence');
END;
