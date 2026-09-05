-- Migration 13 repairs M12's same-digest backfill.  The cache blob
-- reference role is the distinguishing identity when several output Blobs
-- share bytes; use the role-bearing source occurrence instead of the first
-- occurrence found for the digest.

-- M12 made cache blob order immutable after its backfill.  Temporarily relax
-- only that trigger so this forward migration can repair historical rows;
-- the trigger is restored before the migration commits.
DROP TRIGGER cache_blob_order_identity_immutable;

UPDATE cache_blob_order AS order_rows
SET source_occurrence_id = (
    SELECT source.source_occurrence_id
    FROM cache_entry_sources source
    JOIN artifact_occurrences occurrence
      ON occurrence.occurrence_id = source.source_occurrence_id
    WHERE source.cache_key_digest = order_rows.cache_key_digest
      AND source.source_digest = order_rows.digest
      AND occurrence.digest = order_rows.digest
      AND occurrence.size = order_rows.size
      AND occurrence.role = order_rows.role
    ORDER BY source.source_occurrence_id
    LIMIT 1
)
WHERE EXISTS (
    SELECT 1
    FROM cache_entry_sources source
    JOIN artifact_occurrences occurrence
      ON occurrence.occurrence_id = source.source_occurrence_id
    WHERE source.cache_key_digest = order_rows.cache_key_digest
      AND source.source_digest = order_rows.digest
      AND occurrence.digest = order_rows.digest
      AND occurrence.size = order_rows.size
      AND occurrence.role = order_rows.role
);

CREATE TRIGGER cache_blob_order_identity_immutable
BEFORE UPDATE ON cache_blob_order
BEGIN SELECT RAISE(ABORT, 'cache blob order is immutable'); END;

CREATE TRIGGER cache_blob_order_source_role_guard
BEFORE INSERT ON cache_blob_order
WHEN NEW.source_occurrence_id IS NOT NULL
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1
        FROM cache_entry_sources source
        JOIN artifact_occurrences occurrence
          ON occurrence.occurrence_id = source.source_occurrence_id
        WHERE source.cache_key_digest = NEW.cache_key_digest
          AND source.source_occurrence_id = NEW.source_occurrence_id
          AND source.source_digest = NEW.digest
          AND occurrence.digest = NEW.digest
          AND occurrence.size = NEW.size
          AND occurrence.role = NEW.role
    ) THEN RAISE(ABORT, 'cache blob order source role does not match') END;
END;
