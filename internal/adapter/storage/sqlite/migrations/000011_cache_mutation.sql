-- Migration 11 adds CPGen cache provenance and mutation accounting.  The
-- earlier migrations are historical bytes and remain immutable.

-- M6's public Blob state is the publication/quarantine state machine.  Keep
-- that historical check closed and add the orthogonal maintenance marker for
-- GC's DELETING phase instead of rewriting a parent table with live FKs.
ALTER TABLE blobs ADD COLUMN gc_state TEXT NOT NULL DEFAULT 'NONE' CHECK (gc_state IN ('NONE','DELETING'));

CREATE TRIGGER blobs_gc_state_transition
BEFORE UPDATE OF gc_state ON blobs
WHEN NOT ((OLD.gc_state = 'NONE' AND NEW.gc_state IN ('NONE','DELETING')) OR
          (OLD.gc_state = 'DELETING' AND NEW.gc_state IN ('NONE','DELETING')))
BEGIN SELECT RAISE(ABORT, 'invalid blob GC state transition'); END;

CREATE TABLE cache_entries (
    cache_key_digest TEXT PRIMARY KEY CHECK (length(cache_key_digest) = 71),
    kind TEXT NOT NULL CHECK (kind <> '' AND length(kind) <= 128),
    schema_version TEXT NOT NULL CHECK (schema_version <> ''),
    policy_digest TEXT NOT NULL CHECK (length(policy_digest) = 71),
    input_digest TEXT NOT NULL CHECK (length(input_digest) = 71),
    state TEXT NOT NULL CHECK (state IN ('VALID','INVALIDATED')),
    expires_at TEXT CHECK (expires_at IS NULL OR (length(expires_at) = 30 AND julianday(expires_at) IS NOT NULL)),
    invalidation_cause TEXT CHECK (invalidation_cause IS NULL OR invalidation_cause IN ('CORRUPT_BLOB','MISSING_BLOB','EXPIRED','POLICY_CHANGED','MANUAL')),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    CHECK ((state = 'VALID' AND invalidation_cause IS NULL) OR (state = 'INVALIDATED' AND invalidation_cause IS NOT NULL))
) STRICT;

CREATE TABLE cache_entry_sources (
    cache_key_digest TEXT NOT NULL,
    source_run_id TEXT NOT NULL,
    source_call_record_id TEXT NOT NULL,
    source_occurrence_id TEXT NOT NULL,
    source_digest TEXT NOT NULL CHECK (length(source_digest) = 71),
    PRIMARY KEY (cache_key_digest, source_occurrence_id),
    FOREIGN KEY (cache_key_digest) REFERENCES cache_entries(cache_key_digest) ON DELETE RESTRICT,
    FOREIGN KEY (source_call_record_id, source_run_id) REFERENCES call_records(call_record_id, run_id) ON DELETE RESTRICT,
    FOREIGN KEY (source_occurrence_id) REFERENCES artifact_occurrences(occurrence_id) ON DELETE RESTRICT
    -- The digest/occurrence pair is checked by the trigger below.  The
    -- historical occurrence table does not expose that pair as a UNIQUE key.
) STRICT;

CREATE TABLE cache_blob_refs (
    cache_key_digest TEXT NOT NULL,
    digest TEXT NOT NULL CHECK (length(digest) = 71),
    size INTEGER NOT NULL CHECK (size >= 0),
    role TEXT NOT NULL CHECK (role IN ('SOURCE','PROGRAM','INPUT','OUTPUT','STDOUT','STDERR','COMPILE_LOG','EXECUTION_LOG','EVIDENCE')),
    media_type TEXT NOT NULL CHECK (media_type <> ''),
    logical_path TEXT NOT NULL CHECK (logical_path <> '' AND logical_path NOT LIKE '/%' AND logical_path NOT LIKE '%..%' AND logical_path NOT LIKE '%\%'),
    provenance_json BLOB NOT NULL CHECK (length(provenance_json) > 0 AND json_valid(CAST(provenance_json AS TEXT))),
    provenance_digest TEXT NOT NULL CHECK (length(provenance_digest) = 71),
    PRIMARY KEY (cache_key_digest, digest, size, role),
    FOREIGN KEY (cache_key_digest) REFERENCES cache_entries(cache_key_digest) ON DELETE RESTRICT,
    FOREIGN KEY (digest, size) REFERENCES blobs(digest, size) ON DELETE RESTRICT
) STRICT;

