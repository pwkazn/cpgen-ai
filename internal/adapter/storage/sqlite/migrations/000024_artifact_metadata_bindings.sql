-- Preserve historical rows and migration bytes. New stage attachments must
-- retain the immutable metadata that authorized the write or cache use.

CREATE TRIGGER artifact_occurrence_declared_metadata_guard
BEFORE INSERT ON artifact_occurrences
WHEN NEW.kind = 'NEW_WRITE'
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM artifact_declarations declaration
        WHERE declaration.declaration_id = NEW.declaration_id
          AND declaration.role = NEW.role
          AND declaration.media_type = NEW.media_type
          AND declaration.logical_path = NEW.logical_path
          AND declaration.provenance_json = NEW.provenance_json
    ) THEN RAISE(ABORT, 'artifact occurrence differs from declared metadata') END;
END;

CREATE TRIGGER artifact_occurrence_cached_metadata_guard
BEFORE INSERT ON artifact_occurrences
WHEN NEW.kind = 'CACHE_REUSE'
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM cache_reuse_records reuse
        JOIN cache_blob_order ordered ON ordered.cache_key_digest = reuse.cache_key_digest
          AND ordered.source_occurrence_id = reuse.source_occurrence_id
          AND ordered.digest = reuse.digest AND ordered.size = reuse.size
        JOIN cache_blob_refs blob ON blob.cache_key_digest = ordered.cache_key_digest
          AND blob.digest = ordered.digest AND blob.size = ordered.size AND blob.role = ordered.role
        WHERE reuse.cache_reuse_record_id = NEW.cache_reuse_record_id
          AND blob.role = NEW.role
          AND blob.media_type = NEW.media_type
          AND blob.logical_path = NEW.logical_path
          AND blob.provenance_json = NEW.provenance_json
          AND blob.provenance_digest = NEW.provenance_digest
    ) THEN RAISE(ABORT, 'artifact occurrence differs from cached metadata') END;
END;
