package application_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"cpgen/internal/agent"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestDurableLLMCountsHTTPRetriesAndSettlesEveryReservation(t *testing.T) {
	t.Setenv("CPGEN_DURABLE_TEST_KEY", "fixture-key")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":"private"}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"test-provider","choices":[{"message":{"content":"{\"schema_version\":\"cpgen.idea/v1\",\"title\":\"ok\"}"}}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	}))
	defer server.Close()
	model, request := durableTestProvider(t, server.URL)
	fixture := newCoordinatorFixture(t, "a1", domain.BudgetLimits{MaxLLMCalls: 3, MaxLLMInputTokens: 10000, MaxLLMOutputTokens: 1000, MaxLLMCostMicroUSD: 1000})
	open := fixture.openRequest(501)
	request.LogicalIdempotencyKey = open.LogicalOperationID
	plan, err := model.PlanGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	open.Provider, open.RequestDigest, open.PolicyDigest = plan.Provider, plan.RequestDigest, request.ProviderPolicyDigest
	callsService, err := application.NewLLMCalls(fixture.store, model, fixture.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := callsService.Generate(context.Background(), open, request)
	if err != nil || outcome.Value == nil {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	if calls != 2 || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 2 || !outcome.Value.CallTrace.Equal(outcome.CallTrace) {
		t.Fatalf("calls=%d trace=%+v", calls, outcome.CallTrace)
	}
	prepared, err := fixture.store.LoadCall(context.Background(), open.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, physical := range prepared.PhysicalCalls {
		if physical.State != domain.PhysicalCompleted {
			t.Fatalf("unsettled call: %+v", physical)
		}
	}
	for _, reservation := range prepared.Reservations {
		if reservation.State != domain.ReservationSettled {
			t.Fatalf("unsettled reservation: %+v", reservation)
		}
	}
	// Until private Blob response replay is connected, restart must stop explicitly
	// instead of invoking the provider to recreate a completed response.
	_, err = callsService.Generate(context.Background(), open, request)
	if err == nil || calls != 2 {
		t.Fatalf("completed replay must fail closed, calls=%d err=%v", calls, err)
	}
}

func durableTestProvider(t *testing.T, endpoint string, extraPrompts ...port.PromptVersion) (*agent.LangChain, port.GenerateRequest) {
	t.Helper()
	schema := port.OutputSchemaRef{SchemaVersion: "cpgen.idea/v1", Digest: domain.SumBytes([]byte("durable-test-schema"))}
	definition := port.PromptVersion{Step: "idea", Version: "v1", Template: "Return JSON", TemplateDigest: domain.SumBytes([]byte("Return JSON")), OutputSchema: schema}
	registry, err := port.NewPromptRegistry(definition)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range extraPrompts {
		if err := registry.Register(extra); err != nil {
			t.Fatal(err)
		}
	}
	validators, err := port.NewSchemaValidatorRegistry(port.SchemaValidatorDefinition{Schema: schema, Validate: func(raw []byte, expected port.OutputSchemaRef, maxBytes int64) error {
		var v struct {
			SchemaVersion domain.SchemaVersion `json:"schema_version"`
			Title         string               `json:"title"`
		}
		return port.DecodeStructuredOutput(raw, expected.SchemaVersion, maxBytes, &v)
	}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := agent.NewLangChain(agent.Config{Endpoint: endpoint, Model: "fixture-model", APIKeyEnv: "CPGEN_DURABLE_TEST_KEY", AllowInsecureHTTP: true, PromptRegistry: registry, SchemaRegistry: validators, MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	return model, port.GenerateRequest{Prompt: port.PromptRef{Step: "idea", Version: "v1", Digest: definition.TemplateDigest}, Schema: schema, Variables: json.RawMessage(`{"brief":"private prompt"}`), Sampling: port.SamplingPolicy{TopP: 1}, MaxOutput: port.OutputLimit{Tokens: 128, Bytes: 4096}, LogicalIdempotencyKey: "will-be-bound", ProviderPolicyDigest: domain.SumBytes([]byte("provider-policy")), PrivacyClassification: "private"}
}
