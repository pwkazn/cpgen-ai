package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// These acceptance tests exercise the public physical port without using the
// standalone Generate implementation or adapter accounting helpers as an oracle.
// All credentials and provider responses below are synthetic fixtures.
const physicalAcceptanceKeyEnv = "CPGEN_PHYSICAL_ACCEPTANCE_KEY"

const physicalAcceptanceSuccess = `{"id":"acceptance-provider","choices":[{"message":{"content":"{\"schema_version\":\"cpgen.idea/v1\",\"title\":\"ok\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`

type physicalAcceptanceTransport func(*http.Request) (*http.Response, error)

func (f physicalAcceptanceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func physicalAcceptanceID(n int) domain.AttemptCallID {
	return domain.AttemptCallID(fmt.Sprintf("call_%032x", n))
}

func physicalAcceptanceModel(t *testing.T, cfg Config) port.PhysicalLLM {
	t.Helper()
	model, err := NewLangChain(cfg)
	if err != nil {
		t.Fatalf("construct physical adapter: %v", err)
	}
	return model
}

func physicalAcceptancePlan(t *testing.T, model port.PhysicalLLM, request port.GenerateRequest) port.LLMRequestPlan {
	t.Helper()
	plan, err := model.PlanGenerate(request)
	if err != nil {
		t.Fatalf("plan physical request: %v", err)
	}
	if plan.RequestDigest.Validate() != nil || plan.Provider == "" || plan.InputTokenUpperBound <= 0 || plan.OutputTokenUpperBound != request.MaxOutput.Tokens {
		t.Fatal("plan must have a valid identity, provider and positive reservation bounds")
	}
	return plan
}

func physicalAcceptanceExecute(t *testing.T, model port.PhysicalLLM, ctx context.Context, request port.GenerateRequest, id domain.AttemptCallID) port.PhysicalLLMResult {
	t.Helper()
	result, err := model.GeneratePhysical(ctx, request, id)
	if err != nil {
		t.Fatalf("physical execution returned an error instead of accounting: %v", err)
	}
	if err := result.Execution.Validate(); err != nil {
		t.Errorf("invalid physical execution: %v", err)
	}
	return result
}

func physicalAcceptanceUsage(t *testing.T, result port.PhysicalLLMResult, plan port.LLMRequestPlan, verified bool) {
	t.Helper()
	want := port.Usage{InputTokens: plan.InputTokenUpperBound, OutputTokens: plan.OutputTokenUpperBound}
	if verified {
		want = port.Usage{InputTokens: 3, OutputTokens: 4}
	}
	if result.UsageVerified != verified || result.Usage != want {
		t.Errorf("usage verified=%t tokens=%+v; want verified=%t tokens=%+v", result.UsageVerified, result.Usage, verified, want)
	}
	if result.Execution.Value != nil && result.Execution.Value.Usage != result.Usage {
		t.Error("response usage differs from physical accounting")
	}
}

func physicalAcceptanceFailure(t *testing.T, result port.PhysicalLLMResult, boundary domain.PhysicalBoundary, code domain.PortFailureCode, class domain.FailureClass) {
	t.Helper()
	execution := result.Execution
	if execution.Boundary != boundary {
		t.Errorf("boundary=%s; want %s", execution.Boundary, boundary)
	}
	if execution.Value != nil {
		t.Error("failed physical execution exposed a generated value")
	}
	if execution.Failure == nil {
		t.Error("failed physical execution lost its failure")
	} else if execution.Failure.Code != code || execution.Failure.Class != class {
		t.Errorf("failure=%s/%s; want %s/%s", execution.Failure.Code, execution.Failure.Class, code, class)
	}
	if boundary == domain.BoundaryConfirmedNoSend || boundary == domain.BoundaryUnknown {
		if execution.ProviderRequestID != "" || execution.ResponseDigest != nil {
			t.Error("incomplete execution claimed a provider receipt or response digest")
		}
	}
	if boundary == domain.BoundaryConfirmedNoSend && (result.UsageVerified || result.Usage != (port.Usage{}) || len(execution.Usage) != 0) {
		t.Error("confirmed no-send must not claim billable usage")
	}
}

func TestPhysicalAcceptancePlanIsLocalAndCredentialIndependent(t *testing.T) {
	var dnsCalls, dialCalls atomic.Int32
	previousResolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		dnsCalls.Add(1)
		return nil, errors.New("acceptance DNS must remain unused")
	}}
	t.Cleanup(func() { net.DefaultResolver = previousResolver })
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		dialCalls.Add(1)
		return nil, errors.New("acceptance network must remain unused")
	}}
	t.Cleanup(transport.CloseIdleConnections)
	cfg := testConfig("https://physical-acceptance.invalid/v1")
	cfg.APIKeyEnv = physicalAcceptanceKeyEnv
	cfg.HTTPClient = &http.Client{Transport: transport}
	t.Setenv(physicalAcceptanceKeyEnv, "")
	if err := os.Unsetenv(physicalAcceptanceKeyEnv); err != nil {
		t.Fatal("could not unset synthetic credential")
	}
	model := physicalAcceptanceModel(t, cfg)
	request := testGenerateRequest()
	request.Variables = []byte(`{"brief":"private-acceptance-input"}`)
	before, _ := json.Marshal(request)
	baseline := physicalAcceptancePlan(t, model, request)
	if baseline.Provider != "physical-acceptance.invalid" {
		t.Error("plan provider must identify the configured hostname")
	}
	for _, credential := range []string{"", "invalid\nfixture", "acceptance-key-a", "acceptance-key-b"} {
		t.Setenv(physicalAcceptanceKeyEnv, credential)
		got := physicalAcceptancePlan(t, model, request)
		if !reflect.DeepEqual(baseline, got) {
			t.Error("credential state changed the request plan")
		}
		encoded, err := json.Marshal(got)
		if err != nil {
			t.Fatal("plan is not serializable")
		}
		for _, private := range []string{"private-acceptance-input", "acceptance-key-a", "acceptance-key-b", physicalAcceptanceKeyEnv} {
			if strings.Contains(string(encoded), private) {
				t.Error("plan contains private input or credential material")
			}
		}
	}
	request.Variables = []byte(" { \"brief\" : \"private-acceptance-input\" } ")
	if got := physicalAcceptancePlan(t, model, request); !reflect.DeepEqual(baseline, got) {
		t.Error("equivalent JSON changed the canonical plan")
	}
	request.Variables = []byte(`{"brief":"private-acceptance-input"}`)
	after, _ := json.Marshal(request)
	if string(before) != string(after) {
		t.Error("planning mutated the request")
	}
	if dnsCalls.Load() != 0 || dialCalls.Load() != 0 {
		t.Errorf("planning attempted external I/O: DNS=%d dials=%d", dnsCalls.Load(), dialCalls.Load())
	}
}

