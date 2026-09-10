package application_test

import (
	"testing"
	"time"

	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

func explicitSlice2ApplicationConfig(t *testing.T) config.Config {
	t.Helper()
	cfg := llmApplicationConfig(t)
	cfg.Workflow = &config.WorkflowConfig{Revision: workflow.Slice2CheckpointWorkflowRevision, IdeaCount: 2, LLMCostUpperBoundMicroUSD: 1000, SimilarityCostUpperBoundMicroUSD: 100}
	cfg.Similarity = &config.SimilarityConfig{Endpoint: "https://similarity.example.com/search", APIKeyEnv: "CPGEN_SIMILARITY_CONFIG_TEST_KEY", ProviderIdentity: "fixture", ServiceIdentity: "index-v1", Timeout: 30 * time.Second, MaxResponseBytes: 16384, Limit: 10, PolicyRef: "similarity-policy/v1", AcceptanceThreshold: .5, RejectionThreshold: .8, MinimumHits: 1}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBuildSimilarityConfigPreservesFrozenPolicyAndOnePhysicalExchange(t *testing.T) {
	cfg := explicitSlice2ApplicationConfig(t)
	t.Setenv(cfg.Similarity.APIKeyEnv, "")
	mapped, policy, err := application.BuildSimilarityConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.Endpoint != cfg.Similarity.Endpoint || mapped.APIKeyEnv != cfg.Similarity.APIKeyEnv || mapped.ProviderIdentity != "fixture" || mapped.ServiceIdentity != "index-v1" || mapped.Timeout != 30*time.Second || mapped.MaxResponseBytes != 16384 || mapped.MaxHits != 10 || mapped.MaxAttempts != 1 {
		t.Fatalf("similarity mapping lost settings: %+v", mapped)
	}
	if mapped.AllowInsecureHTTP || mapped.AllowLoopbackForTesting || mapped.HTTPClient != nil || mapped.Now != nil {
		t.Fatal("live configuration admitted test overrides")
	}
	if _, err := similarity.New(mapped); err != nil {
		t.Fatalf("construction required credentials: %v", err)
	}
	if policy.Validate() != nil || policy.PolicyRef != cfg.Similarity.PolicyRef || policy.AcceptanceThreshold != .5 || policy.RejectionThreshold != .8 || policy.ReviewBandLower != .5 || policy.ReviewBandUpper != .8 || policy.MinimumHits != 1 {
		t.Fatalf("decision policy mapping differs: %+v", policy)
	}
}

func TestBuildSlice2SettingsFreezesTypedContentAndApplicationRetryPolicy(t *testing.T) {
	cfg := explicitSlice2ApplicationConfig(t)
	content, retry, err := application.BuildSlice2ExecutionSettings(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if content.IdeaCount != 2 || content.SelectionPolicy != domain.SelectionOrdinalPolicyV1 || content.StatementRevision != 1 || content.ProviderPolicyDigest != cfg.EffectiveDigest() || content.MaxOutput.Tokens != cfg.LLM.MaxOutputTokens || content.MaxOutput.Bytes != cfg.LLM.MaxResponseBytes || retry.Validate() != nil || retry.MaxAttempts != 2 {
		t.Fatalf("content=%+v retry=%+v", content, retry)
	}
	copy := cfg
	workflowCopy := *cfg.Workflow
	copy.Workflow = &workflowCopy
	copy.Workflow.LLMCostUpperBoundMicroUSD++
	changed, changedRetry, err := application.BuildSlice2ExecutionSettings(copy)
	if err != nil || changed.ProviderPolicyDigest == content.ProviderPolicyDigest || changedRetry.JitterSeedDigest == retry.JitterSeedDigest {
		t.Fatalf("execution settings lost frozen config binding: %v", err)
	}
	cfg.Workflow = nil
	if _, _, err := application.BuildSlice2ExecutionSettings(cfg); err == nil {
		t.Fatal("provider-only settings selected live content")
	}
}
