-- Original logical-open metadata is needed when accounting advances the run
-- version before restart. It contains digests and scope, never prompt content
-- or credentials. Historical opens are adopted only with their exact command.
CREATE TABLE call_open_requests (
    call_record_id TEXT PRIMARY KEY,
    request_json BLOB NOT NULL CHECK (length(request_json) > 0 AND length(request_json) <= 16384 AND json_valid(CAST(request_json AS TEXT))),
    request_digest TEXT NOT NULL CHECK (length(request_digest) = 71),
    FOREIGN KEY (call_record_id) REFERENCES call_records(call_record_id) ON DELETE RESTRICT
) STRICT;

CREATE TRIGGER call_open_requests_binding
BEFORE INSERT ON call_open_requests
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM call_records call WHERE call.call_record_id = NEW.call_record_id
        AND call.open_command_digest = NEW.request_digest
        AND json_extract(CAST(NEW.request_json AS TEXT), '$.id') = call.call_record_id
    ) THEN RAISE(ABORT, 'original call open binding differs') END;
END;

CREATE TRIGGER call_open_requests_immutable_update
BEFORE UPDATE ON call_open_requests
BEGIN SELECT RAISE(ABORT, 'original call open is immutable'); END;

CREATE TABLE call_finish_requests (
    call_record_id TEXT PRIMARY KEY,
    request_json BLOB NOT NULL CHECK (length(request_json) > 0 AND length(request_json) <= 16384 AND json_valid(CAST(request_json AS TEXT))),
    request_digest TEXT NOT NULL CHECK (length(request_digest) = 71),
    FOREIGN KEY (call_record_id) REFERENCES call_records(call_record_id) ON DELETE RESTRICT
) STRICT;

CREATE TRIGGER call_finish_requests_binding
BEFORE INSERT ON call_finish_requests
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM call_records call WHERE call.call_record_id = NEW.call_record_id
        AND call.finish_command_digest = NEW.request_digest AND call.state = 'TERMINAL'
        AND json_extract(CAST(NEW.request_json AS TEXT), '$.call_record_id') = call.call_record_id
    ) THEN RAISE(ABORT, 'original call finish binding differs') END;
END;

CREATE TRIGGER call_finish_requests_immutable_update
BEFORE UPDATE ON call_finish_requests
BEGIN SELECT RAISE(ABORT, 'original call finish is immutable'); END;

CREATE TRIGGER call_finish_requests_immutable_delete
BEFORE DELETE ON call_finish_requests
BEGIN SELECT RAISE(ABORT, 'original call finish is immutable'); END;

CREATE TRIGGER call_open_requests_immutable_delete
BEFORE DELETE ON call_open_requests
BEGIN SELECT RAISE(ABORT, 'original call open is immutable'); END;