func TestPhysicalAcceptanceMissingCredentialAndPreCancellationDoNotSend(t *testing.T) {
	for _, name := range []string{"unset", "empty", "invalid-header", "pre-canceled", "expired-deadline"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			cfg := testConfig("http://127.0.0.1")
			cfg.APIKeyEnv, cfg.MaxAttempts = physicalAcceptanceKeyEnv, 4
			cfg.HTTPClient = &http.Client{Transport: physicalAcceptanceTransport(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, io.EOF
			})}
			t.Setenv(physicalAcceptanceKeyEnv, "acceptance-key")
			ctx := context.Background()
			code, class := domain.FailurePolicyRejected, domain.FailureBlocked
			switch name {
			case "unset":
				if err := os.Unsetenv(physicalAcceptanceKeyEnv); err != nil {
					t.Fatal("could not unset synthetic credential")
				}
			case "empty":
				t.Setenv(physicalAcceptanceKeyEnv, "")
			case "invalid-header":
				t.Setenv(physicalAcceptanceKeyEnv, "invalid\r\nfixture")
			case "pre-canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				code, class = domain.FailureTransport, domain.FailureRejected
			case "expired-deadline":
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Now().Add(-time.Second))
				defer cancel()
				code, class = domain.FailureTransport, domain.FailureRejected
			}
			model := physicalAcceptanceModel(t, cfg)
			request := testGenerateRequest()
			physicalAcceptancePlan(t, model, request)
			result := physicalAcceptanceExecute(t, model, ctx, request, physicalAcceptanceID(1))
			physicalAcceptanceFailure(t, result, domain.BoundaryConfirmedNoSend, code, class)
			if calls.Load() != 0 {
				t.Errorf("local rejection invoked HTTP %d times", calls.Load())
			}
		})
	}
}

