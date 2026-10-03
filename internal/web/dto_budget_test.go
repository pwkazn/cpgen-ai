package web

import (
	"encoding/json"
	"math"
	"strconv"
	"testing"

	"cpgen/internal/domain"
)

func TestDecodeRunRequestWithIndependentTokenBudgets(t *testing.T) {
	for _, tc := range []struct {
		input, output int64
	}{
		{0, 0},
		{150000, 50000},
		{9007199254740993, 9},
		{math.MaxInt64, math.MaxInt64},
	} {
		raw := `{"max_llm_input_tokens":"` + strconv.FormatInt(tc.input, 10) + `","max_llm_output_tokens":"` + strconv.FormatInt(tc.output, 10) + `"}`
		t.Run(raw, func(t *testing.T) {
			req, err := decodeRunRequest(tokenBudgetRequestJSON(t, raw))
			if err != nil {
				t.Fatal(err)
			}
			limits := req.BudgetLimits
			if !limits.SplitTokenBudget || limits.UsesTokenBudget() || limits.MaxLLMInputTokens != tc.input || limits.MaxLLMOutputTokens != tc.output {
				t.Fatalf("independent token budgets lost their mode or precision: %+v", limits)
			}
			if limits.MaxLLMCalls <= 0 || limits.MaxActiveTimeMilliseconds <= 0 {
				t.Fatalf("system execution limits were not supplied: %+v", limits)
			}
		})
	}
}

func TestDecodeRunRequestRequiresBothIndependentTokenBudgets(t *testing.T) {
	for _, raw := range []string{
		`{"max_llm_input_tokens":"10"}`,
		`{"max_llm_output_tokens":"10"}`,
		`{"max_llm_input_tokens":"","max_llm_output_tokens":"10"}`,
		`{"max_llm_input_tokens":"10","max_llm_output_tokens":null}`,
		`{"max_llm_input_tokens":"-1","max_llm_output_tokens":"10"}`,
		`{"max_llm_input_tokens":"10","max_llm_output_tokens":"-1"}`,
		`{"max_llm_input_tokens":"10","max_llm_output_tokens":"9223372036854775808"}`,
		`{"max_llm_input_tokens":10,"max_llm_output_tokens":"10"}`,
		`{"max_llm_input_tokens":"10","max_llm_output_tokens":"10","split_token_budget":true}`,
	} {
		t.Run(raw, func(t *testing.T) {
			if _, err := decodeRunRequest(tokenBudgetRequestJSON(t, raw)); err == nil {
				t.Fatal("invalid independent token budgets accepted")
			}
		})
	}
}

func TestParseIndependentTokenBudgetIncreasesRemainSparse(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want domain.BudgetLimits
	}{
		{`{"max_llm_input_tokens":"9007199254740993"}`, domain.BudgetLimits{MaxLLMInputTokens: 9007199254740993}},
		{`{"max_llm_output_tokens":"8000"}`, domain.BudgetLimits{MaxLLMOutputTokens: 8000}},
		{`{"max_llm_input_tokens":"100","max_llm_output_tokens":"50"}`, domain.BudgetLimits{MaxLLMInputTokens: 100, MaxLLMOutputTokens: 50}},
		{`{"max_llm_input_tokens":"0","max_llm_output_tokens":"50"}`, domain.BudgetLimits{MaxLLMOutputTokens: 50}},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			var dto budgetDTO
			if err := json.Unmarshal([]byte(tc.raw), &dto); err != nil {
				t.Fatal(err)
			}
			increase, err := parseBudget(dto)
			if err != nil {
				t.Fatal(err)
			}
			if increase != tc.want {
				t.Fatalf("review delta changed other limits: got %+v, want %+v", increase, tc.want)
			}
		})
	}
}

