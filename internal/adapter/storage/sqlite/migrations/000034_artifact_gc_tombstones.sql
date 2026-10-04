-- Keep released writer/pin/publication evidence after deleting physical bytes.
-- A removed blob is a non-readable STAGING tombstone, never a deleted parent
-- row. A later publication receives a fresh accounting generation.
ALTER TABLE blobs ADD COLUMN gc_removed_at TEXT
    CHECK (gc_removed_at IS NULL OR (length(gc_removed_at) = 30 AND julianday(gc_removed_at) IS NOT NULL));
ALTER TABLE blobs ADD COLUMN publication_generation INTEGER NOT NULL DEFAULT 1
    CHECK (publication_generation > 0);

-- Owners are a leaf table: rebuilding it preserves every incoming FK in the
-- ledger and does not recalculate historical physical_new_bytes or budgets.
DROP TRIGGER artifact_blob_publication_owner_immutable;
DROP TRIGGER artifact_blob_publication_owner_delete_guard;
DROP TRIGGER artifact_blob_publication_owner_binding_insert;
ALTER TABLE artifact_blob_publication_owners RENAME TO artifact_blob_publication_owners_legacy;
CREATE TABLE artifact_blob_publication_owners (
    digest TEXT NOT NULL CHECK (length(digest) = 71),
    size INTEGER NOT NULL CHECK (size >= 0),
    generation INTEGER NOT NULL DEFAULT 1 CHECK (generation > 0),
    pin_id TEXT NOT NULL UNIQUE,
    PRIMARY KEY (digest, size, generation),
    FOREIGN KEY (pin_id, digest, size)
        REFERENCES blob_pins(pin_id, digest, size) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY (digest, size)
        REFERENCES blobs(digest, size) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;
INSERT INTO artifact_blob_publication_owners(digest, size, generation, pin_id)
SELECT digest, size, 1, pin_id FROM artifact_blob_publication_owners_legacy;
DROP TABLE artifact_blob_publication_owners_legacy;
CREATE TRIGGER artifact_blob_publication_owner_immutable
BEFORE UPDATE ON artifact_blob_publication_owners
BEGIN SELECT RAISE(ABORT, 'blob publication owner is immutable'); END;
CREATE TRIGGER artifact_blob_publication_owner_delete_guard
BEFORE DELETE ON artifact_blob_publication_owners
BEGIN SELECT RAISE(ABORT, 'blob publication owners are immutable'); END;
CREATE TRIGGER artifact_blob_publication_owner_binding_insert
BEFORE INSERT ON artifact_blob_publication_owners
WHEN NOT EXISTS (
    SELECT 1 FROM blob_pins pin JOIN blobs blob ON blob.digest = pin.digest AND blob.size = pin.size
    WHERE pin.pin_id = NEW.pin_id AND pin.digest = NEW.digest AND pin.size = NEW.size
      AND pin.physical_new_bytes = pin.size AND pin.state = 'ACTIVE'
      AND blob.publication_generation = NEW.generation AND blob.gc_state = 'NONE' AND blob.gc_removed_at IS NULL
)
BEGIN SELECT RAISE(ABORT, 'blob publication owner pin does not match generation'); END;

DROP TRIGGER blobs_state_transition;
CREATE TRIGGER blobs_state_transition
BEFORE UPDATE OF state ON blobs
WHEN NOT ((OLD.state = 'STAGING' AND NEW.state IN ('STAGING','READY','QUARANTINING')) OR
          (OLD.state = 'READY' AND NEW.state IN ('READY','QUARANTINING')) OR
          (OLD.state = 'READY' AND OLD.gc_state = 'DELETING' AND NEW.state = 'STAGING' AND NEW.gc_removed_at IS NOT NULL) OR
          (OLD.state = 'QUARANTINING' AND NEW.state = 'CORRUPT') OR
          (OLD.state = 'CORRUPT' AND NEW.state = 'CORRUPT'))
BEGIN SELECT RAISE(ABORT, 'invalid blob state transition'); END;

CREATE TRIGGER blobs_gc_insert_guard
BEFORE INSERT ON blobs
WHEN NEW.gc_removed_at IS NOT NULL OR NEW.publication_generation <> 1 OR NEW.gc_state <> 'NONE'
BEGIN SELECT RAISE(ABORT, 'new blob must start its first publication'); END;
CREATE TRIGGER blobs_gc_update_guard
BEFORE UPDATE ON blobs
BEGIN
    SELECT CASE WHEN NEW.gc_removed_at IS NOT NULL AND
        (NEW.state <> 'STAGING' OR NEW.gc_state <> 'NONE' OR NEW.verified_at IS NOT NULL)
        THEN RAISE(ABORT, 'removed blob must be an unverified tombstone') END;
    SELECT CASE WHEN NEW.gc_state = 'DELETING' AND (NEW.state <> 'READY' OR NEW.gc_removed_at IS NOT NULL)
        THEN RAISE(ABORT, 'only READY blobs may be deleting') END;
    SELECT CASE WHEN OLD.gc_state = 'DELETING' AND NEW.state <> 'READY' AND NEW.gc_removed_at IS NULL
        THEN RAISE(ABORT, 'deleting blob requires a removal tombstone') END;
    SELECT CASE WHEN NOT (
        (NEW.publication_generation = OLD.publication_generation AND NEW.gc_removed_at IS OLD.gc_removed_at) OR
        (OLD.gc_state = 'DELETING' AND OLD.gc_removed_at IS NULL AND NEW.gc_removed_at IS NOT NULL
            AND NEW.publication_generation = OLD.publication_generation) OR
        (OLD.gc_removed_at IS NOT NULL AND NEW.gc_removed_at IS NULL AND NEW.state = 'STAGING'
            AND NEW.gc_state = 'NONE' AND NEW.verified_at IS NULL
            AND NEW.publication_generation = OLD.publication_generation + 1)
    ) THEN RAISE(ABORT, 'invalid blob publication generation transition') END;
    SELECT CASE WHEN (NEW.gc_state = 'DELETING' OR NEW.gc_removed_at IS NOT NULL) AND (
        EXISTS (SELECT 1 FROM artifact_occurrences WHERE digest = NEW.digest AND size = NEW.size) OR
        EXISTS (SELECT 1 FROM cache_blob_refs WHERE digest = NEW.digest AND size = NEW.size) OR
        EXISTS (SELECT 1 FROM blob_pins WHERE digest = NEW.digest AND size = NEW.size AND state IN ('ACTIVE','RELEASABLE'))
    ) THEN RAISE(ABORT, 'referenced blob cannot be garbage collected') END;
END;
CREATE TRIGGER blobs_gc_delete_guard
BEFORE DELETE ON blobs
BEGIN SELECT RAISE(ABORT, 'blob publication history must be retained'); END;

-- Prevent a new retaining reference from appearing after GC plans deletion.
CREATE TRIGGER blob_pin_gc_insert_guard
BEFORE INSERT ON blob_pins
WHEN NOT EXISTS (SELECT 1 FROM blobs WHERE digest = NEW.digest AND size = NEW.size
    AND gc_state = 'NONE' AND gc_removed_at IS NULL AND state IN ('STAGING','READY'))
BEGIN SELECT RAISE(ABORT, 'blob pin requires an available publication'); END;
CREATE TRIGGER blob_pin_gc_update_guard
BEFORE UPDATE OF state ON blob_pins
WHEN NEW.state IN ('ACTIVE','RELEASABLE') AND NOT EXISTS (
    SELECT 1 FROM blobs WHERE digest = NEW.digest AND size = NEW.size AND gc_state = 'NONE' AND gc_removed_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'blob pin requires an available publication'); END;
CREATE TRIGGER artifact_occurrence_gc_insert_guard
BEFORE INSERT ON artifact_occurrences
WHEN NOT EXISTS (SELECT 1 FROM blobs WHERE digest = NEW.digest AND size = NEW.size
    AND gc_state = 'NONE' AND gc_removed_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'artifact occurrence cannot retain garbage'); END;
CREATE TRIGGER cache_blob_gc_insert_guard
BEFORE INSERT ON cache_blob_refs
WHEN NOT EXISTS (SELECT 1 FROM blobs WHERE digest = NEW.digest AND size = NEW.size
    AND gc_state = 'NONE' AND gc_removed_at IS NULL)
BEGIN SELECT RAISE(ABORT, 'cache reference cannot retain garbage'); END;