func TestPhysicalAcceptanceHTTPFailureIsOneSendAndRetainsUsage(t *testing.T) {
	t.Setenv(physicalAcceptanceKeyEnv, "acceptance-key")
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	for _, status := range []int{429, 500, 502, 503, 504} {
		for _, body := range []string{`{"error":"private-acceptance-provider-error"}`, physicalAcceptanceSuccess} {
			name := fmt.Sprintf("status-%d/claimed-usage-%t", status, body == physicalAcceptanceSuccess)
			t.Run(name, func(t *testing.T) {
				var calls, requestBytes atomic.Int64
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					requestBody, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error("could not read synthetic request")
					}
					requestBytes.Store(int64(len(requestBody)))
					w.Header().Set("Retry-After", "7")
					w.WriteHeader(status)
					_, _ = io.WriteString(w, body)
				}))
				defer server.Close()
				cfg := testConfig(server.URL)
				cfg.APIKeyEnv, cfg.MaxAttempts = physicalAcceptanceKeyEnv, 4
				cfg.Now = func() time.Time { return now }
				model := physicalAcceptanceModel(t, cfg)
				request := testGenerateRequest()
				plan := physicalAcceptancePlan(t, model, request)
				result := physicalAcceptanceExecute(t, model, context.Background(), request, physicalAcceptanceID(status))
				code := domain.FailureUnavailable
				if status == 429 {
					code = domain.FailureRateLimited
				}
				physicalAcceptanceFailure(t, result, domain.BoundaryCompleted, code, domain.FailureRetryable)
				physicalAcceptanceUsage(t, result, plan, false)
				if calls.Load() != 1 {
					t.Errorf("HTTP calls=%d; want exactly one despite MaxAttempts=4", calls.Load())
				}
				if plan.InputTokenUpperBound != requestBytes.Load() {
					t.Error("planned byte-based input bound differs from actual request bytes")
				}
				if failure := result.Execution.Failure; failure == nil || failure.RetryAfter == nil || !failure.RetryAfter.Equal(now.Add(7*time.Second)) {
					t.Error("Retry-After evidence was not preserved for the application")
				}
				if result.Execution.ResponseDigest == nil || *result.Execution.ResponseDigest != domain.SumBytes([]byte(body)) {
					t.Error("completed HTTP failure lost the raw response digest")
				}
				encoded, _ := json.Marshal(result)
				if strings.Contains(string(encoded), "private-acceptance-provider-error") || strings.Contains(string(encoded), "acceptance-key") {
					t.Error("failure accounting exposed provider body or credential")
				}
			})
		}
	}
}

