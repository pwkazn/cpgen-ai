-- Permit PREPARED/OPEN writers to be released without inventing a seal.
-- Rebuild atomically, preserve every identity and dependent row, and defer
-- foreign keys only until the same transaction restores the original name.
PRAGMA defer_foreign_keys = ON;
CREATE TABLE artifact_writer_tokens_v20 (
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
    CHECK ((state = 'PREPARED' AND opened_at IS NULL AND sealed_at IS NULL) OR (state IN ('OPEN','SEALED','FINALIZED') AND opened_at IS NOT NULL) OR state = 'RELEASED'),
    CHECK ((state IN ('SEALED','FINALIZED') AND sealed_at IS NOT NULL) OR state IN ('PREPARED','OPEN','RELEASED')),
    CHECK ((state = 'FINALIZED' AND finalized_at IS NOT NULL) OR state <> 'FINALIZED'),
    CHECK ((state = 'RELEASED' AND released_at IS NOT NULL) OR state <> 'RELEASED')
) STRICT;

INSERT INTO artifact_writer_tokens_v20 SELECT * FROM artifact_writer_tokens;
DROP TABLE artifact_writer_tokens;
PRAGMA legacy_alter_table = ON;
ALTER TABLE artifact_writer_tokens_v20 RENAME TO artifact_writer_tokens;
PRAGMA legacy_alter_table = OFF;

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

-- Dropping/recreating a referenced table leaves SQLite's deferred violation
-- counter populated even after every referenced identity is restored. Check
-- the actual foreign-key graph before clearing that temporary counter.
CREATE TEMP TABLE cpgen_m20_foreign_key_guard (
    violation_count INTEGER NOT NULL CHECK (violation_count = 0)
);
INSERT INTO cpgen_m20_foreign_key_guard SELECT count(*) FROM pragma_foreign_key_check;
DROP TABLE cpgen_m20_foreign_key_guard;
PRAGMA defer_foreign_keys = OFF;
