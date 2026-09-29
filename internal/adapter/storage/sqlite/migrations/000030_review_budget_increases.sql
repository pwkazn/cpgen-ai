-- Keep the originally admitted limits immutable as a separate baseline. The
-- runs columns remain the effective projection used by existing executors.
CREATE TABLE run_budget_baselines (
    run_id TEXT PRIMARY KEY REFERENCES runs(run_id) ON DELETE RESTRICT,
    max_llm_calls INTEGER NOT NULL CHECK (max_llm_calls >= 0),
    max_similarity_calls INTEGER NOT NULL CHECK (max_similarity_calls >= 0),
    max_llm_input_tokens INTEGER NOT NULL CHECK (max_llm_input_tokens >= 0),
    max_llm_output_tokens INTEGER NOT NULL CHECK (max_llm_output_tokens >= 0),
    max_llm_cost_micro_usd INTEGER NOT NULL CHECK (max_llm_cost_micro_usd >= 0),
    max_similarity_cost_micro_usd INTEGER NOT NULL CHECK (max_similarity_cost_micro_usd >= 0),
    max_sandbox_creates INTEGER NOT NULL CHECK (max_sandbox_creates >= 0),
    max_artifact_bytes INTEGER NOT NULL CHECK (max_artifact_bytes >= 0),
    max_package_bytes INTEGER NOT NULL CHECK (max_package_bytes >= 0),
    max_mutations_per_stage INTEGER NOT NULL CHECK (max_mutations_per_stage >= 0),
    max_active_time_ns INTEGER NOT NULL CHECK (max_active_time_ns >= 0)
) STRICT;

INSERT INTO run_budget_baselines(
    run_id, max_llm_calls, max_similarity_calls, max_llm_input_tokens, max_llm_output_tokens,
    max_llm_cost_micro_usd, max_similarity_cost_micro_usd, max_sandbox_creates,
    max_artifact_bytes, max_package_bytes, max_mutations_per_stage, max_active_time_ns
)
SELECT run_id, max_llm_calls, max_similarity_calls, max_llm_input_tokens, max_llm_output_tokens,
       max_llm_cost_micro_usd, max_similarity_cost_micro_usd, max_sandbox_creates,
       max_artifact_bytes, max_package_bytes, max_mutations_per_stage, max_active_time_ns
FROM runs;

CREATE TRIGGER run_budget_baselines_insert_binding
BEFORE INSERT ON run_budget_baselines
WHEN NOT EXISTS (
    SELECT 1 FROM runs
    WHERE runs.run_id = NEW.run_id
      AND runs.max_llm_calls = NEW.max_llm_calls
      AND runs.max_similarity_calls = NEW.max_similarity_calls
      AND runs.max_llm_input_tokens = NEW.max_llm_input_tokens
      AND runs.max_llm_output_tokens = NEW.max_llm_output_tokens
      AND runs.max_llm_cost_micro_usd = NEW.max_llm_cost_micro_usd
      AND runs.max_similarity_cost_micro_usd = NEW.max_similarity_cost_micro_usd
      AND runs.max_sandbox_creates = NEW.max_sandbox_creates
      AND runs.max_artifact_bytes = NEW.max_artifact_bytes
      AND runs.max_package_bytes = NEW.max_package_bytes
      AND runs.max_mutations_per_stage = NEW.max_mutations_per_stage
      AND runs.max_active_time_ns = NEW.max_active_time_ns
)
BEGIN SELECT RAISE(ABORT, 'run budget baseline differs from admitted run limits'); END;

CREATE TRIGGER run_budget_baselines_immutable_update
BEFORE UPDATE ON run_budget_baselines
BEGIN SELECT RAISE(ABORT, 'run budget baselines are immutable'); END;
CREATE TRIGGER run_budget_baselines_immutable_delete
BEFORE DELETE ON run_budget_baselines
BEGIN SELECT RAISE(ABORT, 'run budget baselines are immutable'); END;

