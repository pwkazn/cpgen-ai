package domain

import "math"

// UsesTokenBudget reports whether input and output share one run allowance.
// TokenBudget remains true when that allowance is explicitly zero.
func (v BudgetLimits) UsesTokenBudget() bool {
	return v.TokenBudget || v.MaxLLMTokens > 0
}

// NewTokenBudgetLimits preserves the earlier shared-token request format.
// New requests use NewSplitTokenBudgetLimits for independent allowances.
func NewTokenBudgetLimits(tokens int64) (BudgetLimits, error) {
	v := BudgetLimits{
		MaxLLMTokens:       tokens,
		TokenBudget:        true,
		MaxLLMCalls:        1024,
		MaxSimilarityCalls: 16,
		// Separate token and cost accounts retain usage evidence. The shared
		// token allowance controls model consumption; these accounting ceilings
		// must not silently introduce smaller user budgets.
		MaxLLMInputTokens:         math.MaxInt64,
		MaxLLMOutputTokens:        math.MaxInt64,
		MaxLLMCostMicroUSD:        math.MaxInt64,
		MaxSimilarityCostMicroUSD: math.MaxInt64,
		MaxSandboxCreates:         4096,
		MaxArtifactBytes:          1 << 30,
		MaxPackageBytes:           64 << 20,
		MaxMutationsPerStage:      0,
		MaxActiveTimeMilliseconds: 60 * 60 * 1000,
	}
	return v, v.Validate()
}

// NewSplitTokenBudgetLimits expands independent input/output allowances into
// the frozen execution policy. Neither allowance can borrow from the other.
// System defaults are persisted so an existing run keeps its policy on resume.
func NewSplitTokenBudgetLimits(inputTokens, outputTokens int64) (BudgetLimits, error) {
	v, err := NewTokenBudgetLimits(0)
	if err != nil {
		return BudgetLimits{}, err
	}
	v.TokenBudget = false
	v.SplitTokenBudget = true
	v.MaxLLMInputTokens = inputTokens
	v.MaxLLMOutputTokens = outputTokens
	return v, v.Validate()
}