func TestPhysicalAcceptanceResponseValidationAndUsage(t *testing.T) {
	t.Setenv(physicalAcceptanceKeyEnv, "acceptance-key")
	usageField := `,"usage":{"prompt_tokens":3,"completion_tokens":4}`
	withoutUsage := strings.Replace(physicalAcceptanceSuccess, usageField, "", 1)
	cases := []struct {
		name     string
		body     string
		accepted bool
		verified bool
	}{
		{"success", physicalAcceptanceSuccess, true, true},
		{"missing-usage", withoutUsage, true, false},
		{"null-usage", strings.Replace(physicalAcceptanceSuccess, usageField, `,"usage":null`, 1), true, false},
		{"partial-usage", strings.Replace(physicalAcceptanceSuccess, `,"completion_tokens":4`, "", 1), true, false},
		{"negative-usage", strings.Replace(physicalAcceptanceSuccess, `"prompt_tokens":3`, `"prompt_tokens":-1`, 1), true, false},
		{"null-counter", strings.Replace(physicalAcceptanceSuccess, `"prompt_tokens":3`, `"prompt_tokens":null`, 1), true, false},
		{"wrong-type-usage", strings.Replace(physicalAcceptanceSuccess, `"prompt_tokens":3`, `"prompt_tokens":"3"`, 1), false, false},
		{"overflow-usage", strings.Replace(physicalAcceptanceSuccess, `"prompt_tokens":3`, `"prompt_tokens":9223372036854775808`, 1), false, false},
		{"fractional-usage", strings.Replace(physicalAcceptanceSuccess, `"prompt_tokens":3`, `"prompt_tokens":3.5`, 1), false, false},
		{"malformed-envelope", `not-json`, false, false},
		{"truncated-envelope", physicalAcceptanceSuccess[:len(physicalAcceptanceSuccess)-1], false, false},
		{"trailing-json", physicalAcceptanceSuccess + `{}`, false, false},
		{"duplicate-envelope", strings.Replace(physicalAcceptanceSuccess, `"id":"acceptance-provider"`, `"id":"first","id":"second"`, 1), false, false},
		{"duplicate-usage-object", strings.Replace(physicalAcceptanceSuccess, usageField, usageField+usageField, 1), false, false},
		{"duplicate-usage-counter", strings.Replace(physicalAcceptanceSuccess, `"prompt_tokens":3`, `"prompt_tokens":99,"prompt_tokens":3`, 1), false, false},
		{"invalid-utf8-envelope", strings.Replace(physicalAcceptanceSuccess, "acceptance-provider", "invalid-\xff-id", 1), false, false},
		{"null-choice", `{"choices":[null]}`, false, false},
		{"empty-choices", `{"choices":[]}`, false, false},
		{"malformed-output", strings.Replace(withoutUsage, `\"title\":\"ok\"`, `\"title\":`, 1), false, false},
		{"duplicate-output", strings.Replace(withoutUsage, `\"title\":\"ok\"`, `\"title\":\"ok\",\"title\":\"duplicate\"`, 1), false, false},
		{"truncated-output", strings.Replace(withoutUsage, `"stop"`, `"length"`, 1), false, false},
		{"filtered-output", strings.Replace(withoutUsage, `"stop"`, `"content_filter"`, 1), false, false},
		// Output rejection does not erase complete, well-formed provider usage.
		{"schema-failure-with-usage", strings.Replace(physicalAcceptanceSuccess, `\"title\":\"ok\"`, `\"unexpected\":true`, 1), false, true},
		{"truncated-output-with-usage", strings.Replace(physicalAcceptanceSuccess, `"stop"`, `"length"`, 1), false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			cfg := testConfig(server.URL)
			cfg.APIKeyEnv, cfg.MaxAttempts = physicalAcceptanceKeyEnv, 4
			model := physicalAcceptanceModel(t, cfg)
			request := testGenerateRequest()
			plan := physicalAcceptancePlan(t, model, request)
			id := physicalAcceptanceID(2)
			result := physicalAcceptanceExecute(t, model, context.Background(), request, id)
			if tc.accepted {
				if result.Execution.Boundary != domain.BoundaryCompleted || result.Execution.Value == nil || result.Execution.Failure != nil {
					t.Error("valid structured output was not accepted as completed")
				}
				if value := result.Execution.Value; value != nil {
					if err := value.Validate(); err != nil {
						t.Errorf("invalid generated response: %v", err)
					}
					trace := value.CallTrace
					if len(trace.PhysicalAttemptCallIDs) != 1 || trace.PhysicalAttemptCallIDs[0] != id || trace.ResultAttemptCallID == nil || *trace.ResultAttemptCallID != id {
						t.Error("physical response did not retain exactly the authorized attempt ID")
					}
					if value.ProviderMeta["request_digest"] != string(plan.RequestDigest) {
						t.Error("physical response request digest differs from the plan")
					}
				}
			} else {
				physicalAcceptanceFailure(t, result, domain.BoundaryCompleted, domain.FailureProtocol, domain.FailureRejected)
			}
			physicalAcceptanceUsage(t, result, plan, tc.verified)
			if result.Execution.ResponseDigest == nil || *result.Execution.ResponseDigest != domain.SumBytes([]byte(tc.body)) {
				t.Error("completed response lost its raw-byte digest")
			}
			if calls.Load() != 1 {
				t.Errorf("validation path made %d HTTP calls; want one", calls.Load())
			}
		})
	}
}

