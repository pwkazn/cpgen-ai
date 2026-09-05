-- Migration 7 hardens the artifact ledger without rewriting migration 6.
-- Every guard below repeats the full run/stage/attempt/call binding at the
-- boundary where artifact rows are attached; the old single-column FKs remain
-- for historical compatibility and these triggers make direct SQL forgery
-- fail closed.

ALTER TABLE blob_pins ADD COLUMN physical_new_bytes INTEGER NOT NULL DEFAULT 0 CHECK (physical_new_bytes >= 0);

CREATE TRIGGER artifact_occurrence_new_write_scope_insert
BEFORE INSERT ON artifact_occurrences
WHEN NEW.kind = 'NEW_WRITE'
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1
        FROM artifact_declarations declaration
        JOIN artifact_writer_tokens token ON token.writer_token_id = NEW.writer_token_id
        JOIN blob_pins pin ON pin.pin_id = NEW.pin_id
        JOIN budget_reservations reservation ON reservation.reservation_id = NEW.reservation_id
        WHERE declaration.declaration_id = NEW.declaration_id
          AND declaration.run_id = NEW.run_id
          AND declaration.stage_name = NEW.stage_name
          AND declaration.attempt_id = NEW.attempt_id
          AND declaration.call_record_id = NEW.current_call_record_id
          AND declaration.reservation_id = NEW.reservation_id
          AND declaration.reservation_dimension = 'ARTIFACT_PHYSICAL_NEW_BYTES'
          AND declaration.reservation_subkey = reservation.subkey
          AND reservation.run_id = NEW.run_id
          AND reservation.dimension = declaration.reservation_dimension
          AND token.declaration_id = declaration.declaration_id
          AND token.run_id = NEW.run_id
          AND pin.writer_token_id = token.writer_token_id
          AND pin.digest = NEW.digest
          AND pin.size = NEW.size
    ) THEN RAISE(ABORT, 'NEW_WRITE artifact scope binding does not match') END;
END;

CREATE TRIGGER artifact_occurrence_cache_scope_insert
BEFORE INSERT ON artifact_occurrences
WHEN NEW.kind = 'CACHE_REUSE'
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1
        FROM cache_reuse_records reuse
        JOIN call_records current_call ON current_call.call_record_id = NEW.current_call_record_id
        JOIN call_records source_call ON source_call.call_record_id = NEW.source_call_record_id
        JOIN artifact_occurrences source ON source.occurrence_id = NEW.source_occurrence_id
        WHERE reuse.cache_reuse_record_id = NEW.cache_reuse_record_id
          AND reuse.run_id = NEW.run_id
          AND reuse.stage_name = NEW.stage_name
          AND reuse.attempt_id = NEW.attempt_id
          AND reuse.current_call_record_id = NEW.current_call_record_id
          AND reuse.source_call_record_id = NEW.source_call_record_id
          AND reuse.source_occurrence_id = NEW.source_occurrence_id
          AND reuse.digest = NEW.digest
          AND reuse.size = NEW.size
          AND current_call.run_id = NEW.run_id
          AND current_call.stage_name = NEW.stage_name
          AND current_call.attempt_id = NEW.attempt_id
          AND source_call.run_id = source.run_id
          AND source.current_call_record_id = source_call.call_record_id
          AND source.run_id = reuse.run_id
          AND source.digest = NEW.digest
          AND source.size = NEW.size
    ) THEN RAISE(ABORT, 'CACHE_REUSE artifact scope binding does not match') END;
END;

CREATE TRIGGER cache_reuse_scope_insert
BEFORE INSERT ON cache_reuse_records
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1
        FROM call_records current_call
        JOIN artifact_occurrences source ON source.occurrence_id = NEW.source_occurrence_id
        JOIN call_records source_call ON source_call.call_record_id = NEW.source_call_record_id
        WHERE current_call.call_record_id = NEW.current_call_record_id
          AND current_call.run_id = NEW.run_id
          AND current_call.stage_name = NEW.stage_name
          AND current_call.attempt_id = NEW.attempt_id
          AND source.run_id = NEW.run_id
          AND source.current_call_record_id = source_call.call_record_id
          AND source_call.run_id = NEW.run_id
          AND source.digest = NEW.digest
          AND source.size = NEW.size
    ) THEN RAISE(ABORT, 'cache reuse scope binding does not match') END;
END;

CREATE TRIGGER artifact_pin_physical_bytes_immutable
BEFORE UPDATE OF physical_new_bytes ON blob_pins
WHEN NEW.physical_new_bytes <> OLD.physical_new_bytes
BEGIN SELECT RAISE(ABORT, 'blob pin physical byte count is immutable'); END;
