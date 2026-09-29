package application

import (
	"errors"
	"testing"

	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestValidateCreateRequestAcceptsFreeformGenerationTags(t *testing.T) {
	cfg := config.Config{Workflow: &config.WorkflowConfig{Revision: workflow.GenerationRevision}}
	request := domain.RunRequest{
		SchemaVersion: domain.RequestSchemaV1, Mode: domain.RequestModeManual,
		Brief: "localized prompt", Tags: []string{"图论", "动态规划"},
		NormalizedTags: []string{"动态规划", "图论"}, Language: "zh-CN", Difficulty: "hard",
		TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp",
		VerificationProfile: "default", ExportTargets: []string{"internal"},
	}
	if err := ValidateCreateRequest(cfg, request); err != nil {
		t.Fatalf("freeform tags rejected: %v", err)
	}

	request.NormalizedTags = []string{"graph"}
	if err := ValidateCreateRequest(cfg, request); !errors.Is(err, ErrInvalidCreateRequest) {
		t.Fatalf("deterministic request validation error is not classified: %v", err)
	}
}
