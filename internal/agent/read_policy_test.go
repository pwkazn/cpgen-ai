package agent

import (
	"context"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestReadPolicyMatchesPhysicalPlansAndSavedResponseValidation(t *testing.T) {
	t.Setenv("CPGEN_TEST_LLM_KEY", "fixture-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, langchainSuccess) }))
	defer server.Close()
	cfg := testConfig(server.URL)
	model, err := NewLangChain(cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := testGenerateRequest()
	result, err := model.GeneratePhysical(context.Background(), request, domain.AttemptCallID("call_00000000000000000000000000000002"))
	if err != nil || result.Execution.Value == nil {
		t.Fatalf("fixture response: %+v %v", result, err)
	}
	server.Close()
	t.Setenv("CPGEN_TEST_LLM_KEY", "")
	reader, err := NewReadPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	variants := []port.GenerateRequest{request, request, request}
	variants[1].Sampling = port.SamplingPolicy{Temperature: 0.35, TopP: 0.72}
	variants[2].Variables = []byte(`{"brief":"changed input"}`)
	for _, req := range variants {
		expected, err := model.PlanGenerate(req)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := reader.PlanGenerate(req)
		if err != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("plan differs: got=%+v want=%+v err=%v", actual, expected, err)
		}
	}
	for _, tc := range []struct {
		response port.GenerateResponse
		valid    bool
	}{{*result.Execution.Value, true}, {port.GenerateResponse{}, false}} {
		oldErr := model.ValidatePhysicalResponse(request, tc.response)
		newErr := reader.ValidatePhysicalResponse(request, tc.response)
		if (oldErr == nil) != tc.valid || (newErr == nil) != tc.valid {
			t.Fatalf("validation differs: physical=%v reader=%v valid=%t", oldErr, newErr, tc.valid)
		}
	}
}