type physicalAcceptanceBrokenBody struct{ io.Reader }

func (b physicalAcceptanceBrokenBody) Read(p []byte) (int, error) {
	n, err := b.Reader.Read(p)
	if err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}

func (physicalAcceptanceBrokenBody) Close() error { return nil }

func TestPhysicalAcceptanceTransportBoundariesNeverRetry(t *testing.T) {
	t.Setenv(physicalAcceptanceKeyEnv, "acceptance-key")
	for _, name := range []string{"inflight-cancel", "adapter-timeout", "caller-timeout", "EOF", "unexpected-EOF", "body-EOF-after-usage", "confirmed-no-send"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			cfg := testConfig("http://127.0.0.1")
			cfg.APIKeyEnv, cfg.MaxAttempts = physicalAcceptanceKeyEnv, 4
			cfg.Timeout = 2 * time.Second
			if name == "adapter-timeout" {
				cfg.Timeout = 25 * time.Millisecond
			}
			if name == "caller-timeout" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 250*time.Millisecond)
				defer deadlineCancel()
			}
			cfg.HTTPClient = &http.Client{Transport: physicalAcceptanceTransport(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				// Consume the request to make cancellation/EOF occur after the
				// transport has been entered, without relying on scheduling sleeps.
				_, _ = io.Copy(io.Discard, r.Body)
				if r.GetBody != nil {
					t.Error("request permits net/http to replay an idempotent POST")
				}
				switch name {
				case "inflight-cancel":
					cancel()
					<-r.Context().Done()
					return nil, r.Context().Err()
				case "adapter-timeout", "caller-timeout":
					<-r.Context().Done()
					return nil, r.Context().Err()
				case "unexpected-EOF":
					return nil, io.ErrUnexpectedEOF
				case "body-EOF-after-usage":
					return &http.Response{StatusCode: 200, Header: make(http.Header), ContentLength: -1, Body: physicalAcceptanceBrokenBody{strings.NewReader(physicalAcceptanceSuccess)}}, nil
				case "confirmed-no-send":
					return nil, fmt.Errorf("synthetic transport: %w", ErrConfirmedNoSend)
				default:
					return nil, io.EOF
				}
			})}
			model := physicalAcceptanceModel(t, cfg)
			request := testGenerateRequest()
			plan := physicalAcceptancePlan(t, model, request)
			result := physicalAcceptanceExecute(t, model, ctx, request, physicalAcceptanceID(3))
			if name == "confirmed-no-send" {
				physicalAcceptanceFailure(t, result, domain.BoundaryConfirmedNoSend, domain.FailureTransport, domain.FailureRetryable)
			} else {
				physicalAcceptanceFailure(t, result, domain.BoundaryUnknown, domain.FailureBoundaryUnknown, domain.FailureUnknown)
				physicalAcceptanceUsage(t, result, plan, false)
			}
			if calls.Load() != 1 {
				t.Errorf("transport invocations=%d; want one despite MaxAttempts=4", calls.Load())
			}
		})
	}
}

