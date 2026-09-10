-- Similarity receipts share the private local publication protocol. Provider
-- requests and local writes retain separate physical budgets and boundaries.
DROP TRIGGER physical_calls_kind_binding;
CREATE TRIGGER physical_calls_kind_binding
BEFORE INSERT ON physical_calls
BEGIN
    SELECT CASE WHEN NOT EXISTS (
        SELECT 1 FROM call_records call
        WHERE call.call_record_id = NEW.call_record_id
          AND (
            (call.call_kind = 'LLM_GENERATE' AND NEW.physical_kind IN ('LLM_REQUEST','LOCAL_ARTIFACT_WRITE')) OR
            (call.call_kind = 'SIMILARITY_SEARCH' AND NEW.physical_kind IN ('SIMILARITY_REQUEST','LOCAL_ARTIFACT_WRITE')) OR
            (call.call_kind IN ('SANDBOX_COMPILE','SANDBOX_RUN','SANDBOX_PROBE')
                AND NEW.physical_kind IN ('DOCKER_ENGINE_PING','DOCKER_CONTAINER_CREATE','LOCAL_ARTIFACT_WRITE'))
          )
    ) THEN RAISE(ABORT, 'physical call kind does not match logical call') END;
END;
