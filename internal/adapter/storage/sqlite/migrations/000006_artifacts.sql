-- Migration 6 is the first formal artifact ledger. It is deliberately
-- forward-only: migrations 1-5 are historical bytes and must never be edited.

CREATE TABLE artifact_declarations (
    declaration_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    call_record_id TEXT NOT NULL,
    attempt_call_id TEXT NOT NULL,
    physical_kind TEXT NOT NULL DEFAULT 'LOCAL_ARTIFACT_WRITE' CHECK (physical_kind = 'LOCAL_ARTIFACT_WRITE'),
    reservation_id TEXT NOT NULL,
    reservation_dimension TEXT NOT NULL DEFAULT 'ARTIFACT_PHYSICAL_NEW_BYTES' CHECK (reservation_dimension = 'ARTIFACT_PHYSICAL_NEW_BYTES'),
    reservation_subkey TEXT NOT NULL CHECK (reservation_subkey <> '' AND length(reservation_subkey) <= 128),
    role TEXT NOT NULL CHECK (role IN ('SOURCE','PROGRAM','INPUT','OUTPUT','STDOUT','STDERR','COMPILE_LOG','EXECUTION_LOG','EVIDENCE')),
    media_type TEXT NOT NULL CHECK (media_type <> ''),
    logical_path TEXT NOT NULL CHECK (logical_path <> '' AND logical_path NOT LIKE '/%' AND logical_path NOT LIKE '%..%' AND logical_path NOT LIKE '%\\%'),
    max_bytes INTEGER NOT NULL CHECK (max_bytes > 0),
    provenance_json BLOB NOT NULL CHECK (length(provenance_json) > 0 AND json_valid(CAST(provenance_json AS TEXT))),
    declaration_digest TEXT NOT NULL CHECK (length(declaration_digest) = 71),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    UNIQUE (run_id, declaration_id),
    UNIQUE (attempt_call_id, reservation_id),
    FOREIGN KEY (run_id, stage_name) REFERENCES stage_records(run_id, stage_name) ON DELETE RESTRICT,
    FOREIGN KEY (run_id, stage_name, attempt_id) REFERENCES stage_attempts(run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    FOREIGN KEY (attempt_call_id, call_record_id, run_id, stage_name, attempt_id, physical_kind)
        REFERENCES physical_calls(attempt_call_id, call_record_id, run_id, stage_name, attempt_id, physical_kind) ON DELETE RESTRICT,
    FOREIGN KEY (reservation_id, run_id, reservation_dimension, reservation_subkey)
        REFERENCES budget_reservations(reservation_id, run_id, dimension, subkey) ON DELETE RESTRICT
) STRICT;

CREATE TABLE blobs (
    digest TEXT NOT NULL CHECK (length(digest) = 71),
    size INTEGER NOT NULL CHECK (size >= 0),
    state TEXT NOT NULL CHECK (state IN ('STAGING','READY','QUARANTINING','CORRUPT')),
    canonical_relative_path TEXT NOT NULL CHECK (canonical_relative_path <> ''),
    verified_at TEXT CHECK (verified_at IS NULL OR (length(verified_at) = 30 AND julianday(verified_at) IS NOT NULL)),
    PRIMARY KEY (digest, size),
    UNIQUE (canonical_relative_path),
    CHECK ((state = 'READY' AND verified_at IS NOT NULL) OR (state <> 'READY'))
) STRICT;

CREATE TABLE artifact_writer_tokens (
    writer_token_id TEXT PRIMARY KEY,
    declaration_id TEXT NOT NULL,
    run_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('PREPARED','OPEN','SEALED','FINALIZED','RELEASED')),
    final_digest TEXT,
    final_size INTEGER CHECK (final_size IS NULL OR final_size >= 0),
    pin_id TEXT NOT NULL,
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    opened_at TEXT CHECK (opened_at IS NULL OR (length(opened_at) = 30 AND julianday(opened_at) IS NOT NULL AND opened_at >= created_at)),
    sealed_at TEXT CHECK (sealed_at IS NULL OR (length(sealed_at) = 30 AND julianday(sealed_at) IS NOT NULL AND sealed_at >= created_at)),
    finalized_at TEXT CHECK (finalized_at IS NULL OR (length(finalized_at) = 30 AND julianday(finalized_at) IS NOT NULL AND finalized_at >= created_at)),
    released_at TEXT CHECK (released_at IS NULL OR (length(released_at) = 30 AND julianday(released_at) IS NOT NULL AND released_at >= created_at)),
    UNIQUE (declaration_id),
    UNIQUE (pin_id),
    FOREIGN KEY (declaration_id, run_id) REFERENCES artifact_declarations(declaration_id, run_id) ON DELETE RESTRICT,
    FOREIGN KEY (final_digest, final_size) REFERENCES blobs(digest, size) ON DELETE RESTRICT,
    CHECK ((state IN ('PREPARED','OPEN') AND final_digest IS NULL AND final_size IS NULL AND finalized_at IS NULL AND released_at IS NULL) OR
           (state IN ('SEALED','FINALIZED') AND final_digest IS NOT NULL AND final_size IS NOT NULL) OR
           (state = 'RELEASED' AND ((final_digest IS NULL AND final_size IS NULL) OR (final_digest IS NOT NULL AND final_size IS NOT NULL)))),
    CHECK ((state = 'PREPARED' AND opened_at IS NULL AND sealed_at IS NULL) OR (state <> 'PREPARED' AND opened_at IS NOT NULL)),
    CHECK ((state IN ('SEALED','FINALIZED','RELEASED') AND sealed_at IS NOT NULL) OR state IN ('PREPARED','OPEN')),
    CHECK ((state = 'FINALIZED' AND finalized_at IS NOT NULL) OR state <> 'FINALIZED'),
    CHECK ((state = 'RELEASED' AND released_at IS NOT NULL) OR state <> 'RELEASED')
) STRICT;

