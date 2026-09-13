-- M4 is immutable once applied. Keep its byte history stable and install the
-- terminal projection guards in this ordered forward migration instead.

CREATE TRIGGER call_records_terminal_matrix_insert
BEFORE INSERT ON call_records
WHEN NEW.state = 'TERMINAL'
BEGIN
    SELECT CASE WHEN NEW.dispatch_kind = 'DISPATCHED' AND NOT EXISTS (
        SELECT 1 FROM physical_calls
        WHERE attempt_call_id = NEW.result_attempt_call_id
          AND call_record_id = NEW.call_record_id
          AND run_id = NEW.run_id
          AND stage_name = NEW.stage_name
          AND attempt_id = NEW.attempt_id
          AND state IN ('COMPLETED','UNKNOWN')
    ) THEN RAISE(ABORT, 'DISPATCHED result physical call is not terminal') END;
    SELECT CASE WHEN NEW.dispatch_kind = 'CACHE_HIT' AND EXISTS (
        SELECT 1 FROM physical_calls WHERE call_record_id = NEW.call_record_id
    ) THEN RAISE(ABORT, 'CACHE_HIT cannot have physical calls') END;
END;

DROP TRIGGER IF EXISTS physical_calls_terminal_parent_update;

CREATE TRIGGER physical_calls_terminal_parent_update
BEFORE UPDATE ON physical_calls
BEGIN
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM call_records
        WHERE call_record_id = NEW.call_record_id
          AND state = 'TERMINAL'
          AND dispatch_kind = 'DISPATCHED'
          AND result_attempt_call_id = NEW.attempt_call_id
          AND NEW.state NOT IN ('COMPLETED','UNKNOWN')
    ) THEN RAISE(ABORT, 'terminal DISPATCHED result must stay terminal') END;
END;
