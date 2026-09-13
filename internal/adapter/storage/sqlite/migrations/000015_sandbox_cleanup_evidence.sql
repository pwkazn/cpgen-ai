-- Migration 15 binds cleanup transitions to independently observed evidence.
-- M1-M14 are historical bytes and must remain unchanged.
ALTER TABLE sandbox_resources ADD COLUMN stop_proof_digest TEXT
    CHECK (stop_proof_digest IS NULL OR length(stop_proof_digest) = 71);
ALTER TABLE sandbox_resources ADD COLUMN stop_proof_kind TEXT;
ALTER TABLE sandbox_resources ADD COLUMN stop_proof_at TEXT
    CHECK (stop_proof_at IS NULL OR (length(stop_proof_at) = 30 AND julianday(stop_proof_at) IS NOT NULL));
ALTER TABLE sandbox_resources ADD COLUMN cleanup_evidence_digest TEXT
    CHECK (cleanup_evidence_digest IS NULL OR length(cleanup_evidence_digest) = 71);
ALTER TABLE sandbox_executions ADD COLUMN reconciliation_evidence_digest TEXT
    CHECK (reconciliation_evidence_digest IS NULL OR length(reconciliation_evidence_digest) = 71);

CREATE UNIQUE INDEX sandbox_precreate_ack_idempotency
    ON sandbox_precreate_acks(sandbox_execution_id, idempotency_key);

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

CREATE TRIGGER sandbox_execution_reconciliation_evidence_immutable
BEFORE UPDATE OF reconciliation_evidence_digest ON sandbox_executions
WHEN COALESCE(NEW.reconciliation_evidence_digest, '') <> COALESCE(OLD.reconciliation_evidence_digest, '') AND
    OLD.reconciliation_evidence_digest IS NOT NULL
BEGIN
    SELECT RAISE(ABORT, 'sandbox reconciliation evidence is immutable');
END;
