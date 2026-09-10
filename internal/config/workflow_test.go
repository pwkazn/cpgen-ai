package config_test

import (
	"bytes"
	"strings"
	"testing"

	"cpgen/internal/config"
	"cpgen/internal/workflow"
)

const similarityYAML = "similarity:\n  endpoint: https://similarity.example.com/search\n  api_key_env: CPGEN_TEST_SIMILARITY_KEY\n  provider_identity: fixture\n  service_identity: index-v1\n  timeout: 30s\n  max_response_bytes: 1048576\n  limit: 20\n  policy_ref: similarity-policy/v1\n  acceptance_threshold: 0.5\n  rejection_threshold: 0.8\n  minimum_hits: 1\n"

func explicitWorkflowYAML() string {
	return "workflow:\n  revision: " + workflow.Slice2CheckpointWorkflowRevision + "\n  idea_count: 2\n  llm_cost_upper_bound_micro_usd: 10000\n  similarity_cost_upper_bound_micro_usd: 1000\n"
}

func TestWorkflowDefaultIdeaCountMatchesDeclaredContentPolicy(t *testing.T) {
	selector := strings.ReplaceAll(explicitWorkflowYAML(), "  idea_count: 2\n", "")
	cfg, err := config.Decode([]byte(configBase(t) + providerYAML + similarityYAML + selector))
	if err != nil || cfg.Workflow == nil || cfg.Workflow.IdeaCount != 4 {
		t.Fatalf("default candidate count=%+v %v", cfg.Workflow, err)
	}
}

func TestWorkflowSelectionIsExplicitAndFrozenWithoutCredentialValues(t *testing.T) {
	base := configBase(t)
	plain, err := config.Decode([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := plain.Effective()
	if err != nil || bytes.Contains(legacy, []byte(`"workflow"`)) || bytes.Contains(legacy, []byte(`"similarity"`)) {
		t.Fatalf("legacy snapshot changed: %v", err)
	}
	configured, err := config.Decode([]byte(base + providerYAML + similarityYAML))
	if err != nil || configured.Workflow != nil {
		t.Fatalf("provider settings selected live work: %v", err)
	}
	t.Setenv("CPGEN_TEST_PROVIDER_KEY", "private-provider-value")
	t.Setenv("CPGEN_TEST_SIMILARITY_KEY", "private-similarity-value")
	live, err := config.Decode([]byte(base + providerYAML + similarityYAML + explicitWorkflowYAML()))
	if err != nil || live.Workflow == nil || live.Similarity == nil || live.Workflow.Revision != workflow.Slice2CheckpointWorkflowRevision || live.Workflow.IdeaCount != 2 || live.Similarity.Limit != 20 {
		t.Fatalf("explicit selection=%+v err=%v", live.Workflow, err)
	}
	raw, err := live.Effective()
	if err != nil || bytes.Contains(raw, []byte("private-provider-value")) || bytes.Contains(raw, []byte("private-similarity-value")) || !bytes.Contains(raw, []byte("CPGEN_TEST_SIMILARITY_KEY")) {
		t.Fatalf("effective credential policy: %v", err)
	}
	initial := live.EffectiveDigest()
	t.Setenv("CPGEN_TEST_SIMILARITY_KEY", "rotated-private-value")
	if live.EffectiveDigest() != initial {
		t.Fatal("credential value changed policy identity")
	}
	for _, change := range []func(*config.Config){
		func(c *config.Config) { c.Workflow.IdeaCount++ },
		func(c *config.Config) { c.Workflow.LLMCostUpperBoundMicroUSD++ },
		func(c *config.Config) { c.Workflow.SimilarityCostUpperBoundMicroUSD++ },
		func(c *config.Config) { c.Similarity.Limit++ },
		func(c *config.Config) { c.Similarity.AcceptanceThreshold = .4 },
		func(c *config.Config) { c.Similarity.ServiceIdentity = "index-v2" },
	} {
		copy, err := config.Decode([]byte(base + providerYAML + similarityYAML + explicitWorkflowYAML()))
		if err != nil {
			t.Fatal(err)
		}
		change(&copy)
		if copy.EffectiveDigest() == initial || copy.EffectiveDigest() == "" {
			t.Fatal("live execution setting was not included in effective identity")
		}
	}
}

func TestWorkflowConfigurationRequiresCompleteClosedDependencies(t *testing.T) {
	base := configBase(t)
	assertConfigField(t, base+explicitWorkflowYAML()+similarityYAML, "llm")
	assertConfigField(t, base+providerYAML+explicitWorkflowYAML(), "similarity")
	complete := base + providerYAML + similarityYAML + explicitWorkflowYAML()
	for _, test := range []struct{ old, replacement, field string }{
		{workflow.Slice2CheckpointWorkflowRevision, workflow.Slice2WorkflowRevision, "workflow.revision"},
		{"idea_count: 2", "idea_count: 1", "workflow.idea_count"},
		{"idea_count: 2", "idea_count: 9", "workflow.idea_count"},
		{"llm_cost_upper_bound_micro_usd: 10000", "llm_cost_upper_bound_micro_usd: 0", "workflow.llm_cost_upper_bound_micro_usd"},
		{"similarity_cost_upper_bound_micro_usd: 1000", "similarity_cost_upper_bound_micro_usd: -1", "workflow.similarity_cost_upper_bound_micro_usd"},
		{"https://similarity.example.com/search", "http://localhost/search", "similarity.endpoint"},
		{"acceptance_threshold: 0.5", "acceptance_threshold: .nan", "similarity.acceptance_threshold"},
		{"acceptance_threshold: 0.5", "acceptance_threshold: 0.9", "similarity.acceptance_threshold"},
		{"minimum_hits: 1", "minimum_hits: 21", "similarity.minimum_hits"},
		{"limit: 20", "limit: 0", "similarity.limit"},
		{"timeout: 30s\n  max_response_bytes: 1048576\n  limit", "timeout: 0s\n  max_response_bytes: 1048576\n  limit", "similarity.timeout"},
		{"service_identity: index-v1", "service_identity: ${PRIVATE_KEY}", "similarity.service_identity"},
	} {
		assertConfigField(t, strings.Replace(complete, test.old, test.replacement, 1), test.field)
	}
	for _, field := range []struct{ prefix, key, value string }{
		{"workflow", "idea_count", "2"}, {"similarity", "limit", "20"}, {"similarity", "acceptance_threshold", "0.5"},
	} {
		for _, value := range []string{"null", "true", `"` + field.value + `"`, "[]", "{}"} {
			assertConfigField(t, strings.Replace(complete, field.key+": "+field.value, field.key+": "+value, 1), field.prefix+"."+field.key)
		}
	}
	assertConfigField(t, complete+"  arbitrary_stage: execute\n", "workflow.arbitrary_stage")
}
