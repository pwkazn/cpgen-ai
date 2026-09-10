package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const langchainSuccess = `{"id":"fixture-1","choices":[{"message":{"content":"{\"schema_version\":\"cpgen.idea/v1\",\"title\":\"ok\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestLangChainPreservesCanonicalRequest(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	t.Setenv("OPENAI_ORGANIZATION", "ambient-organization-must-not-leak")
	t.Setenv("OPENAI_BASE_URL", "https://ambient.invalid")
	for _, path := range []string{"", "/v1", "/v1/chat/completions", "/custom"} {
		t.Run(path, func(t *testing.T) {
			var bodies [][]byte
			var keys []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := strings.TrimSuffix(path, "/chat/completions") + "/chat/completions"
				if path == "/custom" {
					wantPath = "/custom/generate"
				}
				if r.URL.Path != wantPath || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture-key" || r.Header.Get("OpenAI-Organization") != "" {
					t.Error("request endpoint, method or credential contract differs")
				}
				body, _ := io.ReadAll(r.Body)
				bodies = append(bodies, body)
				keys = append(keys, r.Header.Get("Idempotency-Key"))
				_, _ = io.WriteString(w, langchainSuccess)
			}))
			defer server.Close()
			config := testConfig(server.URL + path)
			if path == "/custom" {
				config.CompletionPath = "/generate"
			}
			legacy, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			model, err := NewLangChain(config)
			if err != nil {
				t.Fatal(err)
			}
			request := testGenerateRequest()
			request.Sampling = port.SamplingPolicy{Temperature: 0.35, TopP: 0.72}
			before, err := legacy.Generate(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			after, err := model.Generate(context.Background(), request)
			if err != nil || after.Value == nil {
				t.Fatalf("outcome=%#v err=%v", after, err)
			}
			if len(bodies) != 2 || string(bodies[0]) != string(bodies[1]) || keys[0] == "" || keys[0] != keys[1] {
				t.Fatalf("canonical wire request or identity changed: requests=%d", len(bodies))
			}
			if after.Value.ProviderMeta["adapter"] != "langchaingo-openai-v1" {
				t.Fatal("missing adapter provenance")
			}
			after.Value.ProviderMeta["adapter"] = before.Value.ProviderMeta["adapter"]
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("port contract differs: before=%#v after=%#v", before, after)
			}
		})
	}
}

func TestLangChainMatchesHTTPFailureAndValidationContracts(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	cases := []struct {
		name, body string
		status     int
		cap        int64
	}{
		{"success", langchainSuccess, 200, 0},
		{"missing-usage", `{"choices":[{"message":{"content":"{\"schema_version\":\"cpgen.idea/v1\"}"}}]}`, 200, 0},
		{"partial-usage", strings.Replace(langchainSuccess, `,"completion_tokens":4`, "", 1), 200, 0},
		{"negative-usage", strings.Replace(langchainSuccess, `"prompt_tokens":3`, `"prompt_tokens":-3`, 1), 200, 0},
		{"duplicate-envelope", strings.Replace(langchainSuccess, `"id":"fixture-1"`, `"id":"fixture-1","id":"duplicate"`, 1), 200, 0},
		{"duplicate-output", strings.Replace(langchainSuccess, `\"title\":\"ok\"`, `\"title\":\"ok\",\"title\":\"duplicate\"`, 1), 200, 0},
		{"unknown-output-field", strings.Replace(langchainSuccess, `\"title\":\"ok\"`, `\"unexpected\":true`, 1), 200, 0},
		{"invalid-utf8", "{\"choices\":\"\xff\"}", 200, 0},
		{"trailing-json", langchainSuccess + `{}`, 200, 0},
		{"empty-choices", `{"choices":[]}`, 200, 0},
		{"null-choice", `{"choices":[null]}`, 200, 0},
		{"truncated-json", `{"choices":[`, 200, 0},
		{"length-finish", strings.Replace(langchainSuccess, `"stop"`, `"length"`, 1), 200, 0},
		{"filtered-finish", strings.Replace(langchainSuccess, `"stop"`, `"content_filter"`, 1), 200, 0},
		{"function-finish-without-call", strings.Replace(langchainSuccess, `"stop"`, `"function_call"`, 1), 200, 0},
		{"size-cap", langchainSuccess, 200, 16},
		{"unauthorized", `{"error":{"message":"private-provider-body"}}`, 401, 0},
		{"forbidden", `private-provider-body`, 403, 0},
		{"redirect", `private-provider-body`, 307, 0},
		{"rate-limit", `private-provider-body`, 429, 0},
		{"unavailable", `private-provider-body`, 503, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", "https://redirect.invalid")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			config := testConfig(server.URL)
			config.MaxAttempts, config.MaxResponseBytes = 1, tc.cap
			legacy, err := New(config)
			if err != nil {
				t.Fatal(err)
			}
			model, err := NewLangChain(config)
			if err != nil {
				t.Fatal(err)
			}
			before, errBefore := legacy.Generate(context.Background(), testGenerateRequest())
			after, errAfter := model.Generate(context.Background(), testGenerateRequest())
			if errBefore != nil || errAfter != nil {
				t.Fatalf("errors: %v / %v", errBefore, errAfter)
			}
			if after.Value != nil {
				after.Value.ProviderMeta["adapter"] = before.Value.ProviderMeta["adapter"]
			}
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("port outcomes differ: before=%#v after=%#v", before, after)
			}
			if calls.Load() != 2 {
				t.Fatalf("hidden requests: %d", calls.Load())
			}
			if strings.Contains(fmt.Sprintf("%+v", after), "private-provider-body") {
				t.Fatal("provider body escaped")
			}
		})
	}
}

