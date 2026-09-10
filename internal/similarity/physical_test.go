package similarity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestPhysicalSimilarityBindsPolicyAndUsesDurableIdentity(t *testing.T) {
	t.Setenv("CPGEN_SIM_TEST_KEY", "physical-fixture-key")
	const responseBody = `{"provider_identity":"fixture","hits":[{"source":"catalog","external_id":"42","title":"private third-party title","score":0.2}],"usage":{"input_tokens":7,"output_tokens":9,"cost_micro_usd":11}}`
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sends.Add(1)
		_, _ = w.Write([]byte(responseBody))
	}))
	t.Cleanup(server.Close)
	cfg := testHTTPConfig(server.URL)
	cfg.MaxAttempts = 4
	adapter, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := physicalSimilarityRequest(t)
	plan, err := adapter.PlanSearch(request)
	if err != nil || plan.RequestDigest.Validate() != nil || plan.PolicyDigest.Validate() != nil || sends.Load() != 0 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	cfg.Timeout += time.Second
	changed, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	changedPlan, err := changed.PlanSearch(request)
	if err != nil || changedPlan.RequestDigest == plan.RequestDigest || changedPlan.PolicyDigest == plan.PolicyDigest {
		t.Fatalf("changed plan=%+v err=%v", changedPlan, err)
	}
	id := domain.AttemptCallID("call_00000000000000000000000000001701")
	result, err := adapter.SearchPhysical(context.Background(), request, id)
	if err != nil || result.Execution.Value == nil || result.Execution.Boundary != domain.BoundaryCompleted || !result.CostVerified || sends.Load() != 1 {
		t.Fatalf("result=%+v err=%v HTTP=%d", result, err, sends.Load())
	}
	evidence := result.Execution.Value
	if err := result.Execution.Validate(); err != nil {
		t.Fatal(err)
	}
	if evidence.CallTrace.LogicalOperationID != request.LogicalIdempotencyKey || len(evidence.CallTrace.PhysicalAttemptCallIDs) != 1 || evidence.CallTrace.PhysicalAttemptCallIDs[0] != id || evidence.Usage.CostMicroUSD != 11 {
		t.Fatalf("evidence=%+v", evidence)
	}
	if err := adapter.ValidatePhysicalResponse(request, *evidence); err != nil {
		t.Fatal(err)
	}
	if result.Execution.ResponseDigest == nil || *result.Execution.ResponseDigest != domain.SumBytes([]byte(responseBody)) {
		t.Fatal("physical receipt lacks exact HTTP response digest")
	}
	other := request
	other.LogicalIdempotencyKey = "other-similarity-operation"
	if err := adapter.ValidatePhysicalResponse(other, *evidence); err == nil {
		t.Fatal("response accepted for another operation")
	}
}

func TestPhysicalSimilarityNeverRunsCompatibilityRetryLoop(t *testing.T) {
	t.Setenv("CPGEN_SIM_TEST_KEY", "physical-fixture-key")
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "http-503", true: "unknown"}[unknown], func(t *testing.T) {
			var sends atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				if unknown {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err == nil {
						_ = conn.Close()
					}
					return
				}
				w.WriteHeader(http.StatusServiceUnavailable)
			}))
			t.Cleanup(server.Close)
			cfg := testHTTPConfig(server.URL)
			cfg.MaxAttempts = 4
			adapter, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			result, err := adapter.SearchPhysical(context.Background(), physicalSimilarityRequest(t), "call_00000000000000000000000000001702")
			if err != nil || result.Execution.Failure == nil || sends.Load() != 1 {
				t.Fatalf("result=%+v err=%v HTTP=%d", result, err, sends.Load())
			}
			if err := result.Execution.Validate(); err != nil {
				t.Fatal(err)
			}
			if unknown && result.Execution.Boundary != domain.BoundaryUnknown {
				t.Fatal("lost unknown send boundary")
			}
			if !unknown && (result.Execution.Boundary != domain.BoundaryCompleted || result.Execution.Failure.Class != domain.FailureRetryable) {
				t.Fatal("lost completed HTTP retryable failure")
			}
		})
	}
}

func physicalSimilarityRequest(t *testing.T) Request {
	t.Helper()
	policy, err := NewPolicy("physical/v1", .5, .8, .5, .8, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := NewPackageSafeProjection("title", "statement", []string{"tag"}, "go")
	if err != nil {
		t.Fatal(err)
	}
	request, err := NewRequest(projection, policy.PolicyRef, policy.PolicyDigest, "physical-similarity-operation")
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func TestPhysicalSimilarityPreservesEmptyTagsAndExplicitZeroCost(t *testing.T) {
	t.Setenv("CPGEN_SIM_TEST_KEY", "physical-fixture-key")
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "explicit-zero"}[explicit], func(t *testing.T) {
			body := `{"hits":[],"usage":{"input_tokens":0,"output_tokens":0}}`
			if explicit {
				body = `{"hits":[],"usage":{"input_tokens":0,"output_tokens":0,"cost_micro_usd":0}}`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
			t.Cleanup(server.Close)
			adapter, err := New(testHTTPConfig(server.URL))
			if err != nil {
				t.Fatal(err)
			}
			request := physicalSimilarityRequest(t)
			projection, err := NewPackageSafeProjection("title", "statement", []string{}, "go")
			if err != nil {
				t.Fatal(err)
			}
			request, err = NewRequest(projection, request.PolicyRef, request.PolicyDigest, request.LogicalIdempotencyKey)
			if err != nil {
				t.Fatal(err)
			}
			if request.NormalizedTags == nil {
				t.Fatal("fixture must preserve a nonnil empty tag list")
			}
			result, err := adapter.SearchPhysical(context.Background(), request, "call_00000000000000000000000000001703")
			if err != nil || result.Execution.Value == nil || result.Execution.Validate() != nil || result.CostVerified != explicit {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if err := adapter.ValidatePhysicalResponse(request, *result.Execution.Value); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPhysicalSimilarityPreflightAndIncompleteResponsesHaveExactBoundaries(t *testing.T) {
	for _, kind := range []string{"cancelled", "credential", "malformed", "oversize"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("CPGEN_SIM_TEST_KEY", "physical-fixture-key")
			var sends atomic.Int32
			body := "malformed response"
			if kind == "oversize" {
				body = strings.Repeat("x", 257)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sends.Add(1); _, _ = w.Write([]byte(body)) }))
			t.Cleanup(server.Close)
			cfg := testHTTPConfig(server.URL)
			cfg.MaxResponseBytes = 256
			adapter, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if kind == "cancelled" {
				cancel()
			}
			if kind == "credential" {
				t.Setenv("CPGEN_SIM_TEST_KEY", "")
			}
			result, err := adapter.SearchPhysical(ctx, physicalSimilarityRequest(t), "call_00000000000000000000000000001704")
			if err != nil || result.Execution.Failure == nil || result.Execution.Validate() != nil {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			expected, count := domain.BoundaryConfirmedNoSend, int32(0)
			if kind == "malformed" {
				expected, count = domain.BoundaryCompleted, 1
			}
			if kind == "oversize" {
				expected, count = domain.BoundaryUnknown, 1
			}
			if result.Execution.Boundary != expected || sends.Load() != count {
				t.Fatalf("boundary=%s HTTP=%d", result.Execution.Boundary, sends.Load())
			}
		})
	}
}