-- Grants are the normalized, append-only representation of the amount
-- approved by a RETRY decision. Mutation quota is intentionally omitted: its
-- proof and ordinal space are bound to the immutable submitted request.
CREATE TABLE review_budget_grants (
    review_id TEXT NOT NULL REFERENCES review_decisions(review_id) ON DELETE RESTRICT,
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    field TEXT NOT NULL CHECK (field IN (
        'max_llm_calls','max_similarity_calls','max_llm_input_tokens','max_llm_output_tokens',
        'max_llm_cost_micro_usd','max_similarity_cost_micro_usd','max_sandbox_creates',
        'max_artifact_bytes','max_package_bytes','max_active_time_milliseconds'
    )),
    delta_value INTEGER NOT NULL CHECK (delta_value > 0),
    PRIMARY KEY (review_id, field),
    UNIQUE (review_id, field, delta_value)
) STRICT;

CREATE TRIGGER review_budget_grants_approved_amount
BEFORE INSERT ON review_budget_grants
WHEN NOT EXISTS (
    SELECT 1 FROM review_decisions decision
    WHERE decision.review_id = NEW.review_id
      AND decision.run_id = NEW.run_id
      AND decision.kind = 'RETRY'
      AND decision.state IN ('PENDING','APPLIED','STALE')
      AND decision.budget_increase_json IS NOT NULL
      AND json_valid(decision.budget_increase_json)
      AND NEW.delta_value = CASE NEW.field
          WHEN 'max_llm_calls' THEN json_extract(decision.budget_increase_json, '$.max_llm_calls')
          WHEN 'max_similarity_calls' THEN json_extract(decision.budget_increase_json, '$.max_similarity_calls')
          WHEN 'max_llm_input_tokens' THEN json_extract(decision.budget_increase_json, '$.max_llm_input_tokens')
          WHEN 'max_llm_output_tokens' THEN json_extract(decision.budget_increase_json, '$.max_llm_output_tokens')
          WHEN 'max_llm_cost_micro_usd' THEN json_extract(decision.budget_increase_json, '$.max_llm_cost_micro_usd')
          WHEN 'max_similarity_cost_micro_usd' THEN json_extract(decision.budget_increase_json, '$.max_similarity_cost_micro_usd')
          WHEN 'max_sandbox_creates' THEN json_extract(decision.budget_increase_json, '$.max_sandbox_creates')
          WHEN 'max_artifact_bytes' THEN json_extract(decision.budget_increase_json, '$.max_artifact_bytes')
          WHEN 'max_package_bytes' THEN json_extract(decision.budget_increase_json, '$.max_package_bytes')
          WHEN 'max_active_time_milliseconds' THEN json_extract(decision.budget_increase_json, '$.max_active_time_milliseconds')
      END
)
BEGIN SELECT RAISE(ABORT, 'review budget grant exceeds the pending RETRY approval'); END;

CREATE TRIGGER review_budget_grants_immutable_update
BEFORE UPDATE ON review_budget_grants
BEGIN SELECT RAISE(ABORT, 'review budget grants are immutable'); END;
CREATE TRIGGER review_budget_grants_immutable_delete
BEFORE DELETE ON review_budget_grants
BEGIN SELECT RAISE(ABORT, 'review budget grants are immutable'); END;

CREATE TABLE review_budget_applications (
    review_id TEXT NOT NULL,
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    field TEXT NOT NULL,
    delta_value INTEGER NOT NULL CHECK (delta_value > 0),
    applied_at TEXT NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('LIVE_APPLY','LEGACY_RECOVERY')),
    PRIMARY KEY (review_id, field),
    FOREIGN KEY (review_id, field, delta_value)
        REFERENCES review_budget_grants(review_id, field, delta_value) ON DELETE RESTRICT
) STRICT;