CREATE TABLE blob_pins (
    pin_id TEXT PRIMARY KEY,
    writer_token_id TEXT NOT NULL,
    digest TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    state TEXT NOT NULL CHECK (state IN ('ACTIVE','RELEASABLE','RELEASED')),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    released_at TEXT CHECK (released_at IS NULL OR (length(released_at) = 30 AND julianday(released_at) IS NOT NULL AND released_at >= created_at)),
    UNIQUE (writer_token_id),
    FOREIGN KEY (writer_token_id) REFERENCES artifact_writer_tokens(writer_token_id) ON DELETE RESTRICT,
    FOREIGN KEY (digest, size) REFERENCES blobs(digest, size) ON DELETE RESTRICT,
    CHECK ((state = 'RELEASED' AND released_at IS NOT NULL) OR (state <> 'RELEASED' AND released_at IS NULL))
) STRICT;

CREATE TABLE blob_pin_history (
    pin_id TEXT NOT NULL,
    ordinal INTEGER NOT NULL CHECK (ordinal > 0),
    state TEXT NOT NULL CHECK (state IN ('ACTIVE','RELEASABLE','RELEASED')),
    changed_at TEXT NOT NULL CHECK (length(changed_at) = 30 AND julianday(changed_at) IS NOT NULL),
    PRIMARY KEY (pin_id, ordinal),
    FOREIGN KEY (pin_id) REFERENCES blob_pins(pin_id) ON DELETE RESTRICT
) STRICT;

