package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"cpgen/internal/domain"
)

const activeTimeNSPerMillisecond int64 = int64(time.Millisecond)

func insertInitialRunBudgetBaseline(ctx context.Context, tx *immediateTx, runID domain.RunID, limits domain.BudgetLimits) error {
	var tableCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='run_budget_baselines'`).Scan(&tableCount); err != nil {
		return err
	}
	if tableCount == 0 {
		// A few historical-upgrade tests intentionally operate against an older
		// schema before reopening it through the migration runner. Production
		// stores always have this table before CreateRun is exposed.
		return nil
	}
	tokenColumns, tokenValues := "", ""
	if limits.UsesTokenBudget() {
		tokenColumns, tokenValues = ",max_llm_tokens,token_budget", ",?,?"
	}
	query := `INSERT INTO run_budget_baselines(
		run_id,max_llm_calls,max_similarity_calls,max_llm_input_tokens,max_llm_output_tokens,
		max_llm_cost_micro_usd,max_similarity_cost_micro_usd,max_sandbox_creates,max_artifact_bytes,
		max_package_bytes,max_mutations_per_stage,max_active_time_ns` + tokenColumns + `
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?` + tokenValues + `)`
	args := []any{
		string(runID), limits.MaxLLMCalls, limits.MaxSimilarityCalls, limits.MaxLLMInputTokens, limits.MaxLLMOutputTokens,
		limits.MaxLLMCostMicroUSD, limits.MaxSimilarityCostMicroUSD, limits.MaxSandboxCreates, limits.MaxArtifactBytes,
		limits.MaxPackageBytes, limits.MaxMutationsPerStage, limits.MaxActiveTimeMilliseconds * activeTimeNSPerMillisecond,
	}
	if limits.UsesTokenBudget() {
		args = append(args, limits.MaxLLMTokens, limits.TokenBudget)
	}
	_, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("insert immutable run budget baseline: %w", err)
	}
	return nil
}

type reviewBudgetField struct {
	name      string
	delta     int64
	runColumn string
	dimension domain.BudgetDimension
	factor    int64
}

func reviewBudgetFields(increase domain.BudgetLimits) []reviewBudgetField {
	fields := []reviewBudgetField{
		{name: "max_llm_tokens", delta: increase.MaxLLMTokens, runColumn: "max_llm_tokens", factor: 1},
		{name: "max_llm_calls", delta: increase.MaxLLMCalls, runColumn: "max_llm_calls", dimension: domain.BudgetLLMCalls, factor: 1},
		{name: "max_similarity_calls", delta: increase.MaxSimilarityCalls, runColumn: "max_similarity_calls", dimension: domain.BudgetSimilarityCalls, factor: 1},
		{name: "max_llm_input_tokens", delta: increase.MaxLLMInputTokens, runColumn: "max_llm_input_tokens", dimension: domain.BudgetLLMInputTokens, factor: 1},
		{name: "max_llm_output_tokens", delta: increase.MaxLLMOutputTokens, runColumn: "max_llm_output_tokens", dimension: domain.BudgetLLMOutputTokens, factor: 1},
		{name: "max_llm_cost_micro_usd", delta: increase.MaxLLMCostMicroUSD, runColumn: "max_llm_cost_micro_usd", dimension: domain.BudgetExternalCostMicroUSD, factor: 1},
		{name: "max_similarity_cost_micro_usd", delta: increase.MaxSimilarityCostMicroUSD, runColumn: "max_similarity_cost_micro_usd", dimension: domain.BudgetSimilarityCostMicroUSD, factor: 1},
		{name: "max_sandbox_creates", delta: increase.MaxSandboxCreates, runColumn: "max_sandbox_creates", dimension: domain.BudgetDockerContainerCreates, factor: 1},
		{name: "max_artifact_bytes", delta: increase.MaxArtifactBytes, runColumn: "max_artifact_bytes", dimension: domain.BudgetArtifactPhysicalNewBytes, factor: 1},
		{name: "max_package_bytes", delta: increase.MaxPackageBytes, runColumn: "max_package_bytes", factor: 1},
		{name: "max_active_time_milliseconds", delta: increase.MaxActiveTimeMilliseconds, runColumn: "max_active_time_ns", dimension: domain.BudgetActiveTimeNS, factor: activeTimeNSPerMillisecond},
	}
	result := make([]reviewBudgetField, 0, len(fields))
	for _, field := range fields {
		if field.delta > 0 {
			result = append(result, field)
		}
	}
	return result
}

type runBudgetCaps struct {
	llmTokens                                                  int64
	tokenBudget                                                bool
	llmCalls, similarityCalls, llmInputTokens, llmOutputTokens int64
	llmCost, similarityCost, sandboxCreates, artifactBytes     int64
	packageBytes, mutationQuota, activeTimeNS                  int64
}

func readRunBudgetCapsTx(ctx context.Context, tx rowQuerier, runID domain.RunID) (runBudgetCaps, error) {
	var caps runBudgetCaps
	err := tx.QueryRowContext(ctx, `SELECT max_llm_calls,max_similarity_calls,max_llm_input_tokens,max_llm_output_tokens,
		max_llm_cost_micro_usd,max_similarity_cost_micro_usd,max_sandbox_creates,max_artifact_bytes,
		max_package_bytes,max_mutations_per_stage,max_active_time_ns,max_llm_tokens,token_budget FROM runs WHERE run_id=?`, string(runID)).Scan(
		&caps.llmCalls, &caps.similarityCalls, &caps.llmInputTokens, &caps.llmOutputTokens,
		&caps.llmCost, &caps.similarityCost, &caps.sandboxCreates, &caps.artifactBytes,
		&caps.packageBytes, &caps.mutationQuota, &caps.activeTimeNS, &caps.llmTokens, &caps.tokenBudget,
	)
	return caps, err
}

func budgetCapValue(caps runBudgetCaps, column string) int64 {
	switch column {
	case "max_llm_tokens":
		return caps.llmTokens
	case "max_llm_calls":
		return caps.llmCalls
	case "max_similarity_calls":
		return caps.similarityCalls
	case "max_llm_input_tokens":
		return caps.llmInputTokens
	case "max_llm_output_tokens":
		return caps.llmOutputTokens
	case "max_llm_cost_micro_usd":
		return caps.llmCost
	case "max_similarity_cost_micro_usd":
		return caps.similarityCost
	case "max_sandbox_creates":
		return caps.sandboxCreates
	case "max_artifact_bytes":
		return caps.artifactBytes
	case "max_package_bytes":
		return caps.packageBytes
	case "max_active_time_ns":
		return caps.activeTimeNS
	default:
		panic("unknown compiled review budget column")
	}
}

func validateReviewBudgetFitsTx(ctx context.Context, tx rowQuerier, runID domain.RunID, increase domain.BudgetLimits) error {
	caps, err := readRunBudgetCapsTx(ctx, tx, runID)
	if err != nil {
		return err
	}
	if increase.MaxLLMTokens > 0 && !caps.tokenBudget && caps.llmTokens == 0 {
		return wrap(ErrConsistency, "a shared token increase requires a token-budget run", nil)
	}
	var baselineCount int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM run_budget_baselines WHERE run_id=?`, string(runID)).Scan(&baselineCount); err != nil {
		return err
	}
	if baselineCount != 1 {
		return wrap(ErrConsistency, "run budget baseline is missing", nil)
	}
	for _, field := range reviewBudgetFields(increase) {
		if field.delta > math.MaxInt64/field.factor {
			return wrap(ErrConsistency, "review budget increase overflows its stored unit", nil)
		}
		unitDelta := field.delta * field.factor
		current := budgetCapValue(caps, field.runColumn)
		if current < 0 || unitDelta > math.MaxInt64-current {
			return wrap(ErrConsistency, fmt.Sprintf("review budget increase overflows %s", field.name), nil)
		}
		if field.dimension == "" {
			continue
		}
		var limit, reserved, consumed, version int64
		var snapshotDigest string
		if err := tx.QueryRowContext(ctx, `SELECT limit_value,reserved_value,consumed_value,account_version,request_snapshot_digest
			FROM budget_accounts WHERE run_id=? AND dimension=?`, string(runID), field.dimension).Scan(
			&limit, &reserved, &consumed, &version, &snapshotDigest,
		); errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrConsistency, fmt.Sprintf("%s budget account is missing", field.dimension), err)
		} else if err != nil {
			return err
		}
		if limit != current || version <= 0 || version == math.MaxInt64 || reserved < 0 || consumed < 0 || consumed > limit || reserved > limit-consumed {
			return wrap(ErrConsistency, fmt.Sprintf("%s budget account differs from the run projection", field.dimension), nil)
		}
		var requestDigest string
		if err := tx.QueryRowContext(ctx, `SELECT submitted_request_digest FROM runs WHERE run_id=?`, string(runID)).Scan(&requestDigest); err != nil {
			return err
		}
		if snapshotDigest != requestDigest {
			return wrap(ErrConsistency, fmt.Sprintf("%s budget account is detached from its submitted request", field.dimension), nil)
		}
	}
	return nil
}

