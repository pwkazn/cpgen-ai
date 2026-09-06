-- Migration 17 restores legal historical VOLUME rows whose M14 schema
-- allowed a NULL physical call ID in a post-dispatch phase. Migration 16 is
-- immutable, so the migration runner temporarily parks those phase values
-- while M16 rebuilds the table; this forward migration restores them under a
-- volume-specific compatibility check.
CREATE TABLE IF NOT EXISTS sandbox_m16_legacy_volume_phases (
    resource_id TEXT PRIMARY KEY,
    phase TEXT NOT NULL
) STRICT;

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

ALTER TABLE sandbox_precreate_acks RENAME TO sandbox_precreate_acks_v16;
ALTER TABLE sandbox_resources RENAME TO sandbox_resources_v16;

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
    CHECK (physical_call_id IS NOT NULL OR resource_kind = 'VOLUME' OR phase IN ('PLANNED','CREATING','INTERRUPTED'))
) STRICT;

INSERT INTO sandbox_resources(
    resource_id, sandbox_execution_id, run_id, stage_name, attempt_id, plan_ordinal,
    resource_kind, resource_role, physical_call_id, deterministic_name,
    expected_labels_digest, labels_digest, engine_resource_id, engine_identity_digest,
    cgroup_identity_digest, creation_nonce, last_idempotency_key, last_command_digest,
    phase, version, created_at, updated_at, stop_proof_digest, stop_proof_kind,
    stop_proof_at, cleanup_evidence_digest
)
SELECT resource_id, sandbox_execution_id, run_id, stage_name, attempt_id, plan_ordinal,
       resource_kind, resource_role, physical_call_id, deterministic_name,
       expected_labels_digest, labels_digest, engine_resource_id, engine_identity_digest,
       cgroup_identity_digest, creation_nonce, last_idempotency_key, last_command_digest,
       phase, version, created_at, updated_at, stop_proof_digest, stop_proof_kind,
       stop_proof_at, cleanup_evidence_digest
FROM sandbox_resources_v16;

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
FROM sandbox_precreate_acks_v16;

DROP TABLE sandbox_precreate_acks_v16;
DROP TABLE sandbox_resources_v16;

UPDATE sandbox_resources
SET phase = (
    SELECT phase
    FROM sandbox_m16_legacy_volume_phases AS legacy
    WHERE legacy.resource_id = sandbox_resources.resource_id
)
WHERE resource_id IN (SELECT resource_id FROM sandbox_m16_legacy_volume_phases);

DROP TABLE sandbox_m16_legacy_volume_phases;

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
