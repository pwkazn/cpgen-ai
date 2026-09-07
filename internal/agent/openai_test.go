package agent

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestOpenAICompatibleSuccessUsesStrictPortAndConservativeMetadata(t *testing.T) {
	const secret = "test-secret-never-log"
	t.Setenv("CPGEN_TEST_LLM_KEY", secret)
	var gotAuthorization atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthorization.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"provider-1","model":"fixture","choices":[{"message":{"content":"{\"schema_version\":\"cpgen.idea/v1\",\"title\":\"ok\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`))
	}))
	defer server.Close()

	model, err := New(Config{Endpoint: server.URL, Model: "fixture", APIKeyEnv: "CPGEN_TEST_LLM_KEY", AllowInsecureHTTP: true, RetryBaseDelay: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatal(err)
	}
	if outcome.Value == nil || string(outcome.Value.Structured) != `{"schema_version":"cpgen.idea/v1","title":"ok"}` {
		t.Fatalf("outcome = %#v", outcome)
	}
	if outcome.Value.Usage != (port.Usage{InputTokens: 3, OutputTokens: 4}) {
		t.Fatalf("usage = %#v", outcome.Value.Usage)
	}
	if outcome.Value.ProviderMeta["usage_source"] != "provider_verified" || outcome.Value.ProviderMeta["idempotency"] != "stable" {
		t.Fatalf("metadata = %#v", outcome.Value.ProviderMeta)
	}
	if got := gotAuthorization.Load().(string); got != "Bearer "+secret {
		t.Fatalf("authorization = %q", got)
	}
	if strings.Contains(outcome.Value.ProviderMeta["adapter"], secret) {
		t.Fatal("credential leaked into provider metadata")
	}
}

func TestOpenAICompatibleRetriesTransientStatusWithStableIdentity(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "secret")
	var attempts atomic.Int32
	var firstKey, secondKey atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			firstKey.Store(r.Header.Get("Idempotency-Key"))
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		secondKey.Store(r.Header.Get("Idempotency-Key"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"schema_version\":\"cpgen.idea/v1\"}"}}]}`))
	}))
	defer server.Close()
	model, err := New(Config{Endpoint: server.URL, Model: "fixture", APIKeyEnv: "CPGEN_TEST_LLM_KEY", AllowInsecureHTTP: true, MaxAttempts: 2, RetryBaseDelay: time.Nanosecond, RetryMaxDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil || outcome.Value == nil {
		t.Fatalf("outcome = %#v, err = %v", outcome, err)
	}
	if attempts.Load() != 2 || firstKey.Load().(string) != secondKey.Load().(string) {
		t.Fatalf("attempts=%d keys=%q/%q", attempts.Load(), firstKey.Load(), secondKey.Load())
	}
	if len(outcome.CallTrace.PhysicalAttemptCallIDs) != 2 || outcome.CallTrace.ResultAttemptCallID == nil {
		t.Fatalf("trace = %#v", outcome.CallTrace)
	}
}

func TestOpenAICompatibleReturnsTypedFailuresWithoutProviderContent(t *testing.T) {
	const secret = "response-secret-must-not-escape"
	t.Setenv("CPGEN_TEST_LLM_KEY", "key")
	cases := []struct {
		name string
		run  func(http.ResponseWriter)
	}{
		{name: "http", run: func(w http.ResponseWriter) { w.WriteHeader(http.StatusBadRequest); _, _ = w.Write([]byte(secret)) }},
		{name: "invalid-json", run: func(w http.ResponseWriter) { _, _ = w.Write([]byte(`{"choices":[` + secret + `]}`)) }},
		{name: "too-large", run: func(w http.ResponseWriter) { _, _ = w.Write([]byte(strings.Repeat("x", 80))) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { tc.run(w) }))
			defer server.Close()
			maxBytes := int64(1 << 20)
			if tc.name == "too-large" {
				maxBytes = 16
			}
			model, err := New(Config{Endpoint: server.URL, Model: "fixture", APIKeyEnv: "CPGEN_TEST_LLM_KEY", AllowInsecureHTTP: true, MaxAttempts: 1, MaxResponseBytes: maxBytes})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := model.Generate(context.Background(), testGenerateRequest())
			if err != nil || outcome.Failure == nil {
				t.Fatalf("outcome = %#v, err = %v", outcome, err)
			}
			if strings.Contains(errString(err), secret) {
				t.Fatal("provider content leaked into error")
			}
			if outcome.CallTrace.DispatchKind != domain.DispatchDispatched {
				t.Fatalf("trace = %#v", outcome.CallTrace)
			}
		})
	}
}

func TestOpenAICompatibleRejectsEndpointPolicyAndPreservesCancellation(t *testing.T) {
	if _, err := New(Config{Endpoint: "http://example.com", Model: "fixture", APIKeyEnv: "CPGEN_TEST_LLM_KEY"}); !errors.Is(err, &Error{Code: ErrorPolicy}) {
		var typed *Error
		if !errors.As(err, &typed) || typed.Code != ErrorPolicy {
			t.Fatalf("HTTP policy error = %T %v", err, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	t.Setenv("CPGEN_TEST_LLM_KEY", "secret")
	model, err := New(Config{Endpoint: server.URL, Model: "fixture", APIKeyEnv: "CPGEN_TEST_LLM_KEY", AllowInsecureHTTP: true, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = model.Generate(ctx, testGenerateRequest())
	var typed *Error
	if !errors.As(err, &typed) || typed.Code != ErrorCanceled || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation error = %T %v", err, err)
	}
}

func TestOpenAICompatibleUsesConservativeUsageWhenProviderOmitsUsage(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"{\"schema_version\":\"cpgen.idea/v1\"}"}}]}`))
	}))
	defer server.Close()
	model, err := New(Config{Endpoint: server.URL, Model: "fixture", APIKeyEnv: "CPGEN_TEST_LLM_KEY", AllowInsecureHTTP: true, MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil || outcome.Value == nil {
		t.Fatalf("outcome = %#v, err = %v", outcome, err)
	}
	if outcome.Value.ProviderMeta["usage_source"] != "conservative_upper_bound_v1" || outcome.Value.Usage.OutputTokens != 8 || outcome.Value.Usage.InputTokens <= 0 {
		t.Fatalf("usage = %#v metadata = %#v", outcome.Value.Usage, outcome.Value.ProviderMeta)
	}
}

func testGenerateRequest() port.GenerateRequest {
	return port.GenerateRequest{
		Prompt:    port.PromptRef{Step: "idea", Version: "v1", Digest: domain.SumBytes([]byte("prompt"))},
		Schema:    port.OutputSchemaRef{SchemaVersion: "cpgen.idea/v1", Digest: domain.SumBytes([]byte("schema"))},
		Variables: []byte(`{"brief":"fixture"}`),
		Sampling:  port.SamplingPolicy{TopP: 1},
		MaxOutput: port.OutputLimit{Tokens: 8, Bytes: 1024},
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