func TestPhysicalAcceptanceLogicalIdempotencySurvivesNewPhysicalIDs(t *testing.T) {
	t.Setenv(physicalAcceptanceKeyEnv, "acceptance-key-before-plan")
	type observedRequest struct{ key, body string }
	var mu sync.Mutex
	var observed []observedRequest
	var staleCredential atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer acceptance-key-at-send" {
			staleCredential.Store(true)
		}
		mu.Lock()
		observed = append(observed, observedRequest{r.Header.Get("Idempotency-Key"), string(body)})
		n := len(observed)
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":"rate limited"}`)
			return
		}
		_, _ = io.WriteString(w, physicalAcceptanceSuccess)
	}))
	defer server.Close()
	cfg := testConfig(server.URL)
	cfg.APIKeyEnv, cfg.MaxAttempts = physicalAcceptanceKeyEnv, 4
	model := physicalAcceptanceModel(t, cfg)
	request := testGenerateRequest()
	plan := physicalAcceptancePlan(t, model, request)
	t.Setenv(physicalAcceptanceKeyEnv, "acceptance-key-at-send")
	first := physicalAcceptanceExecute(t, model, context.Background(), request, physicalAcceptanceID(10))
	physicalAcceptanceFailure(t, first, domain.BoundaryCompleted, domain.FailureRateLimited, domain.FailureRetryable)
	physicalAcceptanceUsage(t, first, plan, false)
	logicalID := ""
	for i := 11; i <= 13; i++ {
		if i == 13 {
			request.LogicalIdempotencyKey += "_different_operation"
		}
		id := physicalAcceptanceID(i)
		result := physicalAcceptanceExecute(t, model, context.Background(), request, id)
		physicalAcceptanceUsage(t, result, plan, true)
		if result.Execution.Value == nil || result.Execution.Failure != nil {
			t.Fatal("manual physical invocation did not succeed")
		}
		trace := result.Execution.Value.CallTrace
		if len(trace.PhysicalAttemptCallIDs) != 1 || trace.PhysicalAttemptCallIDs[0] != id || trace.ResultAttemptCallID == nil || *trace.ResultAttemptCallID != id {
			t.Error("physical call merged, replaced or invented authorized attempt IDs")
		}
		if i == 11 {
			logicalID = trace.LogicalOperationID
		} else if (trace.LogicalOperationID == logicalID) != (i == 12) {
			t.Error("logical trace identity does not match caller logical operation")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(observed) != 4 {
		t.Fatalf("four manual invocations made %d HTTP calls", len(observed))
	}
	if observed[0].key == "" || observed[0].key != observed[1].key || observed[1].key != observed[2].key || observed[2].key == observed[3].key {
		t.Error("provider idempotency key must remain stable across physical IDs and separate logical operations")
	}
	for _, got := range observed[1:] {
		if got.body != observed[0].body {
			t.Error("attempt/logical identity leaked into or changed the canonical wire body")
		}
	}
	if staleCredential.Load() {
		t.Error("physical dispatch did not resolve the current credential at send time")
	}
}

func TestPhysicalAcceptanceInvalidConfigurationRejectsBeforeSend(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"missing-credential-reference", func(c *Config) { c.APIKeyEnv = "" }},
		{"missing-model", func(c *Config) { c.Model = "" }},
		{"endpoint-userinfo", func(c *Config) { c.Endpoint = "http://fixture:fixture@127.0.0.1" }},
		{"host-not-allowed", func(c *Config) { c.AllowedHosts = []string{"different.invalid"} }},
		{"insecure-public-host", func(c *Config) { c.Endpoint = "http://public.invalid" }},
		{"missing-prompt-registry", func(c *Config) { c.PromptRegistry = nil }},
		{"missing-schema-registry", func(c *Config) { c.SchemaRegistry = nil }},
		{"too-many-attempts", func(c *Config) { c.MaxAttempts = 9 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			cfg := testConfig("http://127.0.0.1")
			cfg.HTTPClient = &http.Client{Transport: physicalAcceptanceTransport(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return nil, io.EOF
			})}
			tc.mutate(&cfg)
			if _, err := NewLangChain(cfg); err == nil {
				t.Error("invalid adapter configuration was accepted")
			}
			if calls.Load() != 0 {
				t.Error("invalid configuration attempted HTTP")
			}
		})
	}
}

