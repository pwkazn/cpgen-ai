package similarity

import (
	"context"
	"cpgen/internal/domain"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestReadPolicyMatchesPhysicalPlansAndSavedResponseValidation(t *testing.T) {
	t.Setenv("CPGEN_SIM_TEST_KEY", "fixture-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"provider_identity":"fixture","hits":[{"source":"catalog","external_id":"42","title":"fixture title","score":0.2}],"usage":{"input_tokens":7,"output_tokens":9,"cost_micro_usd":11}}`))
	}))
	defer server.Close()
	cfg := testHTTPConfig(server.URL)
	adapter, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := physicalSimilarityRequest(t)
	result, err := adapter.SearchPhysical(context.Background(), request, domain.AttemptCallID("call_00000000000000000000000000001701"))
	if err != nil || result.Execution.Value == nil {
		t.Fatalf("fixture response: %+v %v", result, err)
	}
	server.Close()
	t.Setenv("CPGEN_SIM_TEST_KEY", "")
	reader, err := NewReadPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.LogicalIdempotencyKey = "another-operation"
	for _, req := range []Request{request, changed} {
		expected, err := adapter.PlanSearch(req)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := reader.PlanSearch(req)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("plan differs: got=%+v want=%+v err=%v", actual, expected, err)
		}
	}
	for _, tc := range []struct {
		request Request
		valid   bool
	}{{request, true}, {changed, false}} {
		oldErr := adapter.ValidatePhysicalResponse(tc.request, *result.Execution.Value)
		newErr := reader.ValidatePhysicalResponse(tc.request, *result.Execution.Value)
		if (oldErr == nil) != tc.valid || (newErr == nil) != tc.valid {
			t.Fatalf("validation differs: physical=%v reader=%v valid=%t", oldErr, newErr, tc.valid)
		}
	}
}
