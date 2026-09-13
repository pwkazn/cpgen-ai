package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestPhysicalFormatRepairContainsOnlyEligibleSanitizedCodes(t *testing.T) {
	for _, tc := range []struct {
		name, content, finish string
		status                int
		want                  port.StructuredOutputErrorCode
	}{
		{name: "syntax", content: `{"secret-unclosed":"private-response`, want: port.StructuredOutputInvalidJSON},
		{name: "duplicate", content: `{"schema_version":"cpgen.idea/v1","title":"a","title":"private-response"}`, want: port.StructuredOutputDuplicateField},
		{name: "unknown-field", content: `{"schema_version":"cpgen.idea/v1","title":"a","private-field":"private-response"}`, want: port.StructuredOutputUnknownField},
		{name: "schema-missing", content: `{"title":"private-response"}`, want: port.StructuredOutputSchemaMissing},
		{name: "truncated", content: `{"schema_version":"cpgen.idea/v1","title":"ok"}`, finish: "length"},
		{name: "filtered", content: `{"schema_version":"cpgen.idea/v1","title":"ok"}`, finish: "content_filter"},
		{name: "rate-limit", content: `broken`, status: 429},
		{name: "auth", content: `broken`, status: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "fixture", "choices": []any{map[string]any{"message": map[string]string{"content": tc.content}, "finish_reason": tc.finish}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
			}))
			defer server.Close()
			model, err := NewLangChain(testConfig(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			result, err := model.GeneratePhysical(context.Background(), testGenerateRequest(), "call_00000000000000000000000000000e01")
			if err != nil || result.Execution.Value != nil || result.Execution.Failure == nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if tc.want == "" {
				if result.FormatRepair != nil {
					t.Fatalf("ineligible response allows format repair: %+v", result.FormatRepair)
				}
				return
			}
			if result.FormatRepair == nil || len(result.FormatRepair.ErrorCodes) != 1 || result.FormatRepair.ErrorCodes[0] != tc.want {
				t.Fatalf("repair=%+v want=%s", result.FormatRepair, tc.want)
			}
			if err := result.FormatRepair.Validate(port.DefaultRepairInputMaxBytes); err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(result.FormatRepair)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.FormatRepair.FieldPaths) != 0 || len(result.FormatRepair.InvalidFragment) != 0 || strings.Contains(string(raw), "private") {
				t.Fatal("provider text leaked into repair evidence")
			}
			if !result.UsageVerified || result.Usage.InputTokens != 3 || result.Usage.OutputTokens != 4 {
				t.Fatal("validation rejection lost provider usage")
			}
		})
	}
}

func TestPhysicalFormatRepairRejectsEnvelopeAndDomainFailures(t *testing.T) {
	for _, mode := range []string{"envelope", "domain"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if mode == "envelope" {
					_, _ = io.WriteString(w, `{"private"`)
				} else {
					_, _ = io.WriteString(w, langchainSuccess)
				}
			}))
			defer server.Close()
			cfg := testConfig(server.URL)
			if mode == "domain" {
				registry, err := port.NewSchemaValidatorRegistry(port.SchemaValidatorDefinition{Schema: testGenerateRequest().Schema, Validate: func([]byte, port.OutputSchemaRef, int64) error { return errors.New("private domain violation") }})
				if err != nil {
					t.Fatal(err)
				}
				cfg.SchemaRegistry = registry
			}
			model, err := NewLangChain(cfg)
			if err != nil {
				t.Fatal(err)
			}
			result, err := model.GeneratePhysical(context.Background(), testGenerateRequest(), domain.AttemptCallID("call_00000000000000000000000000000e02"))
			if err != nil || result.Execution.Failure == nil || result.FormatRepair != nil {
				t.Fatalf("unexpected repair=%+v err=%v", result.FormatRepair, err)
			}
		})
	}
}
