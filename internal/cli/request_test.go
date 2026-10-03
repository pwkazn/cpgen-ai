package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/domain"
)

func writeRequestInput(t *testing.T, format, budget string) string {
	t.Helper()
	var document string
	if format == "yaml" {
		document = "schema_version: cpgen.request/v1\nmode: manual\nbrief: Graph problem\nlanguage: en\ndifficulty: easy\ntime_limit_milliseconds: 1000\nmemory_limit_megabytes: 64\nsolution_language: cpp\nverification_profile: default\nbudget_limits:\n" + budget
	} else {
		document = `{"schema_version":"cpgen.request/v1","mode":"manual","brief":"Graph problem","language":"en","difficulty":"easy","time_limit_milliseconds":1000,"memory_limit_megabytes":64,"solution_language":"cpp","verification_profile":"default","budget_limits":` + budget + `}`
	}
	path := filepath.Join(t.TempDir(), "request."+format)
	if err := os.WriteFile(path, []byte(document), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadRequestTokenBudgetUsesSystemLimitsAndExactIntegers(t *testing.T) {
	for _, test := range []struct {
		name, format, budget string
		tokens               int64
	}{
		{"yaml", "yaml", "  max_llm_tokens: 100000\n", 100000},
		{"json", "json", `{"max_llm_tokens":100000}`, 100000},
		{"yaml_zero", "yaml", "  max_llm_tokens: 0\n", 0},
		{"json_zero", "json", `{"max_llm_tokens":0}`, 0},
		{"yaml_large", "yaml", "  max_llm_tokens: 9007199254740993\n", 9007199254740993},
		{"json_large", "json", `{"max_llm_tokens":9007199254740993}`, 9007199254740993},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := loadRequest(writeRequestInput(t, test.format, test.budget))
			if err != nil {
				t.Fatal(err)
			}
			limits := request.BudgetLimits
			if !limits.UsesTokenBudget() || limits.MaxLLMTokens != test.tokens {
				t.Fatalf("token limit lost or decoded imprecisely: %+v", limits)
			}
			if limits.MaxLLMCalls <= 0 || limits.MaxActiveTimeMilliseconds <= 0 || limits.MaxSandboxCreates <= 0 || limits.MaxArtifactBytes <= 0 {
				t.Fatalf("system execution limits missing: %+v", limits)
			}
		})
	}
}

func TestLoadRequestSplitTokenBudgetUsesSystemLimitsAndExactIntegers(t *testing.T) {
	for _, test := range []struct {
		name, format, budget string
		input, output        int64
	}{
		{"yaml", "yaml", "  max_llm_input_tokens: 100000\n  max_llm_output_tokens: 20000\n", 100000, 20000},
		{"json", "json", `{"max_llm_input_tokens":100000,"max_llm_output_tokens":20000}`, 100000, 20000},
		{"yaml_zero", "yaml", "  max_llm_input_tokens: 0\n  max_llm_output_tokens: 0\n", 0, 0},
		{"json_zero", "json", `{"max_llm_input_tokens":0,"max_llm_output_tokens":0}`, 0, 0},
		{"zero_input", "json", `{"max_llm_input_tokens":0,"max_llm_output_tokens":20000}`, 0, 20000},
		{"zero_output", "yaml", "  max_llm_input_tokens: 100000\n  max_llm_output_tokens: 0\n", 100000, 0},
		{"yaml_large", "yaml", "  max_llm_input_tokens: 9007199254740993\n  max_llm_output_tokens: 9007199254740995\n", 9007199254740993, 9007199254740995},
		{"json_large", "json", `{"max_llm_input_tokens":9007199254740993,"max_llm_output_tokens":9007199254740995}`, 9007199254740993, 9007199254740995},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := loadRequest(writeRequestInput(t, test.format, test.budget))
			if err != nil {
				t.Fatal(err)
			}
			limits := request.BudgetLimits
			if !limits.SplitTokenBudget || limits.UsesTokenBudget() || limits.MaxLLMInputTokens != test.input || limits.MaxLLMOutputTokens != test.output {
				t.Fatalf("separate token limits lost or decoded imprecisely: %+v", limits)
			}
			if limits.MaxLLMCalls <= 0 || limits.MaxActiveTimeMilliseconds <= 0 || limits.MaxSandboxCreates <= 0 || limits.MaxArtifactBytes <= 0 {
				t.Fatalf("system execution limits missing: %+v", limits)
			}
		})
	}
}

