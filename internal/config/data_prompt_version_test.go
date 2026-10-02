package config_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestDataPromptVersionPreservesHistoricalEffectiveBytes(t *testing.T) {
	base := solutionConfigYAML(t)
	for _, revision := range []string{workflow.GenerationRevision, workflow.RetryingGenerationRevision, workflow.ExecutedSamplesRevision} {
		t.Run(revision, func(t *testing.T) {
			raw := strings.Replace(base, workflow.LegacySolutionCheckpointRevision, revision, 1)
			cfg, err := config.Decode([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			effective, err := cfg.Effective()
			if err != nil || bytes.Contains(effective, []byte("data_prompt_version")) {
				t.Fatalf("omitted selection changed wire shape: %s %v", effective, err)
			}
			// This is the exact pre-selection provider shape, independent of
			// EffectiveLLM's fields, including the absence of a default version.
			var historical map[string]json.RawMessage
			if err := json.Unmarshal(effective, &historical); err != nil {
				t.Fatal(err)
			}
			historical["llm"] = json.RawMessage(`{"api_key_env":"CPGEN_TEST_PROVIDER_KEY","base_url":"https://api.deepseek.com","max_output_tokens":4096,"max_response_bytes":1048576,"model":"deepseek-v4-flash","timeout":"30s"}`)
			want, err := json.Marshal(historical)
			if err != nil || !bytes.Equal(effective, want) || cfg.EffectiveDigest() != domain.SumBytes(want) {
				t.Fatalf("historical effective bytes/digest changed: %v", err)
			}
			for _, versionLine := range []string{"", "  data_prompt_version: \"\"\n"} {
				decoded, err := config.Decode([]byte(strings.Replace(raw, "llm:\n", "llm:\n"+versionLine, 1)))
				if err != nil || decoded.EffectiveDigest() != cfg.EffectiveDigest() {
					t.Fatalf("empty version differs from omission: %v", err)
				}
			}
			restored, err := config.DecodeEffective(want)
			if err != nil || restored.LLM.DataPromptVersion != "" || restored.EffectiveDigest() != cfg.EffectiveDigest() {
				t.Fatalf("historical config no longer round-trips: %v", err)
			}
		})
	}
}

func TestDataPromptVersionRequiresExplicitSupportedWorkflowAndIsFrozen(t *testing.T) {
	base := strings.Replace(solutionConfigYAML(t), workflow.LegacySolutionCheckpointRevision, workflow.ExecutedSamplesRevision, 1)
	selected := strings.Replace(base, "llm:\n", "llm:\n  data_prompt_version: v3\n", 1)
	old, err := config.Decode([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Decode([]byte(selected))
	if err != nil || cfg.LLM.DataPromptVersion != "v3" || cfg.EffectiveDigest() == old.EffectiveDigest() {
		t.Fatalf("explicit version was not selected and digest-bound: %v", err)
	}
	frozen, err := cfg.Effective()
	if err != nil || !bytes.Contains(frozen, []byte(`"data_prompt_version":"v3"`)) {
		t.Fatalf("effective selection missing: %s %v", frozen, err)
	}
	restored, err := config.DecodeEffective(frozen)
	if err != nil || restored.LLM.DataPromptVersion != "v3" || restored.EffectiveDigest() != cfg.EffectiveDigest() {
		t.Fatalf("selected version did not round-trip: %v", err)
	}
	v5YAML := strings.Replace(selected, "data_prompt_version: v3", "data_prompt_version: v5", 1)
	v5, err := config.Decode([]byte(v5YAML))
	if err != nil || v5.LLM.DataPromptVersion != "v5" {
		t.Fatalf("v5 was not accepted: %v", err)
	}
	v5Frozen, err := v5.Effective()
	if err != nil || !bytes.Contains(v5Frozen, []byte(`"data_prompt_version":"v5"`)) {
		t.Fatalf("v5 selection missing from frozen config: %s %v", v5Frozen, err)
	}
	v5Restored, err := config.DecodeEffective(v5Frozen)
	if err != nil || v5Restored.LLM.DataPromptVersion != "v5" || v5Restored.EffectiveDigest() != v5.EffectiveDigest() {
		t.Fatalf("v5 selection did not round-trip: %v", err)
	}
	for _, value := range []string{"v1", "v2", "v4", "v6", `" v3"`, "null", "3", "true", "[]", "{}"} {
		assertConfigField(t, strings.Replace(selected, "data_prompt_version: v3", "data_prompt_version: "+value, 1), "llm.data_prompt_version")
	}
	assertConfigField(t, strings.Replace(selected, "data_prompt_version: v3", "data_prompt_version: v3\n  data_prompt_version: v3", 1), "llm.data_prompt_version")
	for _, revision := range []string{workflow.LegacySimilarityCheckpointRevision, workflow.LegacySolutionCheckpointRevision, workflow.GenerationRevision, workflow.RetryingGenerationRevision} {
		assertConfigField(t, strings.Replace(selected, workflow.ExecutedSamplesRevision, revision, 1), "llm.data_prompt_version")
		assertConfigField(t, strings.Replace(v5YAML, workflow.ExecutedSamplesRevision, revision, 1), "llm.data_prompt_version")
	}
	assertConfigField(t, configBase(t)+providerYAML+"  data_prompt_version: v3\n", "llm.data_prompt_version")
	assertConfigField(t, configBase(t)+providerYAML+"  data_prompt_version: v5\n", "llm.data_prompt_version")
	cfg.Workflow.Revision = workflow.GenerationRevision
	var fieldErr *config.FieldError
	if err := cfg.Validate(); !errors.As(err, &fieldErr) || fieldErr.Field != "llm.data_prompt_version" {
		t.Fatalf("programmatic invalid selection accepted: %v", err)
	}
	if _, err := cfg.Effective(); err == nil || cfg.EffectiveDigest() != "" {
		t.Fatal("invalid selection produced frozen policy")
	}
}
