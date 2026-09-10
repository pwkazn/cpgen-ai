package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"cpgen/internal/domain"
)

func TestPhysicalGenerationKeepsFailedUsageAndNeverRetries(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":"private-provider-error"}`)
	}))
	defer server.Close()
	cfg := testConfig(server.URL)
	cfg.MaxAttempts = 3
	model, err := NewLangChain(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := testGenerateRequest()
	plan, err := model.PlanGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	id := domain.AttemptCallID("call_00000000000000000000000000000001")
	result, err := model.GeneratePhysical(context.Background(), request, id)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || result.Execution.Boundary != domain.BoundaryCompleted || result.Execution.Failure.Code != domain.FailureRateLimited {
		t.Fatalf("calls=%d result=%+v", calls, result)
	}
	if result.UsageVerified || result.Usage.InputTokens != plan.InputTokenUpperBound || result.Usage.OutputTokens != request.MaxOutput.Tokens {
		t.Fatalf("failed usage lost: %+v", result)
	}
	if err := result.Execution.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalGenerationUsesAuthorizedIdentity(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, langchainSuccess) }))
	defer server.Close()
	model, err := NewLangChain(testConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	id := domain.AttemptCallID("call_00000000000000000000000000000002")
	result, err := model.GeneratePhysical(context.Background(), testGenerateRequest(), id)
	if err != nil || result.Execution.Value == nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if !result.UsageVerified || result.Usage.InputTokens != 3 || result.Usage.OutputTokens != 4 || *result.Execution.Value.CallTrace.ResultAttemptCallID != id {
		t.Fatalf("usage or physical identity mismatch: %+v", result)
	}
}