CREATE TRIGGER review_budget_applications_review_binding
BEFORE INSERT ON review_budget_applications
WHEN NOT EXISTS (
    SELECT 1 FROM review_decisions decision
    WHERE decision.review_id = NEW.review_id
      AND decision.run_id = NEW.run_id
      AND decision.kind = 'RETRY'
      AND decision.state = 'APPLIED'
      AND decision.applied_at = NEW.applied_at
)
BEGIN SELECT RAISE(ABORT, 'review budget application is not bound to an applied RETRY'); END;

CREATE TRIGGER review_budget_applications_immutable_update
BEFORE UPDATE ON review_budget_applications
BEGIN SELECT RAISE(ABORT, 'review budget applications are immutable'); END;
CREATE TRIGGER review_budget_applications_immutable_delete
BEFORE DELETE ON review_budget_applications
BEGIN SELECT RAISE(ABORT, 'review budget applications are immutable'); END;

CREATE TABLE review_budget_recovery_exceptions (
    review_id TEXT NOT NULL REFERENCES review_decisions(review_id) ON DELETE RESTRICT,
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    field TEXT NOT NULL CHECK (field IN (
        'max_mutations_per_stage','max_llm_calls','max_similarity_calls','max_llm_input_tokens',
        'max_llm_output_tokens','max_llm_cost_micro_usd','max_similarity_cost_micro_usd',
        'max_sandbox_creates','max_artifact_bytes','max_package_bytes','max_active_time_milliseconds'
    )),
    delta_value INTEGER NOT NULL CHECK (delta_value > 0),
    reason TEXT NOT NULL CHECK (reason IN ('FROZEN_MUTATION_QUOTA','READY_RUN_IMMUTABLE')),
    recorded_at TEXT NOT NULL,
    PRIMARY KEY (review_id, field)
) STRICT;

CREATE TRIGGER review_budget_recovery_exceptions_review_binding
BEFORE INSERT ON review_budget_recovery_exceptions
WHEN NOT EXISTS (
    SELECT 1 FROM review_decisions decision
    WHERE decision.review_id = NEW.review_id
      AND decision.run_id = NEW.run_id
      AND decision.kind = 'RETRY'
      AND decision.state IN ('APPLIED','STALE')
)
BEGIN SELECT RAISE(ABORT, 'review budget recovery exception is not bound to an applied or stale RETRY'); END;

CREATE TRIGGER review_budget_recovery_exceptions_immutable_update
BEFORE UPDATE ON review_budget_recovery_exceptions
BEGIN SELECT RAISE(ABORT, 'review budget recovery exceptions are immutable'); END;
CREATE TRIGGER review_budget_recovery_exceptions_immutable_delete
BEFORE DELETE ON review_budget_recovery_exceptions
BEGIN SELECT RAISE(ABORT, 'review budget recovery exceptions are immutable'); END;

DROP TRIGGER runs_budget_limits_immutable;
CREATE TRIGGER runs_budget_limits_approved_projection
BEFORE UPDATE OF max_llm_calls, max_similarity_calls, max_llm_input_tokens, max_llm_output_tokens,
    max_llm_cost_micro_usd, max_similarity_cost_micro_usd, max_sandbox_creates,
    max_artifact_bytes, max_package_bytes, max_mutations_per_stage, max_active_time_ns ON runs