func TestParseIndependentTokenBudgetIncreaseRejectsInvalidValues(t *testing.T) {
	for _, raw := range []string{
		`{"max_llm_input_tokens":"-1"}`,
		`{"max_llm_output_tokens":"9223372036854775808"}`,
		`{"max_llm_input_tokens":""}`,
		`{"max_llm_output_tokens":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var dto budgetDTO
			if err := json.Unmarshal([]byte(raw), &dto); err != nil {
				t.Fatal(err)
			}
			if _, err := parseBudget(dto); err == nil {
				t.Fatal("invalid review token increase accepted")
			}
		})
	}
}

func TestDecodeRunRequestWithTotalTokenBudget(t *testing.T) {
	for _, tokens := range []int64{0, 200000, 9007199254740993} {
		t.Run(strconv.FormatInt(tokens, 10), func(t *testing.T) {
			req, err := decodeRunRequest(tokenBudgetRequestJSON(t, `{"max_llm_tokens":"`+strconv.FormatInt(tokens, 10)+`"}`))
			if err != nil {
				t.Fatal(err)
			}
			if !req.BudgetLimits.UsesTokenBudget() || req.BudgetLimits.MaxLLMTokens != tokens {
				t.Fatalf("token budget lost its mode or precision: %+v", req.BudgetLimits)
			}
			if req.BudgetLimits.MaxLLMCalls <= 0 || req.BudgetLimits.MaxActiveTimeMilliseconds <= 0 {
				t.Fatalf("system execution limits were not supplied: %+v", req.BudgetLimits)
			}
		})
	}
}

func TestDecodeRunRequestRejectsInvalidTotalTokenBudgets(t *testing.T) {
	for _, budget := range []string{
		`{"max_llm_tokens":"-1"}`,
		`{"max_llm_tokens":"9223372036854775808"}`,
		`{"max_llm_tokens":"1.5"}`,
		`{"max_llm_tokens":""}`,
		`{"max_llm_tokens":null}`,
		`{"max_llm_tokens":200000}`,
		`{"max_llm_tokens":"200000","max_llm_calls":"0"}`,
		`{"max_llm_tokens":"200000","max_llm_calls":""}`,
		`{"max_llm_tokens":"200000","max_llm_calls":null}`,
		`{"max_llm_tokens":"200000","token_budget":true}`,
		`{"max_llm_tokens":"200000","unknown":"1"}`,
	} {
		t.Run(budget, func(t *testing.T) {
			if _, err := decodeRunRequest(tokenBudgetRequestJSON(t, budget)); err == nil {
				t.Fatal("invalid or mixed budget was accepted")
			}
		})
	}
}

func TestParseTokenBudgetIncreaseDoesNotIncreaseSystemLimits(t *testing.T) {
	var dto budgetDTO
	if err := json.Unmarshal([]byte(`{"max_llm_tokens":"9007199254740993"}`), &dto); err != nil {
		t.Fatal(err)
	}
	increase, err := parseBudget(dto)
	if err != nil {
		t.Fatal(err)
	}
	if increase != (domain.BudgetLimits{MaxLLMTokens: 9007199254740993}) {
		t.Fatalf("review delta must contain only token increase: %+v", increase)
	}
}

func TestParseTokenBudgetIncreaseRejectsMixedAndInvalidFields(t *testing.T) {
	for _, raw := range []string{
		`{"max_llm_tokens":"-1"}`,
		`{"max_llm_tokens":"9223372036854775808"}`,
		`{"max_llm_tokens":""}`,
		`{"max_llm_tokens":null}`,
		`{"max_llm_tokens":"1","max_llm_input_tokens":"0"}`,
		`{"max_llm_tokens":"1","max_llm_input_tokens":""}`,
		`{"max_llm_tokens":"1","max_llm_input_tokens":null}`,
	} {
		t.Run(raw, func(t *testing.T) {
			var dto budgetDTO
			if err := json.Unmarshal([]byte(raw), &dto); err != nil {
				t.Fatal(err)
			}
			if _, err := parseBudget(dto); err == nil {
				t.Fatal("invalid or mixed review budget was accepted")
			}
		})
	}
}

func tokenBudgetRequestJSON(t *testing.T, budget string) []byte {
	t.Helper()
	return mustJSONForCreateTest(t, map[string]any{
		"schema_version": "cpgen.request/v1", "mode": "manual", "brief": "token budget fixture",
		"tags": []string{}, "normalized_tags": []string{}, "language": "en", "difficulty": "easy",
		"time_limit_milliseconds": "1000", "memory_limit_megabytes": "64", "solution_language": "cpp",
		"verification_profile": "default", "export_targets": []string{"internal"},
		"budget_limits": json.RawMessage(budget),
	})
}
