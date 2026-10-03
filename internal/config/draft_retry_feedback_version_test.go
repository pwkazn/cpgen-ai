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

func TestDraftRetryFeedbackVersionPreservesHistoricalEffectiveBytes(t *testing.T) {
	base := solutionConfigYAML(t)
	for _, revision := range []string{workflow.LegacySimilarityCheckpointRevision, workflow.LegacySolutionCheckpointRevision, workflow.GenerationRevision, workflow.RetryingGenerationRevision, workflow.ExecutedSamplesRevision} {
		t.Run(revision, func(t *testing.T) {
			raw := strings.Replace(base, workflow.LegacySolutionCheckpointRevision, revision, 1)
			cfg, err := config.Decode([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			effective, err := cfg.Effective()
			if err != nil || bytes.Contains(effective, []byte("draft_retry_feedback_version")) {
				t.Fatalf("omitted selection changed wire shape: %s %v", effective, err)
			}
			// Pin the historical provider object without using EffectiveLLM so
			// adding fields or defaults cannot silently change persisted identity.
			var historical map[string]json.RawMessage
			if err := json.Unmarshal(effective, &historical); err != nil {
				t.Fatal(err)
			}
			historical["llm"] = json.RawMessage(`{"api_key_env":"CPGEN_TEST_PROVIDER_KEY","base_url":"https://api.deepseek.com","max_output_tokens":4096,"max_response_bytes":1048576,"model":"deepseek-v4-flash","timeout":"30s"}`)
			want, err := json.Marshal(historical)
			if err != nil || !bytes.Equal(effective, want) || cfg.EffectiveDigest() != domain.SumBytes(want) {
				t.Fatalf("historical effective bytes/digest changed: %v", err)
			}
			empty, err := config.Decode([]byte(strings.Replace(raw, "llm:\n", "llm:\n  draft_retry_feedback_version: \"\"\n", 1)))
			if err != nil || empty.EffectiveDigest() != cfg.EffectiveDigest() {
				t.Fatalf("empty version differs from omission: %v", err)
			}
			restored, err := config.DecodeEffective(want)
			if err != nil || restored.LLM.DraftRetryFeedbackVersion != "" || restored.EffectiveDigest() != cfg.EffectiveDigest() {
				t.Fatalf("historical config no longer round-trips: %v", err)
			}
		})
	}
}

func TestDraftRetryFeedbackVersionIsExplicitAndFrozen(t *testing.T) {
	base := strings.Replace(solutionConfigYAML(t), workflow.LegacySolutionCheckpointRevision, workflow.ExecutedSamplesRevision, 1)
	legacy, err := config.Decode([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	selected := strings.Replace(base, "llm:\n", "llm:\n  draft_retry_feedback_version: v1\n", 1)
	cfg, err := config.Decode([]byte(selected))
	if err != nil || cfg.LLM.DraftRetryFeedbackVersion != "v1" || cfg.EffectiveDigest() == legacy.EffectiveDigest() {
		t.Fatalf("explicit version was not selected and digest-bound: %v", err)
	}
	frozen, err := cfg.Effective()
	if err != nil || !bytes.Contains(frozen, []byte(`"draft_retry_feedback_version":"v1"`)) {
		t.Fatalf("effective selection missing: %s %v", frozen, err)
	}
	restored, err := config.DecodeEffective(frozen)
	if err != nil || restored.LLM.DraftRetryFeedbackVersion != "v1" || restored.EffectiveDigest() != cfg.EffectiveDigest() {
		t.Fatalf("selected version did not round-trip: %v", err)
	}
	for _, version := range []string{"v3", "v5"} {
		withData, err := config.Decode([]byte(strings.Replace(selected, "llm:\n", "llm:\n  data_prompt_version: "+version+"\n", 1)))
		if err != nil || withData.LLM.DraftRetryFeedbackVersion != "v1" || withData.LLM.DataPromptVersion != version {
			t.Fatalf("draft feedback could not coexist with data prompt %s: %v", version, err)
		}
	}
	for _, value := range []string{"v0", "v2", `" v1"`, `"v1 "`, "null", "1", "true", "[]", "{}"} {
		assertConfigField(t, strings.Replace(selected, "draft_retry_feedback_version: v1", "draft_retry_feedback_version: "+value, 1), "llm.draft_retry_feedback_version")
	}
	assertConfigField(t, strings.Replace(selected, "draft_retry_feedback_version: v1", "draft_retry_feedback_version: v1\n  draft_retry_feedback_version: v1", 1), "llm.draft_retry_feedback_version")
	assertConfigField(t, strings.Replace(selected, "draft_retry_feedback_version:", "draft_retry_feedback_versions:", 1), "llm.draft_retry_feedback_versions")
	for _, revision := range []string{workflow.LegacySimilarityCheckpointRevision, workflow.LegacySolutionCheckpointRevision, workflow.GenerationRevision, workflow.RetryingGenerationRevision} {
		assertConfigField(t, strings.Replace(selected, workflow.ExecutedSamplesRevision, revision, 1), "llm.draft_retry_feedback_version")
	}
	assertConfigField(t, configBase(t)+providerYAML+"  draft_retry_feedback_version: v1\n", "llm.draft_retry_feedback_version")
	for _, mutate := range []func(*config.Config){
		func(c *config.Config) { c.LLM.DraftRetryFeedbackVersion = "v2" },
		func(c *config.Config) { c.Workflow.Revision = workflow.GenerationRevision },
		func(c *config.Config) { c.Workflow = nil },
	} {
		changed, err := config.DecodeEffective(frozen)
		if err != nil {
			t.Fatal(err)
		}
		mutate(&changed)
		var fieldErr *config.FieldError
		if err := changed.Validate(); !errors.As(err, &fieldErr) || fieldErr.Field != "llm.draft_retry_feedback_version" {
			t.Fatalf("programmatic invalid selection accepted: %v", err)
		}
		if _, err := changed.Effective(); err == nil || changed.EffectiveDigest() != "" {
			t.Fatal("invalid selection produced frozen policy")
		}
	}
}
