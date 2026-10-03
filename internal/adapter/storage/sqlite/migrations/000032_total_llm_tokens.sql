-- The shared token cap is separate from the input/output usage accounts.
-- Historical runs keep token_budget=0 and max_llm_tokens=0, preserving their
-- admitted limits. New token budgets also enforce an explicitly zero cap.
ALTER TABLE runs ADD COLUMN max_llm_tokens INTEGER NOT NULL DEFAULT 0
    CHECK (max_llm_tokens >= 0);
ALTER TABLE runs ADD COLUMN token_budget INTEGER NOT NULL DEFAULT 0
    CHECK (token_budget IN (0,1));
ALTER TABLE run_budget_baselines ADD COLUMN max_llm_tokens INTEGER NOT NULL DEFAULT 0
    CHECK (max_llm_tokens >= 0);
ALTER TABLE run_budget_baselines ADD COLUMN token_budget INTEGER NOT NULL DEFAULT 0
    CHECK (token_budget IN (0,1));

CREATE TRIGGER run_budget_baselines_total_tokens_insert_binding
BEFORE INSERT ON run_budget_baselines
WHEN NOT EXISTS (
    SELECT 1 FROM runs
    WHERE runs.run_id = NEW.run_id
      AND runs.max_llm_tokens = NEW.max_llm_tokens
      AND runs.token_budget = NEW.token_budget
)
BEGIN SELECT RAISE(ABORT, 'run token budget baseline differs from admitted run limits'); END;

CREATE TRIGGER runs_token_budget_immutable
BEFORE UPDATE OF token_budget ON runs
WHEN NEW.token_budget <> OLD.token_budget
BEGIN SELECT RAISE(ABORT, 'run token budget mode is immutable'); END;

-- Extend the grant field CHECK without rebuilding usage accounts or changing
-- historical grants. Copy the child table too so foreign keys remain enabled
-- throughout the replacement. Restore every application/projection guard.
DROP TRIGGER runs_budget_limits_approved_projection;

CREATE TABLE _m32_review_budget_grants (
    review_id TEXT NOT NULL REFERENCES review_decisions(review_id) ON DELETE RESTRICT,
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    field TEXT NOT NULL CHECK (field IN (
        'max_llm_tokens','max_llm_calls','max_similarity_calls','max_llm_input_tokens','max_llm_output_tokens',
        'max_llm_cost_micro_usd','max_similarity_cost_micro_usd','max_sandbox_creates',
        'max_artifact_bytes','max_package_bytes','max_active_time_milliseconds'
    )),
    delta_value INTEGER NOT NULL CHECK (delta_value > 0),
    PRIMARY KEY (review_id, field),
    UNIQUE (review_id, field, delta_value)
) STRICT;

CREATE TABLE _m32_review_budget_applications (
    review_id TEXT NOT NULL,
    run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE RESTRICT,
    field TEXT NOT NULL,
    delta_value INTEGER NOT NULL CHECK (delta_value > 0),
    applied_at TEXT NOT NULL,
    source TEXT NOT NULL CHECK (source IN ('LIVE_APPLY','LEGACY_RECOVERY')),
    PRIMARY KEY (review_id, field),
    FOREIGN KEY (review_id, field, delta_value)
        REFERENCES _m32_review_budget_grants(review_id, field, delta_value) ON DELETE RESTRICT
) STRICT;

INSERT INTO _m32_review_budget_grants(review_id, run_id, field, delta_value)
SELECT review_id, run_id, field, delta_value FROM review_budget_grants;

INSERT INTO _m32_review_budget_applications(review_id, run_id, field, delta_value, applied_at, source)
SELECT review_id, run_id, field, delta_value, applied_at, source FROM review_budget_applications;

DROP TABLE review_budget_applications;
DROP TABLE review_budget_grants;
ALTER TABLE _m32_review_budget_grants RENAME TO review_budget_grants;
ALTER TABLE _m32_review_budget_applications RENAME TO review_budget_applications;

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
          WHEN 'max_llm_tokens' THEN json_extract(decision.budget_increase_json, '$.max_llm_tokens')
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