CREATE TRIGGER cache_entry_sources_insert_guard
BEFORE INSERT ON cache_entry_sources
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM cache_entries entry
        JOIN artifact_occurrences occurrence ON occurrence.occurrence_id = NEW.source_occurrence_id
        JOIN call_records call ON call.call_record_id = NEW.source_call_record_id
        WHERE entry.cache_key_digest = NEW.cache_key_digest
          AND occurrence.current_call_record_id = NEW.source_call_record_id
          AND occurrence.run_id = NEW.source_run_id
          AND call.run_id = NEW.source_run_id
          AND occurrence.digest = NEW.source_digest
          AND occurrence.kind IN ('NEW_WRITE','CACHE_REUSE')
    ) THEN RAISE(ABORT, 'cache source provenance does not match') END;
END;

CREATE TRIGGER cache_blob_refs_insert_guard
BEFORE INSERT ON cache_blob_refs
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM blobs WHERE digest = NEW.digest AND size = NEW.size AND state = 'READY'
    ) THEN RAISE(ABORT, 'cache blob reference requires READY blob') END;
END;

CREATE TRIGGER cache_entry_identity_immutable
BEFORE UPDATE ON cache_entries
WHEN NEW.cache_key_digest <> OLD.cache_key_digest OR NEW.kind <> OLD.kind OR NEW.schema_version <> OLD.schema_version
  OR NEW.policy_digest <> OLD.policy_digest OR NEW.input_digest <> OLD.input_digest OR NEW.created_at <> OLD.created_at
BEGIN SELECT RAISE(ABORT, 'cache entry identity is immutable'); END;

CREATE TRIGGER cache_entry_sources_immutable
BEFORE UPDATE ON cache_entry_sources
BEGIN SELECT RAISE(ABORT, 'cache entry source is immutable'); END;

CREATE TRIGGER cache_blob_refs_immutable
BEFORE UPDATE ON cache_blob_refs
BEGIN SELECT RAISE(ABORT, 'cache blob reference is immutable'); END;

CREATE TRIGGER cache_reuse_exact_entry_guard
BEFORE INSERT ON cache_reuse_records
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1
        FROM cache_entries entry
        JOIN cache_entry_sources source ON source.cache_key_digest = entry.cache_key_digest
            AND source.source_call_record_id = NEW.source_call_record_id
            AND source.source_occurrence_id = NEW.source_occurrence_id
        JOIN cache_blob_refs blob ON blob.cache_key_digest = entry.cache_key_digest
            AND blob.digest = NEW.digest AND blob.size = NEW.size
        WHERE entry.cache_key_digest = NEW.cache_key_digest
          AND entry.state = 'VALID'
    ) THEN RAISE(ABORT, 'cache reuse is not bound to the exact cache entry source/blob') END;
END;

CREATE TABLE mutation_accounts (
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    kind TEXT NOT NULL CHECK (kind IN ('CONTENT','METADATA')),
    limit_value INTEGER NOT NULL CHECK (limit_value >= 0),
    claimed_value INTEGER NOT NULL DEFAULT 0 CHECK (claimed_value >= 0),
    account_version INTEGER NOT NULL DEFAULT 1 CHECK (account_version > 0),
    PRIMARY KEY (run_id, stage_name, kind),
    FOREIGN KEY (run_id, stage_name) REFERENCES stage_records(run_id, stage_name) ON DELETE RESTRICT,
    CHECK (claimed_value <= limit_value)
) STRICT;

