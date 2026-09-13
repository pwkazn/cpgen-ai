-- Migration 9 records the immutable first-publication owner for every
-- canonical blob. Migration 8's compatibility backfill used wall-clock
-- ordering, which is not authoritative when historical clocks tie. SQLite's
-- persisted rowid is the insertion identity of the pin row and is stable for
-- the lifetime of that row; use it once to establish an owner record, then
-- make the owner identity the durable source for physical byte accounting.

CREATE TABLE artifact_blob_publication_owners (
    digest TEXT NOT NULL CHECK (length(digest) = 71),
    size INTEGER NOT NULL CHECK (size >= 0),
    pin_id TEXT NOT NULL,
    PRIMARY KEY (digest, size),
    UNIQUE (pin_id),
    FOREIGN KEY (pin_id) REFERENCES blob_pins(pin_id) ON DELETE RESTRICT,
    FOREIGN KEY (digest, size) REFERENCES blobs(digest, size) ON DELETE RESTRICT
) STRICT;

INSERT INTO artifact_blob_publication_owners(digest, size, pin_id)
SELECT digest, size, pin_id
FROM (
    SELECT digest, size, pin_id,
           ROW_NUMBER() OVER (PARTITION BY digest, size ORDER BY rowid) AS owner_ordinal
    FROM blob_pins
)
WHERE owner_ordinal = 1;

DROP TRIGGER artifact_pin_physical_bytes_immutable;
UPDATE blob_pins
SET physical_new_bytes = CASE WHEN pin_id = (
    SELECT owner.pin_id
    FROM artifact_blob_publication_owners owner
    WHERE owner.digest = blob_pins.digest AND owner.size = blob_pins.size
) THEN size ELSE 0 END;
CREATE TRIGGER artifact_pin_physical_bytes_immutable
BEFORE UPDATE OF physical_new_bytes ON blob_pins
WHEN NEW.physical_new_bytes <> OLD.physical_new_bytes
BEGIN SELECT RAISE(ABORT, 'blob pin physical byte count is immutable'); END;

CREATE TRIGGER artifact_blob_publication_owner_immutable
BEFORE UPDATE ON artifact_blob_publication_owners
WHEN NEW.digest <> OLD.digest OR NEW.size <> OLD.size OR NEW.pin_id <> OLD.pin_id
BEGIN SELECT RAISE(ABORT, 'blob publication owner is immutable'); END;