func validateReviewBudgetGrantsTx(ctx context.Context, tx *immediateTx, decision domain.ReviewDecision) error {
	want := make(map[string]int64)
	for _, field := range reviewBudgetFields(decision.BudgetIncrease) {
		want[field.name] = field.delta
	}
	rows, err := tx.QueryContext(ctx, `SELECT field,delta_value FROM review_budget_grants WHERE review_id=? ORDER BY field`, string(decision.ID))
	if err != nil {
		return err
	}
	got := make(map[string]int64)
	for rows.Next() {
		var name string
		var delta int64
		if err := rows.Scan(&name, &delta); err != nil {
			_ = rows.Close()
			return err
		}
		got[name] = delta
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(got) != len(want) {
		return wrap(ErrConsistency, "review budget grants do not match the approved amount", nil)
	}
	for name, delta := range want {
		if got[name] != delta {
			return wrap(ErrConsistency, "review budget grant differs from the approved amount", nil)
		}
	}
	return nil
}

func applyRetryBudgetTx(ctx context.Context, tx *immediateTx, decision domain.ReviewDecision, state domain.RunState, version int64, at time.Time) error {
	if decision.BudgetIncrease.MaxMutationsPerStage != 0 {
		return wrap(ErrConsistency, "stored RETRY attempts to extend mutation quota bound to the submitted request", nil)
	}
	if err := validateReviewBudgetGrantsTx(ctx, tx, decision); err != nil {
		return err
	}
	if err := validateReviewBudgetFitsTx(ctx, tx, decision.RunID, decision.BudgetIncrease); err != nil {
		return err
	}
	fields := reviewBudgetFields(decision.BudgetIncrease)
	for _, field := range fields {
		if _, err := tx.ExecContext(ctx, `INSERT INTO review_budget_applications(review_id,run_id,field,delta_value,applied_at,source)
			VALUES (?,?,?,?,?,'LIVE_APPLY')`, string(decision.ID), string(decision.RunID), field.name, field.delta, formatTime(at)); err != nil {
			return fmt.Errorf("record approved %s budget increase: %w", field.name, err)
		}
	}
	values := make(map[string]int64, len(fields))
	for _, field := range fields {
		values[field.runColumn] = field.delta * field.factor
	}
	result, err := tx.ExecContext(ctx, `UPDATE runs SET
		max_llm_calls=max_llm_calls+?, max_similarity_calls=max_similarity_calls+?,
		max_llm_input_tokens=max_llm_input_tokens+?, max_llm_output_tokens=max_llm_output_tokens+?,
		max_llm_cost_micro_usd=max_llm_cost_micro_usd+?, max_similarity_cost_micro_usd=max_similarity_cost_micro_usd+?,
		max_sandbox_creates=max_sandbox_creates+?, max_artifact_bytes=max_artifact_bytes+?,
		max_package_bytes=max_package_bytes+?, max_active_time_ns=max_active_time_ns+?, max_llm_tokens=max_llm_tokens+?,
		state=?,version=?,updated_at=? WHERE run_id=?`,
		values["max_llm_calls"], values["max_similarity_calls"], values["max_llm_input_tokens"], values["max_llm_output_tokens"],
		values["max_llm_cost_micro_usd"], values["max_similarity_cost_micro_usd"], values["max_sandbox_creates"], values["max_artifact_bytes"],
		values["max_package_bytes"], values["max_active_time_ns"], values["max_llm_tokens"], string(state), version, formatTime(at), string(decision.RunID),
	)
	if err != nil {
		return fmt.Errorf("apply retry run budget projection: %w", err)
	}
	if count, err := result.RowsAffected(); err != nil {
		return err
	} else if count != 1 {
		return wrap(ErrConsistency, "retry budget run projection was not updated", nil)
	}
	for _, field := range fields {
		if field.dimension == "" {
			continue
		}
		result, err := tx.ExecContext(ctx, `UPDATE budget_accounts
			SET limit_value=limit_value+?,account_version=account_version+1
			WHERE run_id=? AND dimension=? AND request_snapshot_digest=(SELECT submitted_request_digest FROM runs WHERE run_id=?)
			  AND account_version < 9223372036854775807`,
			field.delta*field.factor, string(decision.RunID), field.dimension, string(decision.RunID),
		)
		if err != nil {
			return fmt.Errorf("apply %s budget account increase: %w", field.dimension, err)
		}
		if count, err := result.RowsAffected(); err != nil {
			return err
		} else if count != 1 {
			return wrap(ErrConsistency, fmt.Sprintf("%s budget account was not updated", field.dimension), nil)
		}
	}
	return nil
}
