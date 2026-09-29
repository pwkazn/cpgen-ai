package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cpgen/internal/config"
	"cpgen/internal/workflow"
	"github.com/gin-gonic/gin"
)

func TestCreateReturnsActionableClientErrorForInvalidGenerationTags(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body, err := json.Marshal(createRequestBody{
		OperationKey: "validation-case",
		Request: mustJSONForCreateTest(t, runRequestDTO{
			SchemaVersion: "cpgen.request/v1", Mode: "manual", Brief: "tag validation fixture",
			Tags: []string{"图论"}, NormalizedTags: []string{"graphs"}, Language: "zh-CN", Difficulty: "hard",
			TimeLimitMilliseconds: "2000", MemoryLimitMegabytes: "512", SolutionLanguage: "cpp",
			VerificationProfile: "default", ExportTargets: []string{"internal"},
			BudgetLimits: budgetDTO{
				MaxLLMCalls: "0", MaxSimilarityCalls: "0", MaxLLMInputTokens: "0", MaxLLMOutputTokens: "0",
				MaxLLMCostUSD: "0", MaxSimilarityCostUSD: "0", MaxSandboxCreates: "0", MaxArtifactBytes: "0",
				MaxPackageBytes: "0", MaxMutationsPerStage: "0", MaxActiveTimeMilliseconds: "5000",
			},
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/runs/create", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = request
	server := &Server{cfg: config.Config{Workflow: &config.WorkflowConfig{Revision: workflow.GenerationRevision}}}
	server.create(ctx)

	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("HTTP %d, want 422: %s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "算法标签") || !strings.Contains(response.Body.String(), "修改后重试") {
		t.Fatalf("client error is not actionable: %s", response.Body.String())
	}
	if strings.Contains(response.Body.String(), "normalized_tags must") {
		t.Fatalf("internal validation detail leaked: %s", response.Body.String())
	}
}

func mustJSONForCreateTest(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestCreateKeepsInternalErrorsAsGenericServerErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	writeApplicationError(ctx, errors.New("private database failure"))
	if response.Code != http.StatusInternalServerError || strings.Contains(response.Body.String(), "private database failure") {
		t.Fatalf("internal error was misclassified or leaked: HTTP %d: %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf("internal error was reported as user input: %s", response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "操作失败") {
		t.Fatalf("generic server error message missing: %s", response.Body.String())
	}
}