BEGIN
    SELECT CASE WHEN NEW.max_llm_calls <> (
        SELECT base.max_llm_calls + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_llm_calls'),0)
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'LLM call limit differs from approved budget applications') END;
    SELECT CASE WHEN NEW.max_similarity_calls <> (
        SELECT base.max_similarity_calls + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_similarity_calls'),0)
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'similarity call limit differs from approved budget applications') END;
    SELECT CASE WHEN NEW.max_llm_input_tokens <> (
        SELECT base.max_llm_input_tokens + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_llm_input_tokens'),0)
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'LLM input limit differs from approved budget applications') END;
    SELECT CASE WHEN NEW.max_llm_output_tokens <> (
        SELECT base.max_llm_output_tokens + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_llm_output_tokens'),0)
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'LLM output limit differs from approved budget applications') END;
    SELECT CASE WHEN NEW.max_llm_cost_micro_usd <> (
        SELECT base.max_llm_cost_micro_usd + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_llm_cost_micro_usd'),0)
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'LLM cost limit differs from approved budget applications') END;
    SELECT CASE WHEN NEW.max_similarity_cost_micro_usd <> (
        SELECT base.max_similarity_cost_micro_usd + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_similarity_cost_micro_usd'),0)
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'similarity cost limit differs from approved budget applications') END;
    SELECT CASE WHEN NEW.max_sandbox_creates <> (
        SELECT base.max_sandbox_creates + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_sandbox_creates'),0)
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'sandbox limit differs from approved budget applications') END;
    SELECT CASE WHEN NEW.max_artifact_bytes <> (
        SELECT base.max_artifact_bytes + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_artifact_bytes'),0)
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'artifact limit differs from approved budget applications') END;
    SELECT CASE WHEN NEW.max_package_bytes <> (
        SELECT base.max_package_bytes + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_package_bytes'),0)
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'package limit differs from approved budget applications') END;
    SELECT CASE WHEN NEW.max_mutations_per_stage <> (
        SELECT max_mutations_per_stage FROM run_budget_baselines WHERE run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'mutation quota is bound to the immutable submitted request') END;
    SELECT CASE WHEN NEW.max_active_time_ns <> (
        SELECT base.max_active_time_ns + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_active_time_milliseconds'),0) * 1000000
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'active-time limit differs from approved budget applications') END;
END;

-- Normalize all old approvals before projecting them. Re-running startup does
-- not run this migration again; the unique review/field keys also make the
-- audit/application rows intrinsically non-duplicable.
UPDATE review_decisions
SET state='STALE',applied_at=created_at
WHERE kind='RETRY' AND state='PENDING' AND budget_increase_json IS NOT NULL
  AND json_extract(budget_increase_json, '$.max_mutations_per_stage') > 0;

INSERT INTO review_budget_grants(review_id, run_id, field, delta_value)
SELECT review_id, run_id, 'max_llm_calls', json_extract(budget_increase_json, '$.max_llm_calls') FROM review_decisions WHERE kind='RETRY' AND budget_increase_json IS NOT NULL AND json_extract(budget_increase_json, '$.max_llm_calls') > 0
UNION ALL SELECT review_id, run_id, 'max_similarity_calls', json_extract(budget_increase_json, '$.max_similarity_calls') FROM review_decisions WHERE kind='RETRY' AND budget_increase_json IS NOT NULL AND json_extract(budget_increase_json, '$.max_similarity_calls') > 0
UNION ALL SELECT review_id, run_id, 'max_llm_input_tokens', json_extract(budget_increase_json, '$.max_llm_input_tokens') FROM review_decisions WHERE kind='RETRY' AND budget_increase_json IS NOT NULL AND json_extract(budget_increase_json, '$.max_llm_input_tokens') > 0
UNION ALL SELECT review_id, run_id, 'max_llm_output_tokens', json_extract(budget_increase_json, '$.max_llm_output_tokens') FROM review_decisions WHERE kind='RETRY' AND budget_increase_json IS NOT NULL AND json_extract(budget_increase_json, '$.max_llm_output_tokens') > 0
UNION ALL SELECT review_id, run_id, 'max_llm_cost_micro_usd', json_extract(budget_increase_json, '$.max_llm_cost_micro_usd') FROM review_decisions WHERE kind='RETRY' AND budget_increase_json IS NOT NULL AND json_extract(budget_increase_json, '$.max_llm_cost_micro_usd') > 0
UNION ALL SELECT review_id, run_id, 'max_similarity_cost_micro_usd', json_extract(budget_increase_json, '$.max_similarity_cost_micro_usd') FROM review_decisions WHERE kind='RETRY' AND budget_increase_json IS NOT NULL AND json_extract(budget_increase_json, '$.max_similarity_cost_micro_usd') > 0
UNION ALL SELECT review_id, run_id, 'max_sandbox_creates', json_extract(budget_increase_json, '$.max_sandbox_creates') FROM review_decisions WHERE kind='RETRY' AND budget_increase_json IS NOT NULL AND json_extract(budget_increase_json, '$.max_sandbox_creates') > 0
UNION ALL SELECT review_id, run_id, 'max_artifact_bytes', json_extract(budget_increase_json, '$.max_artifact_bytes') FROM review_decisions WHERE kind='RETRY' AND budget_increase_json IS NOT NULL AND json_extract(budget_increase_json, '$.max_artifact_bytes') > 0
UNION ALL SELECT review_id, run_id, 'max_package_bytes', json_extract(budget_increase_json, '$.max_package_bytes') FROM review_decisions WHERE kind='RETRY' AND budget_increase_json IS NOT NULL AND json_extract(budget_increase_json, '$.max_package_bytes') > 0
UNION ALL SELECT review_id, run_id, 'max_active_time_milliseconds', json_extract(budget_increase_json, '$.max_active_time_milliseconds') FROM review_decisions WHERE kind='RETRY' AND budget_increase_json IS NOT NULL AND json_extract(budget_increase_json, '$.max_active_time_milliseconds') > 0;