CREATE TABLE mutation_claims (
    claim_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    scope_digest TEXT NOT NULL CHECK (length(scope_digest) = 71),
    source_batch_digest TEXT NOT NULL CHECK (length(source_batch_digest) = 71),
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    limit_snapshot INTEGER NOT NULL CHECK (limit_snapshot >= 0),
    kind TEXT NOT NULL CHECK (kind IN ('CONTENT','METADATA')),
    intent_digest TEXT NOT NULL CHECK (length(intent_digest) = 71),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    UNIQUE (run_id, stage_name, scope_digest, source_batch_digest, ordinal, kind),
    UNIQUE (run_id, intent_digest),
    FOREIGN KEY (run_id, stage_name, kind) REFERENCES mutation_accounts(run_id, stage_name, kind) ON DELETE RESTRICT
) STRICT;

CREATE TABLE mutation_intents (
    intent_digest TEXT PRIMARY KEY CHECK (length(intent_digest) = 71),
    claim_id TEXT NOT NULL UNIQUE,
    run_id TEXT NOT NULL,
    command_digest TEXT NOT NULL CHECK (length(command_digest) = 71),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    FOREIGN KEY (claim_id) REFERENCES mutation_claims(claim_id) ON DELETE RESTRICT
) STRICT;

CREATE TABLE mutation_records (
    record_id TEXT PRIMARY KEY,
    claim_id TEXT NOT NULL UNIQUE,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    scope_digest TEXT NOT NULL CHECK (length(scope_digest) = 71),
    source_batch_digest TEXT NOT NULL CHECK (length(source_batch_digest) = 71),
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    kind TEXT NOT NULL CHECK (kind IN ('CONTENT','METADATA')),
    intent_digest TEXT NOT NULL CHECK (length(intent_digest) = 71),
    command_digest TEXT NOT NULL CHECK (length(command_digest) = 71),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    FOREIGN KEY (claim_id) REFERENCES mutation_claims(claim_id) ON DELETE RESTRICT,
    UNIQUE (record_id, run_id, stage_name),
    CHECK (ordinal > 0)
) STRICT;

CREATE TRIGGER mutation_intent_claim_guard
BEFORE INSERT ON mutation_intents
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM mutation_claims claim
        WHERE claim.claim_id = NEW.claim_id AND claim.intent_digest = NEW.intent_digest AND claim.run_id = NEW.run_id
    ) THEN RAISE(ABORT, 'mutation intent does not match claim') END;
END;

CREATE TABLE mutation_record_operations (
    record_id TEXT NOT NULL,
    operation_ordinal INTEGER NOT NULL CHECK (operation_ordinal > 0),
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    call_record_id TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    PRIMARY KEY (record_id, operation_ordinal),
    FOREIGN KEY (record_id) REFERENCES mutation_records(record_id) ON DELETE RESTRICT,
    FOREIGN KEY (call_record_id, run_id, stage_name, attempt_id) REFERENCES call_records(call_record_id, run_id, stage_name, attempt_id) ON DELETE RESTRICT
) STRICT;

CREATE TABLE mutation_record_reservations (
    record_id TEXT NOT NULL,
    reservation_ordinal INTEGER NOT NULL CHECK (reservation_ordinal > 0),
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    reservation_id TEXT NOT NULL,
    call_record_id TEXT NOT NULL,
    attempt_call_id TEXT NOT NULL,
    PRIMARY KEY (record_id, reservation_ordinal),
    FOREIGN KEY (record_id) REFERENCES mutation_records(record_id) ON DELETE RESTRICT,
    FOREIGN KEY (reservation_id) REFERENCES budget_reservations(reservation_id) ON DELETE RESTRICT
) STRICT;

