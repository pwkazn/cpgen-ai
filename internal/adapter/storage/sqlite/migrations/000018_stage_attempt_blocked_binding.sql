-- M18 adds durable blocked-checkpoint provenance.  NULL remains legal for
-- historical attempts created before the exact dependency binding existed;
-- all new coordinator-produced blocked attempts populate the JSON value.
ALTER TABLE stage_attempts ADD COLUMN blocked_binding_json BLOB
    CHECK (blocked_binding_json IS NULL OR
        (length(blocked_binding_json) > 0 AND json_valid(CAST(blocked_binding_json AS TEXT))));