INSERT INTO review_budget_applications(review_id, run_id, field, delta_value, applied_at, source)
SELECT grant.review_id, grant.run_id, grant.field, grant.delta_value, decision.applied_at, 'LEGACY_RECOVERY'
FROM review_budget_grants grant
JOIN review_decisions decision ON decision.review_id=grant.review_id
JOIN runs ON runs.run_id=grant.run_id
WHERE decision.state='APPLIED' AND runs.state <> 'READY';

INSERT INTO review_budget_recovery_exceptions(review_id, run_id, field, delta_value, reason, recorded_at)
SELECT review_id, run_id, 'max_mutations_per_stage', json_extract(budget_increase_json, '$.max_mutations_per_stage'), 'FROZEN_MUTATION_QUOTA', applied_at
FROM review_decisions
WHERE kind='RETRY' AND state IN ('APPLIED','STALE') AND budget_increase_json IS NOT NULL
  AND json_extract(budget_increase_json, '$.max_mutations_per_stage') > 0;

INSERT INTO review_budget_recovery_exceptions(review_id, run_id, field, delta_value, reason, recorded_at)
SELECT grant.review_id, grant.run_id, grant.field, grant.delta_value, 'READY_RUN_IMMUTABLE', decision.applied_at
FROM review_budget_grants grant
JOIN review_decisions decision ON decision.review_id=grant.review_id
JOIN runs ON runs.run_id=grant.run_id
WHERE decision.state='APPLIED' AND runs.state='READY';

UPDATE runs SET
    max_llm_calls = (SELECT base.max_llm_calls + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=runs.run_id AND field='max_llm_calls'),0) FROM run_budget_baselines base WHERE base.run_id=runs.run_id),
    max_similarity_calls = (SELECT base.max_similarity_calls + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=runs.run_id AND field='max_similarity_calls'),0) FROM run_budget_baselines base WHERE base.run_id=runs.run_id),
    max_llm_input_tokens = (SELECT base.max_llm_input_tokens + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=runs.run_id AND field='max_llm_input_tokens'),0) FROM run_budget_baselines base WHERE base.run_id=runs.run_id),
    max_llm_output_tokens = (SELECT base.max_llm_output_tokens + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=runs.run_id AND field='max_llm_output_tokens'),0) FROM run_budget_baselines base WHERE base.run_id=runs.run_id),
    max_llm_cost_micro_usd = (SELECT base.max_llm_cost_micro_usd + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=runs.run_id AND field='max_llm_cost_micro_usd'),0) FROM run_budget_baselines base WHERE base.run_id=runs.run_id),
    max_similarity_cost_micro_usd = (SELECT base.max_similarity_cost_micro_usd + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=runs.run_id AND field='max_similarity_cost_micro_usd'),0) FROM run_budget_baselines base WHERE base.run_id=runs.run_id),
    max_sandbox_creates = (SELECT base.max_sandbox_creates + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=runs.run_id AND field='max_sandbox_creates'),0) FROM run_budget_baselines base WHERE base.run_id=runs.run_id),
    max_artifact_bytes = (SELECT base.max_artifact_bytes + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=runs.run_id AND field='max_artifact_bytes'),0) FROM run_budget_baselines base WHERE base.run_id=runs.run_id),
    max_package_bytes = (SELECT base.max_package_bytes + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=runs.run_id AND field='max_package_bytes'),0) FROM run_budget_baselines base WHERE base.run_id=runs.run_id),
    max_active_time_ns = (SELECT base.max_active_time_ns + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=runs.run_id AND field='max_active_time_milliseconds'),0) * 1000000 FROM run_budget_baselines base WHERE base.run_id=runs.run_id)