func TestLangChainDoesNotHideRetriesOrUnknownSends(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	for _, noSend := range []bool{false, true} {
		t.Run(fmt.Sprint(noSend), func(t *testing.T) {
			var calls atomic.Int32
			config := testConfig("http://127.0.0.1")
			config.MaxAttempts = 2
			config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if noSend {
					return nil, ErrConfirmedNoSend
				}
				return nil, errors.New("private transport failure")
			})}
			model, err := NewLangChain(config)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := model.Generate(context.Background(), testGenerateRequest())
			if err != nil || outcome.Failure == nil {
				t.Fatalf("outcome=%#v err=%v", outcome, err)
			}
			want := int32(1)
			if noSend {
				want = 2
			}
			if calls.Load() != want {
				t.Fatalf("requests=%d want=%d", calls.Load(), want)
			}
			if noSend && outcome.CallTrace.DispatchKind != domain.DispatchNone {
				t.Fatal("no-send marked dispatched")
			}
			if !noSend && outcome.Failure.Class != domain.FailureUnknown {
				t.Fatalf("failure=%#v", outcome.Failure)
			}
		})
	}
}

func TestLangChainCancellationAndCredentialStayLocal(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "")
	model, err := NewLangChain(testConfig("http://127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil || outcome.Failure == nil || outcome.Failure.Class != domain.FailureBlocked || outcome.CallTrace.DispatchKind != domain.DispatchNone {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = model.Generate(ctx, testGenerateRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
}

func TestLangChainRejectsTruncationEvenWithValidJSON(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Replace(langchainSuccess, `"stop"`, `"length"`, 1))
	}))
	defer server.Close()
	model, err := NewLangChain(testConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil || outcome.Failure == nil || outcome.Failure.Code != domain.FailureProtocol {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
}

func TestLangChainRejectsSDKMessageRewritingBeforeSend(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	var calls atomic.Int32
	config := testConfig("http://127.0.0.1")
	config.Model = "o1-mini"
	config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { calls.Add(1); return nil, io.EOF })}
	model, err := NewLangChain(config)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil || outcome.Failure == nil || calls.Load() != 0 {
		t.Fatalf("outcome=%#v err=%v calls=%d", outcome, err, calls.Load())
	}
}

