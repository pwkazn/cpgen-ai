-- Migration 12 hardens the cache and mutation ledgers without rewriting
-- migrations 1-11.  The stage account is the single authoritative mutation
-- quota; the historical per-kind accounts remain as an auditable projection.

CREATE TABLE mutation_stage_accounts (
    run_id TEXT NOT NULL,
    stage_name TEXT NOT NULL,
    limit_value INTEGER NOT NULL CHECK (limit_value >= 0),
    claimed_value INTEGER NOT NULL DEFAULT 0 CHECK (claimed_value >= 0),
    account_version INTEGER NOT NULL DEFAULT 1 CHECK (account_version > 0),
    PRIMARY KEY (run_id, stage_name),
    FOREIGN KEY (run_id, stage_name) REFERENCES stage_records(run_id, stage_name) ON DELETE RESTRICT,
    CHECK (claimed_value <= limit_value)
) STRICT;

INSERT INTO mutation_stage_accounts(run_id, stage_name, limit_value, claimed_value, account_version)
SELECT stages.run_id, stages.stage_name, runs.max_mutations_per_stage,
       COUNT(claims.claim_id), 1 + COUNT(claims.claim_id)
FROM stage_records stages
JOIN runs ON runs.run_id = stages.run_id
LEFT JOIN mutation_claims claims
  ON claims.run_id = stages.run_id AND claims.stage_name = stages.stage_name
GROUP BY stages.run_id, stages.stage_name, runs.max_mutations_per_stage;

CREATE TABLE cache_blob_order (
    cache_key_digest TEXT NOT NULL,
    blob_ordinal INTEGER NOT NULL CHECK (blob_ordinal > 0),
    digest TEXT NOT NULL,
    size INTEGER NOT NULL CHECK (size >= 0),
    role TEXT NOT NULL,
    source_occurrence_id TEXT,
    PRIMARY KEY (cache_key_digest, blob_ordinal),
    UNIQUE (cache_key_digest, digest, size, role),
    FOREIGN KEY (cache_key_digest, digest, size, role)
        REFERENCES cache_blob_refs(cache_key_digest, digest, size, role) ON DELETE RESTRICT,
    FOREIGN KEY (cache_key_digest, source_occurrence_id)
        REFERENCES cache_entry_sources(cache_key_digest, source_occurrence_id) ON DELETE RESTRICT
) STRICT;

CREATE TRIGGER cache_blob_order_identity_immutable
BEFORE UPDATE ON cache_blob_order
BEGIN SELECT RAISE(ABORT, 'cache blob order is immutable'); END;
CREATE TRIGGER cache_blob_order_delete_guard
BEFORE DELETE ON cache_blob_order
BEGIN SELECT RAISE(ABORT, 'cache blob order is immutable'); END;

CREATE TRIGGER cache_blob_order_insert_guard
BEFORE INSERT ON cache_blob_order
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM cache_blob_refs blob
        WHERE blob.cache_key_digest = NEW.cache_key_digest
          AND blob.digest = NEW.digest AND blob.size = NEW.size AND blob.role = NEW.role
          AND (NEW.source_occurrence_id IS NULL OR EXISTS (SELECT 1 FROM cache_entry_sources source
                      WHERE source.cache_key_digest = NEW.cache_key_digest
                        AND source.source_occurrence_id = NEW.source_occurrence_id
                        AND source.source_digest = NEW.digest))
    ) THEN RAISE(ABORT, 'cache blob order does not match blob reference') END;
END;

CREATE TRIGGER mutation_stage_account_identity_immutable
BEFORE UPDATE ON mutation_stage_accounts
WHEN NEW.run_id <> OLD.run_id OR NEW.stage_name <> OLD.stage_name OR NEW.limit_value <> OLD.limit_value
BEGIN SELECT RAISE(ABORT, 'mutation stage account identity is immutable'); END;
CREATE TRIGGER mutation_stage_account_delete_guard
BEFORE DELETE ON mutation_stage_accounts
BEGIN SELECT RAISE(ABORT, 'mutation stage account is immutable'); END;

CREATE TRIGGER mutation_claims_immutable
BEFORE UPDATE ON mutation_claims
BEGIN SELECT RAISE(ABORT, 'mutation claims are immutable'); END;
CREATE TRIGGER mutation_claims_delete_guard
BEFORE DELETE ON mutation_claims
BEGIN SELECT RAISE(ABORT, 'mutation claims are immutable'); END;
CREATE TRIGGER mutation_intents_immutable
BEFORE UPDATE ON mutation_intents
BEGIN SELECT RAISE(ABORT, 'mutation intents are immutable'); END;
CREATE TRIGGER mutation_intents_delete_guard
BEFORE DELETE ON mutation_intents
BEGIN SELECT RAISE(ABORT, 'mutation intents are immutable'); END;
CREATE TRIGGER mutation_records_immutable
BEFORE UPDATE ON mutation_records
BEGIN SELECT RAISE(ABORT, 'mutation records are immutable'); END;
CREATE TRIGGER mutation_records_delete_guard
BEFORE DELETE ON mutation_records
BEGIN SELECT RAISE(ABORT, 'mutation records are immutable'); END;
CREATE TRIGGER mutation_record_operations_delete_guard
BEFORE DELETE ON mutation_record_operations
BEGIN SELECT RAISE(ABORT, 'mutation operation evidence is immutable'); END;
CREATE TRIGGER mutation_record_reservations_delete_guard
BEFORE DELETE ON mutation_record_reservations
BEGIN SELECT RAISE(ABORT, 'mutation reservation evidence is immutable'); END;
CREATE TRIGGER mutation_record_output_delete_guard
BEFORE DELETE ON mutation_record_output_occurrences
BEGIN SELECT RAISE(ABORT, 'mutation output evidence is immutable'); END;

-- Existing rows are assigned the deterministic order that M11 exposed. New
-- writers insert their caller-provided order explicitly in the application.
INSERT INTO cache_blob_order(cache_key_digest, blob_ordinal, digest, size, role, source_occurrence_id)
SELECT refs.cache_key_digest,
       ROW_NUMBER() OVER (PARTITION BY cache_key_digest ORDER BY role, digest, size),
       refs.digest, refs.size, refs.role,
       (SELECT source_occurrence_id FROM cache_entry_sources sources
        WHERE sources.cache_key_digest = refs.cache_key_digest
          AND sources.source_digest = refs.digest
        ORDER BY source_occurrence_id LIMIT 1)
FROM cache_blob_refs refs;