CREATE TABLE cache_reuse_records (
    cache_reuse_record_id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    current_call_record_id TEXT NOT NULL,
    source_call_record_id TEXT NOT NULL,
    source_occurrence_id TEXT NOT NULL,
    digest TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    cache_key_digest TEXT NOT NULL CHECK (length(cache_key_digest) = 71),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    FOREIGN KEY (run_id, stage_name) REFERENCES stage_records(run_id, stage_name) ON DELETE RESTRICT,
    FOREIGN KEY (run_id, stage_name, attempt_id) REFERENCES stage_attempts(run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    FOREIGN KEY (current_call_record_id, run_id, stage_name, attempt_id) REFERENCES call_records(call_record_id, run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    FOREIGN KEY (source_call_record_id, run_id) REFERENCES call_records(call_record_id, run_id) ON DELETE RESTRICT,
    FOREIGN KEY (digest, size) REFERENCES blobs(digest, size) ON DELETE RESTRICT,
    UNIQUE (current_call_record_id, source_occurrence_id)
) STRICT;

CREATE TABLE artifact_occurrences (
    occurrence_id TEXT PRIMARY KEY,
    kind TEXT NOT NULL CHECK (kind IN ('NEW_WRITE','CACHE_REUSE')),
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    attempt_id TEXT NOT NULL,
    current_call_record_id TEXT NOT NULL,
    declaration_id TEXT,
    writer_token_id TEXT,
    reservation_id TEXT,
    pin_id TEXT,
    source_occurrence_id TEXT,
    cache_reuse_record_id TEXT,
    source_call_record_id TEXT,
    digest TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    role TEXT NOT NULL CHECK (role IN ('SOURCE','PROGRAM','INPUT','OUTPUT','STDOUT','STDERR','COMPILE_LOG','EXECUTION_LOG','EVIDENCE')),
    logical_path TEXT NOT NULL CHECK (logical_path <> '' AND logical_path NOT LIKE '/%' AND logical_path NOT LIKE '%..%' AND logical_path NOT LIKE '%\\%'),
    media_type TEXT NOT NULL CHECK (media_type <> ''),
    provenance_json BLOB NOT NULL CHECK (length(provenance_json) > 0 AND json_valid(CAST(provenance_json AS TEXT))),
    provenance_digest TEXT NOT NULL CHECK (length(provenance_digest) = 71),
    created_at TEXT NOT NULL CHECK (length(created_at) = 30 AND julianday(created_at) IS NOT NULL),
    FOREIGN KEY (run_id, stage_name) REFERENCES stage_records(run_id, stage_name) ON DELETE RESTRICT,
    FOREIGN KEY (run_id, stage_name, attempt_id) REFERENCES stage_attempts(run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    FOREIGN KEY (current_call_record_id, run_id, stage_name, attempt_id) REFERENCES call_records(call_record_id, run_id, stage_name, attempt_id) ON DELETE RESTRICT,
    FOREIGN KEY (declaration_id) REFERENCES artifact_declarations(declaration_id) ON DELETE RESTRICT,
    FOREIGN KEY (writer_token_id) REFERENCES artifact_writer_tokens(writer_token_id) ON DELETE RESTRICT,
    FOREIGN KEY (reservation_id) REFERENCES budget_reservations(reservation_id) ON DELETE RESTRICT,
    FOREIGN KEY (pin_id) REFERENCES blob_pins(pin_id) ON DELETE RESTRICT,
    FOREIGN KEY (source_occurrence_id) REFERENCES artifact_occurrences(occurrence_id) ON DELETE RESTRICT,
    FOREIGN KEY (cache_reuse_record_id) REFERENCES cache_reuse_records(cache_reuse_record_id) ON DELETE RESTRICT,
    FOREIGN KEY (source_call_record_id, run_id) REFERENCES call_records(call_record_id, run_id) ON DELETE RESTRICT,
    FOREIGN KEY (digest, size) REFERENCES blobs(digest, size) ON DELETE RESTRICT,
    UNIQUE (run_id, stage_name, attempt_id, logical_path),
    CHECK ((kind = 'NEW_WRITE' AND writer_token_id IS NOT NULL AND declaration_id IS NOT NULL AND reservation_id IS NOT NULL AND pin_id IS NOT NULL
            AND source_occurrence_id IS NULL AND cache_reuse_record_id IS NULL AND source_call_record_id IS NULL) OR
           (kind = 'CACHE_REUSE' AND writer_token_id IS NULL AND declaration_id IS NULL AND reservation_id IS NULL AND pin_id IS NULL
            AND source_occurrence_id IS NOT NULL AND cache_reuse_record_id IS NOT NULL AND source_call_record_id IS NOT NULL))
) STRICT;

CREATE INDEX artifact_occurrences_blob ON artifact_occurrences(digest, size);
CREATE INDEX artifact_occurrences_run_stage ON artifact_occurrences(run_id, stage_name, attempt_id);
CREATE INDEX blob_pins_state ON blob_pins(state, digest, size);

CREATE TRIGGER blobs_identity_immutable
BEFORE UPDATE ON blobs
WHEN NEW.digest <> OLD.digest OR NEW.size <> OLD.size OR NEW.canonical_relative_path <> OLD.canonical_relative_path
BEGIN SELECT RAISE(ABORT, 'blob identity is immutable'); END;

CREATE TRIGGER blobs_state_transition
BEFORE UPDATE OF state ON blobs
WHEN NOT ((OLD.state = 'STAGING' AND NEW.state IN ('STAGING','READY','QUARANTINING')) OR
          (OLD.state = 'READY' AND NEW.state IN ('READY','QUARANTINING')) OR
          (OLD.state = 'QUARANTINING' AND NEW.state = 'CORRUPT') OR
          (OLD.state = 'CORRUPT' AND NEW.state = 'CORRUPT'))
BEGIN SELECT RAISE(ABORT, 'invalid blob state transition'); END;

CREATE TRIGGER artifact_writer_identity_immutable
BEFORE UPDATE ON artifact_writer_tokens
WHEN NEW.writer_token_id <> OLD.writer_token_id OR NEW.declaration_id <> OLD.declaration_id OR NEW.run_id <> OLD.run_id OR NEW.pin_id <> OLD.pin_id
BEGIN SELECT RAISE(ABORT, 'artifact writer identity is immutable'); END;

CREATE TRIGGER artifact_writer_state_transition
BEFORE UPDATE OF state ON artifact_writer_tokens
WHEN NOT ((OLD.state = 'PREPARED' AND NEW.state IN ('OPEN','RELEASED')) OR
          (OLD.state = 'OPEN' AND NEW.state IN ('OPEN','SEALED','RELEASED')) OR
          (OLD.state = 'SEALED' AND NEW.state IN ('SEALED','FINALIZED','RELEASED')) OR
          (OLD.state = 'FINALIZED' AND NEW.state IN ('FINALIZED','RELEASED')) OR
          (OLD.state = 'RELEASED' AND NEW.state = 'RELEASED'))
BEGIN SELECT RAISE(ABORT, 'invalid artifact writer state transition'); END;

CREATE TRIGGER blob_pin_identity_immutable
BEFORE UPDATE ON blob_pins
WHEN NEW.pin_id <> OLD.pin_id OR NEW.writer_token_id <> OLD.writer_token_id OR NEW.digest <> OLD.digest OR NEW.size <> OLD.size OR NEW.created_at <> OLD.created_at
BEGIN SELECT RAISE(ABORT, 'blob pin identity is immutable'); END;

CREATE TRIGGER blob_pin_state_transition
BEFORE UPDATE OF state ON blob_pins
WHEN NOT ((OLD.state = 'ACTIVE' AND NEW.state IN ('ACTIVE','RELEASABLE','RELEASED')) OR
          (OLD.state = 'RELEASABLE' AND NEW.state IN ('RELEASABLE','RELEASED')) OR
          (OLD.state = 'RELEASED' AND NEW.state = 'RELEASED'))
BEGIN SELECT RAISE(ABORT, 'invalid blob pin state transition'); END;

CREATE TRIGGER artifact_occurrences_ready_insert
BEFORE INSERT ON artifact_occurrences
BEGIN
    SELECT CASE WHEN NOT EXISTS (SELECT 1 FROM blobs WHERE digest = NEW.digest AND size = NEW.size AND state = 'READY')
        THEN RAISE(ABORT, 'artifact occurrence requires READY blob') END;
    SELECT CASE WHEN NEW.kind = 'NEW_WRITE' AND NOT EXISTS (
        SELECT 1 FROM artifact_writer_tokens token JOIN blob_pins pin ON pin.writer_token_id = token.writer_token_id
        WHERE token.writer_token_id = NEW.writer_token_id AND token.state = 'FINALIZED' AND pin.pin_id = NEW.pin_id AND pin.state IN ('ACTIVE','RELEASABLE')
          AND token.final_digest = NEW.digest AND token.final_size = NEW.size
    ) THEN RAISE(ABORT, 'NEW_WRITE occurrence lacks finalized token and pin') END;
    SELECT CASE WHEN NEW.kind = 'CACHE_REUSE' AND NOT EXISTS (
        SELECT 1 FROM cache_reuse_records reuse WHERE reuse.cache_reuse_record_id = NEW.cache_reuse_record_id
          AND reuse.current_call_record_id = NEW.current_call_record_id AND reuse.source_occurrence_id = NEW.source_occurrence_id
          AND reuse.source_call_record_id = NEW.source_call_record_id AND reuse.digest = NEW.digest AND reuse.size = NEW.size
    ) THEN RAISE(ABORT, 'CACHE_REUSE occurrence lacks matching reuse record') END;
END;

CREATE TRIGGER cache_reuse_source_binding_insert
BEFORE INSERT ON cache_reuse_records
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM artifact_occurrences source
        WHERE source.occurrence_id = NEW.source_occurrence_id AND source.run_id = NEW.run_id
          AND source.digest = NEW.digest AND source.size = NEW.size
    ) THEN RAISE(ABORT, 'cache reuse source occurrence does not match') END;
END;

CREATE TRIGGER cache_reuse_source_binding_update
BEFORE UPDATE ON cache_reuse_records
BEGIN SELECT RAISE(ABORT, 'cache reuse records are immutable'); END;

CREATE TRIGGER artifact_declarations_immutable
BEFORE UPDATE ON artifact_declarations
BEGIN SELECT RAISE(ABORT, 'artifact declarations are immutable'); END;
CREATE TRIGGER artifact_occurrences_immutable
BEFORE UPDATE ON artifact_occurrences
BEGIN SELECT RAISE(ABORT, 'artifact occurrences are immutable'); END;