func TestLangChainRestoresGPT5SamplingWithoutChangingWire(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	var bodies [][]byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		bodies = append(bodies, body)
		_, _ = io.WriteString(w, langchainSuccess)
	}))
	defer server.Close()
	cfg := testConfig(server.URL)
	cfg.Model = "gpt-5.6-luna"
	legacy, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	model, err := NewLangChain(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := testGenerateRequest()
	request.Sampling = port.SamplingPolicy{Temperature: 0.35, TopP: 0.72}
	before, err := legacy.Generate(context.Background(), request)
	if err != nil || before.Value == nil {
		t.Fatalf("legacy: %+v %v", before, err)
	}
	after, err := model.Generate(context.Background(), request)
	if err != nil || after.Value == nil {
		t.Fatalf("LangChain: %+v %v", after, err)
	}
	if len(bodies) != 2 || string(bodies[0]) != string(bodies[1]) {
		t.Fatalf("canonical admitted request changed; sends=%d", len(bodies))
	}
	if !strings.Contains(string(bodies[1]), `"temperature":0.35`) || !strings.Contains(string(bodies[1]), `"top_p":0.72`) {
		t.Fatal("sampling values changed")
	}
}
func TestLangChainConcurrentCallsKeepRequestAndUsageIsolated(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, langchainSuccess)
	}))
	defer server.Close()
	model, err := NewLangChain(testConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			request := testGenerateRequest()
			request.LogicalIdempotencyKey = fmt.Sprintf("parallel_%d", i)
			outcome, err := model.Generate(context.Background(), request)
			if err != nil || outcome.Value == nil || outcome.Value.Usage != (port.Usage{InputTokens: 3, OutputTokens: 4}) || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 1 {
				t.Errorf("outcome=%#v err=%v", outcome, err)
			}
		}(i)
	}
	group.Wait()
	if calls.Load() != 8 {
		t.Fatalf("requests=%d", calls.Load())
	}
}

func TestLangChainPreservesInflightTimeoutAndCancellationBoundaries(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	for _, timeout := range []bool{false, true} {
		t.Run(fmt.Sprint(timeout), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			config := testConfig("http://127.0.0.1")
			config.Timeout = 10 * time.Millisecond
			config.HTTPClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if !timeout {
					cancel()
				}
				<-r.Context().Done()
				return nil, r.Context().Err()
			})}
			model, err := NewLangChain(config)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := model.Generate(ctx, testGenerateRequest())
			if err != nil || outcome.Failure == nil || calls.Load() != 1 || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 1 {
				t.Fatalf("outcome=%#v err=%v calls=%d", outcome, err, calls.Load())
			}
			if timeout && outcome.Failure.Class != domain.FailureUnknown {
				t.Fatalf("timeout failure=%#v", outcome.Failure)
			}
		})
	}
}

func TestLangChainCapsChunkedResponseBeforeSDKDecoding(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, langchainSuccess)
	}))
	defer server.Close()
	config := testConfig(server.URL)
	config.MaxResponseBytes = 32
	model, err := NewLangChain(config)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil || outcome.Failure == nil || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 1 {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
}

func TestLangChainHTTPRetryUsesStableIdentity(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Header.Get("Idempotency-Key"))
		if len(requests) == 1 {
			w.WriteHeader(429)
			return
		}
		_, _ = io.WriteString(w, langchainSuccess)
	}))
	defer server.Close()
	config := testConfig(server.URL)
	config.MaxAttempts, config.RetryBaseDelay, config.RetryMaxDelay = 2, time.Nanosecond, time.Millisecond
	model, err := NewLangChain(config)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := model.Generate(context.Background(), testGenerateRequest())
	if err != nil || outcome.Value == nil {
		t.Fatalf("outcome=%#v err=%v", outcome, err)
	}
	if len(requests) != 2 || requests[0] == "" || requests[0] != requests[1] || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 2 {
		t.Fatalf("requests=%v trace=%#v", requests, outcome.CallTrace)
	}
	if outcome.Value.ProviderMeta["usage_attempt_count"] != "2" {
		t.Fatal("retry usage missing")
	}
}