func TestLoadRequestRejectsInvalidTokenBudget(t *testing.T) {
	for _, test := range []struct{ name, format, budget string }{
		{"negative", "yaml", "  max_llm_tokens: -1\n"},
		{"fraction", "json", `{"max_llm_tokens":1.5}`},
		{"overflow", "yaml", "  max_llm_tokens: 9223372036854775808\n"},
		{"null", "json", `{"max_llm_tokens":null}`},
		{"string", "json", `{"max_llm_tokens":"100000"}`},
		{"mixed_yaml", "yaml", "  max_llm_tokens: 100000\n  max_llm_calls: 0\n"},
		{"mixed_json", "json", `{"max_llm_tokens":100000,"max_llm_input_tokens":0}`},
		{"internal_marker", "json", `{"token_budget":false}`},
		{"unknown", "yaml", "  max_llm_token: 100000\n"},
		{"duplicate", "json", `{"max_llm_tokens":1,"max_llm_tokens":2}`},
		{"split_input_missing", "yaml", "  max_llm_output_tokens: 1\n"},
		{"split_output_missing", "json", `{"max_llm_input_tokens":1}`},
		{"split_negative_input", "yaml", "  max_llm_input_tokens: -1\n  max_llm_output_tokens: 1\n"},
		{"split_negative_output", "json", `{"max_llm_input_tokens":1,"max_llm_output_tokens":-1}`},
		{"split_null_input", "json", `{"max_llm_input_tokens":null,"max_llm_output_tokens":1}`},
		{"split_null_output", "yaml", "  max_llm_input_tokens: 1\n  max_llm_output_tokens: null\n"},
		{"split_fraction", "json", `{"max_llm_input_tokens":1,"max_llm_output_tokens":1.5}`},
		{"split_overflow", "yaml", "  max_llm_input_tokens: 9223372036854775808\n  max_llm_output_tokens: 1\n"},
		{"split_string", "json", `{"max_llm_input_tokens":1,"max_llm_output_tokens":"1"}`},
		{"split_internal_marker", "json", `{"split_token_budget":false}`},
		{"split_total_mix", "json", `{"max_llm_tokens":1,"max_llm_input_tokens":1,"max_llm_output_tokens":1}`},
		{"split_duplicate", "yaml", "  max_llm_input_tokens: 1\n  max_llm_output_tokens: 1\n  max_llm_input_tokens: 2\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := loadRequest(writeRequestInput(t, test.format, test.budget)); err == nil {
				t.Fatal("invalid budget accepted")
			}
		})
	}
}

func TestLoadRequestPreservesLegacyBudgetIdentity(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			budget := "  max_llm_calls: 2\n  max_llm_input_tokens: 70000\n  max_llm_output_tokens: 3000\n  max_active_time_milliseconds: 5000\n"
			if format == "json" {
				budget = `{"max_llm_calls":2,"max_llm_input_tokens":70000,"max_llm_output_tokens":3000,"max_active_time_milliseconds":5000}`
			}
			request, err := loadRequest(writeRequestInput(t, format, budget))
			if err != nil {
				t.Fatal(err)
			}
			want := domain.BudgetLimits{MaxLLMCalls: 2, MaxLLMInputTokens: 70000, MaxLLMOutputTokens: 3000, MaxActiveTimeMilliseconds: 5000}
			if request.BudgetLimits != want {
				t.Fatalf("legacy limits changed: %+v", request.BudgetLimits)
			}
			raw, err := json.Marshal(request.BudgetLimits)
			if err != nil {
				t.Fatal(err)
			}
			const legacyJSON = `{"max_llm_calls":2,"max_similarity_calls":0,"max_llm_input_tokens":70000,"max_llm_output_tokens":3000,"max_llm_cost_micro_usd":0,"max_similarity_cost_micro_usd":0,"max_sandbox_creates":0,"max_artifact_bytes":0,"max_package_bytes":0,"max_mutations_per_stage":0,"max_active_time_milliseconds":5000}`
			if string(raw) != legacyJSON {
				t.Fatalf("legacy canonical budget representation changed: %s", raw)
			}
		})
	}
}

