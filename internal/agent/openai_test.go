package agent

import (
	"context"
	"errors"
	"fmt"
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

	model, err := New(testConfig(server.URL))
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
	config := testConfig(server.URL)
	config.MaxAttempts, config.RetryBaseDelay, config.RetryMaxDelay = 2, time.Nanosecond, time.Millisecond
	model, err := New(config)
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
			config := testConfig(server.URL)
			config.MaxAttempts, config.MaxResponseBytes = 1, maxBytes
			model, err := New(config)
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
	config := testConfig("http://example.com")
	config.AllowInsecureHTTP = false
	if _, err := New(config); !errors.Is(err, &Error{Code: ErrorPolicy}) {
		var typed *Error
		if !errors.As(err, &typed) || typed.Code != ErrorPolicy {
			t.Fatalf("HTTP policy error = %T %v", err, err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	t.Setenv("CPGEN_TEST_LLM_KEY", "secret")
	config = testConfig(server.URL)
	config.Timeout = time.Second
	model, err := New(config)
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
	config := testConfig(server.URL)
	config.MaxAttempts = 1
	model, err := New(config)
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

func TestOpenAICompatibleMissingCredentialIsBlockedWithoutDispatch(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "")
	model, err := New(testConfig("http://127.0.0.1:1"))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil || outcome.Failure == nil {
		t.Fatalf("outcome = %#v, err = %v", outcome, err)
	}
	if outcome.Failure.Code != domain.FailurePolicyRejected || outcome.Failure.Class != domain.FailureBlocked || outcome.CallTrace.DispatchKind != domain.DispatchNone {
		t.Fatalf("blocked outcome = %#v", outcome)
	}
}

func TestOpenAICompatibleTransportUnknownIsNotRetriedAndTraceIsSettled(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "secret")
	transport := &errorRoundTripper{err: errors.New("send boundary is unknown: " + "private-response")}
	config := testConfig("http://127.0.0.1:1")
	config.MaxAttempts = 3
	config.HTTPClient = &http.Client{Transport: transport}
	model, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil || outcome.Failure == nil {
		t.Fatalf("outcome = %#v, err = %v", outcome, err)
	}
	if transport.calls != 1 || outcome.Failure.Code != domain.FailureBoundaryUnknown || outcome.Failure.Class != domain.FailureUnknown || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 1 {
		t.Fatalf("calls=%d failure=%#v trace=%#v", transport.calls, outcome.Failure, outcome.CallTrace)
	}
}

func TestOpenAICompatibleConfirmedNoSendMayRetry(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "secret")
	transport := &errorRoundTripper{err: fmt.Errorf("%w: fixture", ErrConfirmedNoSend)}
	config := testConfig("http://127.0.0.1:1")
	config.MaxAttempts = 2
	config.HTTPClient = &http.Client{Transport: transport}
	model, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil || outcome.Failure == nil || transport.calls != 2 {
		t.Fatalf("calls=%d outcome=%#v err=%v", transport.calls, outcome, err)
	}
	if len(outcome.CallTrace.PhysicalAttemptCallIDs) != 0 || outcome.CallTrace.DispatchKind != domain.DispatchNone {
		t.Fatalf("confirmed no-send trace = %#v", outcome.CallTrace)
	}
}

type errorRoundTripper struct {
	err   error
	calls int
}

func (t *errorRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	return nil, t.err
}

func testGenerateRequest() port.GenerateRequest {
	schema := port.OutputSchemaRef{SchemaVersion: "cpgen.idea/v1", Digest: domain.SumBytes([]byte("schema"))}
	template := "Generate one candidate from the supplied data."
	return port.GenerateRequest{
		Prompt: port.PromptRef{Step: "idea", Version: "v1", Digest: domain.SumBytes([]byte(template)), SchemaVersion: schema.SchemaVersion, SchemaDigest: schema.Digest},
		Schema: schema, Variables: []byte(`{"brief":"fixture"}`), Sampling: port.SamplingPolicy{TopP: 1}, MaxOutput: port.OutputLimit{Tokens: 8, Bytes: 1024},
		LogicalIdempotencyKey: "run_idea_attempt_1", ProviderPolicyDigest: domain.SumBytes([]byte("policy")), PrivacyClassification: "private",
	}
}

type testOutput struct {
	SchemaVersion string `json:"schema_version"`
	Title         string `json:"title,omitempty"`
}

func testConfig(endpoint string) Config {
	schema := port.OutputSchemaRef{SchemaVersion: "cpgen.idea/v1", Digest: domain.SumBytes([]byte("schema"))}
	template := "Generate one candidate from the supplied data."
	registry, err := port.NewPromptRegistry(port.PromptVersion{Step: "idea", Version: "v1", Template: template, TemplateDigest: domain.SumBytes([]byte(template)), OutputSchema: schema})
	if err != nil {
		panic(err)
	}
	schemaRegistry, err := port.NewSchemaValidatorRegistry(port.SchemaValidatorDefinition{Schema: schema, Validate: func(raw []byte, expected port.OutputSchemaRef, maxBytes int64) error {
		var output testOutput
		return port.DecodeStructuredOutput(raw, expected.SchemaVersion, maxBytes, &output)
	}})
	if err != nil {
		panic(err)
	}
	return Config{Endpoint: endpoint, Model: "fixture", APIKeyEnv: "CPGEN_TEST_LLM_KEY", AllowInsecureHTTP: true, PromptRegistry: registry, SchemaRegistry: schemaRegistry}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
