-- Migration 10 binds a publication owner to the exact digest and size of its
-- pin. Migration 9 had only independent pin and blob foreign keys, so a
-- direct SQL writer could cross-wire an otherwise valid pin to another blob.

DROP TRIGGER artifact_blob_publication_owner_immutable;

CREATE UNIQUE INDEX artifact_blob_pin_identity
ON blob_pins(pin_id, digest, size);

ALTER TABLE artifact_blob_publication_owners
RENAME TO artifact_blob_publication_owners_legacy;

CREATE TABLE artifact_blob_publication_owners (
    digest TEXT NOT NULL CHECK (length(digest) = 71),
    size INTEGER NOT NULL CHECK (size >= 0),
    pin_id TEXT NOT NULL,
    PRIMARY KEY (digest, size),
    UNIQUE (pin_id),
    FOREIGN KEY (pin_id, digest, size)
        REFERENCES blob_pins(pin_id, digest, size) ON UPDATE RESTRICT ON DELETE RESTRICT,
    FOREIGN KEY (digest, size)
        REFERENCES blobs(digest, size) ON UPDATE RESTRICT ON DELETE RESTRICT
) STRICT;

INSERT INTO artifact_blob_publication_owners(digest, size, pin_id)
SELECT digest, size, pin_id
FROM artifact_blob_publication_owners_legacy;

DROP TABLE artifact_blob_publication_owners_legacy;

CREATE TRIGGER artifact_blob_publication_owner_immutable
BEFORE UPDATE ON artifact_blob_publication_owners
WHEN NEW.digest <> OLD.digest OR NEW.size <> OLD.size OR NEW.pin_id <> OLD.pin_id
BEGIN SELECT RAISE(ABORT, 'blob publication owner is immutable'); END;

CREATE TRIGGER artifact_blob_publication_owner_delete_guard
BEFORE DELETE ON artifact_blob_publication_owners
BEGIN SELECT RAISE(ABORT, 'blob publication owners are immutable'); END;

CREATE TRIGGER artifact_blob_publication_owner_binding_insert
BEFORE INSERT ON artifact_blob_publication_owners
WHEN NOT EXISTS (
    SELECT 1 FROM blob_pins pin
    WHERE pin.pin_id = NEW.pin_id AND pin.digest = NEW.digest AND pin.size = NEW.size
)
BEGIN SELECT RAISE(ABORT, 'blob publication owner pin does not match blob'); END;