func TestDecodeBudgetPatchKeepsTokenIncreaseSparse(t *testing.T) {
	for _, data := range []string{`{"max_llm_tokens":9007199254740993}`, "max_llm_tokens: 9007199254740993\n"} {
		patch, err := decodeBudgetPatch([]byte(data))
		if err != nil {
			t.Fatal(err)
		}
		if patch != (domain.BudgetLimits{MaxLLMTokens: 9007199254740993}) {
			t.Fatalf("retry patch unexpectedly raises system limits: %+v", patch)
		}
	}
	for _, data := range []string{
		`{"max_llm_tokens":1,"max_active_time_milliseconds":0}`,
		`{"max_llm_tokens":-1}`, `{"max_llm_tokens":null}`, `{"token_budget":true}`,
		`{"unknown":1}`, `{"max_llm_tokens":1,"max_llm_tokens":2}`,
		"max_llm_tokens: 1\n---\nmax_llm_tokens: 2\n",
	} {
		t.Run(strings.ReplaceAll(data, "\n", " "), func(t *testing.T) {
			if _, err := decodeBudgetPatch([]byte(data)); err == nil {
				t.Fatal("invalid budget patch accepted")
			}
		})
	}
}

func TestDecodeBudgetPatchKeepsSeparateTokenIncreasesSparse(t *testing.T) {
	for _, test := range []struct {
		data string
		want domain.BudgetLimits
	}{
		{`{"max_llm_input_tokens":9007199254740993}`, domain.BudgetLimits{MaxLLMInputTokens: 9007199254740993}},
		{"max_llm_output_tokens: 9007199254740995\n", domain.BudgetLimits{MaxLLMOutputTokens: 9007199254740995}},
		{`{"max_llm_input_tokens":100000,"max_llm_output_tokens":20000}`, domain.BudgetLimits{MaxLLMInputTokens: 100000, MaxLLMOutputTokens: 20000}},
		{"max_llm_input_tokens: 100000\nmax_llm_output_tokens: 20000\n", domain.BudgetLimits{MaxLLMInputTokens: 100000, MaxLLMOutputTokens: 20000}},
	} {
		patch, err := decodeBudgetPatch([]byte(test.data))
		if err != nil {
			t.Fatal(err)
		}
		if patch != test.want {
			t.Fatalf("retry patch unexpectedly raises unrelated limits: %+v", patch)
		}
	}
	for _, data := range []string{
		`{"max_llm_input_tokens":1,"max_llm_tokens":1}`,
		`{"max_llm_input_tokens":-1}`, `{"max_llm_output_tokens":-1}`,
		`{"max_llm_input_tokens":null}`, `{"max_llm_output_tokens":null}`,
		`{"split_token_budget":true}`, `{"max_llm_input_tokens":1,"max_llm_input_tokens":2}`,
		"max_llm_output_tokens: 1\n---\nmax_llm_output_tokens: 2\n",
	} {
		t.Run(strings.ReplaceAll(data, "\n", " "), func(t *testing.T) {
			if _, err := decodeBudgetPatch([]byte(data)); err == nil {
				t.Fatal("invalid budget patch accepted")
			}
		})
	}
}