WHERE state <> 'READY'
  AND EXISTS (SELECT 1 FROM review_budget_applications WHERE run_id=runs.run_id);

UPDATE budget_accounts SET
    limit_value = CASE dimension
        WHEN 'LLM_CALLS' THEN (SELECT max_llm_calls FROM runs WHERE runs.run_id=budget_accounts.run_id)
        WHEN 'LLM_INPUT_TOKENS' THEN (SELECT max_llm_input_tokens FROM runs WHERE runs.run_id=budget_accounts.run_id)
        WHEN 'LLM_OUTPUT_TOKENS' THEN (SELECT max_llm_output_tokens FROM runs WHERE runs.run_id=budget_accounts.run_id)
        WHEN 'EXTERNAL_COST_MICRO_USD' THEN (SELECT max_llm_cost_micro_usd FROM runs WHERE runs.run_id=budget_accounts.run_id)
        WHEN 'SIMILARITY_CALLS' THEN (SELECT max_similarity_calls FROM runs WHERE runs.run_id=budget_accounts.run_id)
        WHEN 'SIMILARITY_COST_MICRO_USD' THEN (SELECT max_similarity_cost_micro_usd FROM runs WHERE runs.run_id=budget_accounts.run_id)
        WHEN 'DOCKER_CONTAINER_CREATES' THEN (SELECT max_sandbox_creates FROM runs WHERE runs.run_id=budget_accounts.run_id)
        WHEN 'ARTIFACT_PHYSICAL_NEW_BYTES' THEN (SELECT max_artifact_bytes FROM runs WHERE runs.run_id=budget_accounts.run_id)
        WHEN 'ACTIVE_TIME_NS' THEN (SELECT max_active_time_ns FROM runs WHERE runs.run_id=budget_accounts.run_id)
    END,
    account_version = account_version + 1
WHERE EXISTS (
    SELECT 1 FROM review_budget_applications application
    WHERE application.run_id=budget_accounts.run_id
      AND application.field = CASE budget_accounts.dimension
          WHEN 'LLM_CALLS' THEN 'max_llm_calls'
          WHEN 'LLM_INPUT_TOKENS' THEN 'max_llm_input_tokens'
          WHEN 'LLM_OUTPUT_TOKENS' THEN 'max_llm_output_tokens'
          WHEN 'EXTERNAL_COST_MICRO_USD' THEN 'max_llm_cost_micro_usd'
          WHEN 'SIMILARITY_CALLS' THEN 'max_similarity_calls'
          WHEN 'SIMILARITY_COST_MICRO_USD' THEN 'max_similarity_cost_micro_usd'
          WHEN 'DOCKER_CONTAINER_CREATES' THEN 'max_sandbox_creates'
          WHEN 'ARTIFACT_PHYSICAL_NEW_BYTES' THEN 'max_artifact_bytes'
          WHEN 'ACTIVE_TIME_NS' THEN 'max_active_time_milliseconds'
      END
)
AND EXISTS (SELECT 1 FROM runs WHERE runs.run_id=budget_accounts.run_id AND runs.state <> 'READY');