CREATE TRIGGER runs_budget_limits_approved_projection
BEFORE UPDATE OF max_llm_tokens, max_llm_calls, max_similarity_calls, max_llm_input_tokens, max_llm_output_tokens,
    max_llm_cost_micro_usd, max_similarity_cost_micro_usd, max_sandbox_creates,
    max_artifact_bytes, max_package_bytes, max_mutations_per_stage, max_active_time_ns ON runs
BEGIN
    SELECT CASE WHEN NEW.max_llm_tokens IS NOT (
        SELECT base.max_llm_tokens + COALESCE((SELECT SUM(delta_value) FROM review_budget_applications WHERE run_id=NEW.run_id AND field='max_llm_tokens'),0)
        FROM run_budget_baselines base WHERE base.run_id=NEW.run_id
    ) THEN RAISE(ABORT, 'LLM token limit differs from approved budget applications') END;
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

-- Keep the sum of input/output consumed and reserved tokens within the
-- shared cap. Subtraction avoids overflow even when account limits are int64
-- maxima. Sequential account updates run within the same writer transaction.
CREATE TRIGGER budget_accounts_total_tokens_insert
BEFORE INSERT ON budget_accounts
WHEN NEW.dimension IN ('LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS')
BEGIN
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM runs
        WHERE runs.run_id = NEW.run_id
          AND (runs.token_budget = 1 OR runs.max_llm_tokens > 0)
          AND (
              NEW.consumed_value > runs.max_llm_tokens
              OR NEW.reserved_value > runs.max_llm_tokens - NEW.consumed_value
              OR EXISTS (
                  SELECT 1 FROM budget_accounts other
                  WHERE other.run_id = NEW.run_id
                    AND other.dimension IN ('LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS')
                    AND other.consumed_value + other.reserved_value >
                        runs.max_llm_tokens - NEW.consumed_value - NEW.reserved_value
              )
          )
    ) THEN RAISE(ABORT, 'shared LLM token budget exceeded') END;
END;

CREATE TRIGGER budget_accounts_total_tokens_update
BEFORE UPDATE ON budget_accounts
WHEN NEW.dimension IN ('LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS')
BEGIN
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM runs
        WHERE runs.run_id = NEW.run_id
          AND (runs.token_budget = 1 OR runs.max_llm_tokens > 0)
          AND (
              NEW.consumed_value > runs.max_llm_tokens
              OR NEW.reserved_value > runs.max_llm_tokens - NEW.consumed_value
              OR EXISTS (
                  SELECT 1 FROM budget_accounts other
                  WHERE other.run_id = NEW.run_id
                    AND other.dimension IN ('LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS')
                    AND NOT (other.run_id = OLD.run_id AND other.dimension = OLD.dimension)
                    AND other.consumed_value + other.reserved_value >
                        runs.max_llm_tokens - NEW.consumed_value - NEW.reserved_value
              )
          )
    ) THEN RAISE(ABORT, 'shared LLM token budget exceeded') END;
END;

CREATE TRIGGER runs_total_tokens_usage_binding
BEFORE UPDATE OF max_llm_tokens ON runs
WHEN NEW.token_budget = 1 OR NEW.max_llm_tokens > 0
BEGIN
    SELECT CASE WHEN EXISTS (
        SELECT 1 FROM budget_accounts account
        WHERE account.run_id = NEW.run_id
          AND account.dimension IN ('LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS')
          AND (
              account.consumed_value > NEW.max_llm_tokens
              OR account.reserved_value > NEW.max_llm_tokens - account.consumed_value
              OR EXISTS (
                  SELECT 1 FROM budget_accounts other
                  WHERE other.run_id = NEW.run_id
                    AND other.dimension IN ('LLM_INPUT_TOKENS','LLM_OUTPUT_TOKENS')
                    AND other.dimension <> account.dimension
                    AND other.consumed_value + other.reserved_value >
                        NEW.max_llm_tokens - account.consumed_value - account.reserved_value
              )
          )
    ) THEN RAISE(ABORT, 'shared LLM token budget is below existing usage') END;
END;
