-- Migration 8 closes the artifact recovery and scope gaps without rewriting
-- migrations 1-7.  Token ids are the deterministic private staging identity;
-- the forward trigger below makes the complete reservation binding explicit.

CREATE TRIGGER artifact_occurrence_new_write_reservation_scope_insert
BEFORE INSERT ON artifact_occurrences
WHEN NEW.kind = 'NEW_WRITE'
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1
        FROM artifact_declarations declaration
        JOIN budget_reservations reservation ON reservation.reservation_id = NEW.reservation_id
        WHERE declaration.declaration_id = NEW.declaration_id
          AND declaration.run_id = NEW.run_id
          AND declaration.stage_name = NEW.stage_name
          AND declaration.attempt_id = NEW.attempt_id
          AND declaration.call_record_id = NEW.current_call_record_id
          AND declaration.physical_kind = reservation.physical_kind
          AND reservation.reservation_id = declaration.reservation_id
          AND reservation.run_id = NEW.run_id
          AND reservation.stage_name = NEW.stage_name
          AND reservation.attempt_id = NEW.attempt_id
          AND reservation.call_record_id = NEW.current_call_record_id
          AND reservation.attempt_call_id = declaration.attempt_call_id
          AND reservation.physical_kind = 'LOCAL_ARTIFACT_WRITE'
          AND reservation.dimension = declaration.reservation_dimension
          AND reservation.subkey = declaration.reservation_subkey
    ) THEN RAISE(ABORT, 'NEW_WRITE reservation scope binding does not match') END;
END;

-- M7 introduced the physical byte projection with a compatibility DEFAULT.
-- Replace that placeholder using the historical first pin for each canonical
-- blob.  Pin creation order is the authoritative publication ownership
-- evidence preserved by the ledger; later pins are cache/reuse references.
DROP TRIGGER artifact_pin_physical_bytes_immutable;
WITH historical_owners AS (
    SELECT pin_id, size,
           ROW_NUMBER() OVER (PARTITION BY digest, size ORDER BY created_at, pin_id) AS owner_ordinal
    FROM blob_pins
)
UPDATE blob_pins
SET physical_new_bytes = COALESCE((
    SELECT CASE WHEN owner_ordinal = 1 THEN size ELSE 0 END
    FROM historical_owners
    WHERE historical_owners.pin_id = blob_pins.pin_id
), 0);
CREATE TRIGGER artifact_pin_physical_bytes_immutable
BEFORE UPDATE OF physical_new_bytes ON blob_pins
WHEN NEW.physical_new_bytes <> OLD.physical_new_bytes
BEGIN SELECT RAISE(ABORT, 'blob pin physical byte count is immutable'); END;
