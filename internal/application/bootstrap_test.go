package application_test

import (
	"context"
	"path/filepath"
	"testing"
)

import (
	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestBootstrapBuildsFakeOnlyApplicationAndCloseIsIdempotent(t *testing.T) {

	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.Join(t.TempDir(), "state") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := application.Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if app.Runs == nil || app.Runtime == nil || app.Reviews == nil || app.Maintenance == nil || app.Locks == nil {
		t.Fatal("bootstrap returned an incomplete application")
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal("second Close is not idempotent: ", err)
	}
}

func TestBootstrapExplicitSlice2SelectionUsesDurablePreviewWithoutCredentials(t *testing.T) {
	cfg := explicitSlice2ApplicationConfig(t)
	t.Setenv(cfg.LLM.APIKeyEnv, "")
	t.Setenv(cfg.Similarity.APIKeyEnv, "")
	app, err := application.Bootstrap(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	request := domain.RunRequest{SchemaVersion: domain.RequestSchemaV1, Mode: domain.RequestModeManual, Brief: "Graphs", Language: "en", Difficulty: "hard", TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: domain.BudgetLimits{MaxActiveTimeMilliseconds: 60000}}
	result, err := app.Runs.Generate(context.Background(), request)
	if err != nil || result.WorkflowRevision != workflow.LegacySimilarityCheckpointRevision || result.State != domain.RunNeedsReview || result.CurrentStage != "idea" {
		t.Fatalf("explicit selector did not use budgeted live workflow: %+v %v", result, err)
	}
}