CREATE TABLE mutation_record_output_occurrences (
    record_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    occurrence_id TEXT NOT NULL UNIQUE,
    FOREIGN KEY (record_id) REFERENCES mutation_records(record_id) ON DELETE RESTRICT,
    FOREIGN KEY (occurrence_id) REFERENCES artifact_occurrences(occurrence_id) ON DELETE RESTRICT
) STRICT;

CREATE TRIGGER mutation_record_identity_guard
BEFORE INSERT ON mutation_records
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM mutation_claims claim
        WHERE claim.claim_id = NEW.claim_id AND claim.run_id = NEW.run_id AND claim.stage_name = NEW.stage_name
          AND claim.scope_digest = NEW.scope_digest AND claim.source_batch_digest = NEW.source_batch_digest
          AND claim.ordinal = NEW.ordinal AND claim.kind = NEW.kind AND claim.intent_digest = NEW.intent_digest
    ) THEN RAISE(ABORT, 'mutation record identity does not match claim') END;
END;

CREATE TRIGGER mutation_record_operation_scope_guard
BEFORE INSERT ON mutation_record_operations
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM mutation_records record
        JOIN call_records call ON call.call_record_id = NEW.call_record_id
        WHERE record.record_id = NEW.record_id AND record.run_id = NEW.run_id AND record.stage_name = NEW.stage_name
          AND call.run_id = NEW.run_id AND call.stage_name = NEW.stage_name AND call.attempt_id = NEW.attempt_id
          AND call.state = 'TERMINAL'
    ) THEN RAISE(ABORT, 'mutation operation is outside record scope') END;
END;

CREATE TRIGGER mutation_record_reservation_scope_guard
BEFORE INSERT ON mutation_record_reservations
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM mutation_records record
        JOIN budget_reservations reservation ON reservation.reservation_id = NEW.reservation_id
        WHERE record.record_id = NEW.record_id AND record.run_id = NEW.run_id AND record.stage_name = NEW.stage_name
          AND reservation.run_id = NEW.run_id AND reservation.stage_name = NEW.stage_name
          AND reservation.call_record_id = NEW.call_record_id AND reservation.attempt_call_id = NEW.attempt_call_id
          AND reservation.state IN ('SETTLED','RELEASED')
          AND EXISTS (SELECT 1 FROM mutation_record_operations operation
                      WHERE operation.record_id = NEW.record_id
                        AND operation.call_record_id = NEW.call_record_id
                        AND operation.attempt_id = reservation.attempt_id)
    ) THEN RAISE(ABORT, 'mutation reservation is outside record scope') END;
END;

CREATE TRIGGER mutation_record_output_scope_guard
BEFORE INSERT ON mutation_record_output_occurrences
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM mutation_records record
        JOIN artifact_occurrences occurrence ON occurrence.occurrence_id = NEW.occurrence_id
        WHERE record.record_id = NEW.record_id AND record.run_id = NEW.run_id AND record.stage_name = NEW.stage_name
          AND occurrence.run_id = NEW.run_id AND occurrence.stage_name = NEW.stage_name
          AND EXISTS (SELECT 1 FROM mutation_record_operations operation
                      WHERE operation.record_id = NEW.record_id
                        AND operation.call_record_id = occurrence.current_call_record_id
                        AND operation.attempt_id = occurrence.attempt_id)
          AND occurrence.role = 'OUTPUT'
    ) THEN RAISE(ABORT, 'mutation output occurrence is outside record scope') END;
END;

CREATE TRIGGER mutation_record_children_immutable
BEFORE UPDATE ON mutation_record_operations
BEGIN SELECT RAISE(ABORT, 'mutation operation evidence is immutable'); END;
CREATE TRIGGER mutation_record_reservation_immutable
BEFORE UPDATE ON mutation_record_reservations
BEGIN SELECT RAISE(ABORT, 'mutation reservation evidence is immutable'); END;
CREATE TRIGGER mutation_record_output_immutable
BEFORE UPDATE ON mutation_record_output_occurrences
BEGIN SELECT RAISE(ABORT, 'mutation output evidence is immutable'); END;