func TestPhysicalAcceptanceInvalidRequestAndSchemaRejectBeforeSend(t *testing.T) {
	t.Setenv(physicalAcceptanceKeyEnv, "acceptance-key")
	for _, tc := range []struct {
		name   string
		mutate func(*Config, *port.GenerateRequest)
	}{
		{"schema-digest-mismatch", func(_ *Config, r *port.GenerateRequest) {
			r.Schema.Digest = domain.SumBytes([]byte("different schema"))
		}},
		{"schema-version-mismatch", func(_ *Config, r *port.GenerateRequest) { r.Schema.SchemaVersion = "cpgen.idea/v2" }},
		{"stale-prompt-digest", func(_ *Config, r *port.GenerateRequest) { r.Prompt.Digest = domain.SumBytes([]byte("stale prompt")) }},
		{"malformed-variables", func(_ *Config, r *port.GenerateRequest) { r.Variables = []byte(`{"brief":`) }},
		{"duplicate-variables", func(_ *Config, r *port.GenerateRequest) { r.Variables = []byte(`{"brief":"first","brief":"second"}`) }},
		{"missing-logical-key", func(_ *Config, r *port.GenerateRequest) { r.LogicalIdempotencyKey = "" }},
		{"missing-policy", func(_ *Config, r *port.GenerateRequest) { r.ProviderPolicyDigest = "" }},
		{"missing-privacy", func(_ *Config, r *port.GenerateRequest) { r.PrivacyClassification = "" }},
		{"zero-output-limit", func(_ *Config, r *port.GenerateRequest) { r.MaxOutput.Tokens = 0 }},
		{"invalid-sampling", func(_ *Config, r *port.GenerateRequest) { r.Sampling.TopP = 2 }},
		{"unbound-schema-registry", func(c *Config, _ *port.GenerateRequest) { c.SchemaRegistry, _ = port.NewSchemaValidatorRegistry() }},
		{"same-digest-wrong-schema-version", func(c *Config, r *port.GenerateRequest) {
			wrongSchema := r.Schema
			wrongSchema.SchemaVersion = "cpgen.idea/v2"
			c.SchemaRegistry, _ = port.NewSchemaValidatorRegistry(port.SchemaValidatorDefinition{
				Schema: wrongSchema, Validate: func([]byte, port.OutputSchemaRef, int64) error { return nil },
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			cfg := testConfig("http://127.0.0.1")
			cfg.APIKeyEnv, cfg.MaxAttempts = physicalAcceptanceKeyEnv, 4
			cfg.HTTPClient = &http.Client{Transport: physicalAcceptanceTransport(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(physicalAcceptanceSuccess))}, nil
			})}
			request := testGenerateRequest()
			tc.mutate(&cfg, &request)
			model, err := NewLangChain(cfg)
			if err == nil {
				if _, err := model.PlanGenerate(request); err == nil {
					t.Error("PlanGenerate accepted invalid request/schema binding")
				}
				result, err := model.GeneratePhysical(context.Background(), request, physicalAcceptanceID(4))
				if err == nil {
					if result.Execution.Boundary != domain.BoundaryConfirmedNoSend || result.Execution.Failure == nil {
						t.Errorf("invalid request/schema was not rejected locally: boundary=%s", result.Execution.Boundary)
					}
				}
				if result.UsageVerified || result.Usage != (port.Usage{}) || result.Execution.Value != nil {
					t.Error("invalid request/schema produced usage or a generated value")
				}
			}
			if calls.Load() != 0 {
				t.Errorf("invalid request/schema reached HTTP %d times; want zero", calls.Load())
			}
		})
	}
}

func TestPhysicalAcceptanceInvalidInvocationDoesNotSend(t *testing.T) {
	t.Setenv(physicalAcceptanceKeyEnv, "acceptance-key")
	var calls atomic.Int32
	cfg := testConfig("http://127.0.0.1")
	cfg.APIKeyEnv = physicalAcceptanceKeyEnv
	cfg.HTTPClient = &http.Client{Transport: physicalAcceptanceTransport(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, io.EOF
	})}
	model := physicalAcceptanceModel(t, cfg)
	for _, tc := range []struct {
		name string
		ctx  context.Context
		id   domain.AttemptCallID
	}{
		{"nil-context", nil, physicalAcceptanceID(5)},
		{"empty-physical-id", context.Background(), ""},
		{"invalid-physical-id", context.Background(), "not-a-call-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result, err := model.GeneratePhysical(tc.ctx, testGenerateRequest(), tc.id)
			if err == nil || result.Execution.Value != nil || result.Usage != (port.Usage{}) {
				t.Error("invalid physical invocation was not rejected without usage")
			}
		})
	}
	if calls.Load() != 0 {
		t.Errorf("invalid invocation reached HTTP %d times", calls.Load())
	}
}
