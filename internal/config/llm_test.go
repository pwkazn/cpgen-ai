package config_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpgen/internal/config"
	"cpgen/internal/domain"
)

const providerYAML = "llm:\n  base_url: https://api.deepseek.com\n  model: deepseek-v4-flash\n  api_key_env: CPGEN_TEST_PROVIDER_KEY\n  timeout: 30s\n  max_output_tokens: 4096\n  max_response_bytes: 1048576\n"

func TestLLMFormatRepairPolicyIsExplicitBoundedAndDigestBound(t *testing.T) {
	base := configBase(t)
	disabled, err := config.Decode([]byte(base + providerYAML))
	if err != nil {
		t.Fatal(err)
	}
	zero, err := config.Decode([]byte(base + providerYAML + "  max_format_repairs: 0\n"))
	if err != nil || zero.Digest() != disabled.Digest() {
		t.Fatalf("explicit zero differs from omission: %v", err)
	}
	one, err := config.Decode([]byte(base + providerYAML + "  max_format_repairs: 1\n"))
	if err != nil || one.Digest() == disabled.Digest() {
		t.Fatalf("enabled repair missing from policy digest: %v", err)
	}
	raw, err := one.Effective()
	if err != nil || !bytes.Contains(raw, []byte(`"max_format_repairs":1`)) {
		t.Fatalf("effective repair policy missing: %v", err)
	}
	for _, value := range []string{"-1", "2", "null", "true", `"1"`, "1.5", "[]", "{}", "9223372036854775808"} {
		assertConfigField(t, base+providerYAML+"  max_format_repairs: "+value+"\n", "llm.max_format_repairs")
	}
}

func configBase(t *testing.T) string {
	t.Helper()
	return "storage:\n  state_root: " + filepath.Join(t.TempDir(), "state") + "\n"
}

func TestLLMEffectiveRetainsProviderWithoutCredentials(t *testing.T) {
	base := configBase(t)
	t.Setenv("CPGEN_TEST_PROVIDER_KEY", "")
	first, err := config.Decode([]byte(base + providerYAML))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := first.Effective()
	if err != nil {
		t.Fatal(err)
	}
	var effective map[string]json.RawMessage
	if err := json.Unmarshal(raw, &effective); err != nil {
		t.Fatal(err)
	}
	want := `{"api_key_env":"CPGEN_TEST_PROVIDER_KEY","base_url":"https://api.deepseek.com","max_output_tokens":4096,"max_response_bytes":1048576,"model":"deepseek-v4-flash","timeout":"30s"}`
	if string(effective["llm"]) != want {
		t.Fatalf("llm = %s, want %s", effective["llm"], want)
	}
	object, err := first.EffectiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	objectJSON, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(objectJSON, &decoded); err != nil {
		t.Fatal(err)
	}
	canonical, _ := json.Marshal(decoded)
	if !bytes.Equal(raw, canonical) {
		t.Fatal("Effective and EffectiveConfig disagree")
	}
	t.Setenv("CPGEN_TEST_PROVIDER_KEY", "fixture-secret-never-persist")
	second, err := config.Decode([]byte(base + strings.Replace(providerYAML, "30s", "30000ms", 1)))
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := second.Effective()
	if err != nil || !bytes.Equal(raw, secondJSON) || first.Digest() != second.Digest() || first.Digest() != domain.SumBytes(raw) {
		t.Fatalf("credentials or duration spelling changed snapshot/digest: %v", err)
	}
	for _, change := range [][2]string{
		{"api.deepseek.com", "api.example.com"}, {"deepseek-v4-flash", "another-model"},
		{"CPGEN_TEST_PROVIDER_KEY", "CPGEN_OTHER_PROVIDER_KEY"}, {"30s", "31s"},
		{"4096", "4097"}, {"1048576", "1048577"},
	} {
		changed, err := config.Decode([]byte(base + strings.Replace(providerYAML, change[0], change[1], 1)))
		if err != nil || changed.Digest() == first.Digest() {
			t.Fatalf("provider change %s missing from digest: %v", change[0], err)
		}
	}
}

func TestLLMStrictFields(t *testing.T) {
	base := configBase(t)
	invalid := map[string][]string{
		"base_url":           {`null`, `""`, `42`, `[]`, `{}`, `http://api.deepseek.com`, `https://localhost`, `https://127.0.0.1`, `https://10.0.0.1`, `https://[::1]`, `https://user:password@api.deepseek.com`, `https://api.deepseek.com?key=secret`, `https://api.deepseek.com?`, `https://api.deepseek.com#`, `https://api.deepseek.com:0`, `https://api.deepseek.com:65536`, `"https://api.deepseek.com:"`, `https://*.example.com`, `https://-bad.example.com`, `https://api.deepseek.com/v1/../private`, `https://api.deepseek.com/%2fprivate`, `" https://api.deepseek.com"`, `"https://api.deepseek.com/\\private"`},
		"model":              {`null`, `""`, `true`, `42`, `[]`, `{}`, `" leading"`, `"trailing "`, `"bad\nmodel"`, `"${MODEL}"`, `"$(MODEL)"`, `"$env:MODEL"`, fmt.Sprintf("%q", strings.Repeat("m", 257))},
		"api_key_env":        {`null`, `""`, `false`, `42`, `[]`, `{}`, `1BAD`, `BAD-NAME`, `"${KEY}"`, `" KEY"`, `"KEY=value"`},
		"timeout":            {`null`, `""`, `0s`, `-1s`, `601s`, `forever`, `30`, `[]`, `{}`},
		"max_output_tokens":  {`null`, `0`, `-1`, `1048577`, `9223372036854775808`, `1.5`, `.nan`, `"4096"`, `true`, `[]`, `{}`},
		"max_response_bytes": {`null`, `0`, `-1`, `67108865`, `9223372036854775808`, `1.5`, `.inf`, `"1048576"`, `false`, `[]`, `{}`},
	}
	for name, values := range invalid {
		for _, value := range values {
			t.Run(name+"/"+value, func(t *testing.T) {
				lines := strings.Split(providerYAML, "\n")
				for i, line := range lines {
					if strings.HasPrefix(line, "  "+name+":") {
						lines[i] = "  " + name + ": " + value
					}
				}
				assertConfigField(t, base+strings.Join(lines, "\n"), "llm."+name)
			})
		}
	}
	for _, name := range []string{"base_url", "model", "api_key_env"} {
		lines := strings.Split(providerYAML, "\n")
		for i, line := range lines {
			if strings.HasPrefix(line, "  "+name+":") {
				lines[i] = ""
			}
		}
		assertConfigField(t, base+strings.Join(lines, "\n"), "llm."+name)
	}
	for _, entry := range []struct{ yaml, field string }{
		{"llm: null\n", "llm"}, {"llm: []\n", "llm"}, {"llm: provider\n", "llm"},
		{providerYAML + "llm: {}\n", "llm"},
		{providerYAML + "  model: duplicate\n", "llm.model"},
		{providerYAML + "  endpoint: https://api.example.com\n", "llm.endpoint"},
		{providerYAML + "  api_key: secret\n", "llm.api_key"},
		{providerYAML + "  max_attempts: 2\n", "llm.max_attempts"},
		{providerYAML + "  prompt_registry: {}\n", "llm.prompt_registry"},
		{strings.Replace(providerYAML, "model: deepseek-v4-flash", "model: &model deepseek-v4-flash\n  api_key_env: *model", 1), "llm.api_key_env"},
		{"sqlite: null\n", "sqlite"}, {"runtime:\n  cleanup_wait: null\n", "runtime.cleanup_wait"},
		{"fake_workflow:\n  scenario: null\n", "fake_workflow.scenario"},
	} {
		assertConfigField(t, base+entry.yaml, entry.field)
	}
}

func assertConfigField(t *testing.T, data, want string) {
	t.Helper()
	_, err := config.Decode([]byte(data))
	var fieldErr *config.FieldError
	if !errors.As(err, &fieldErr) || fieldErr.Field != want {
		t.Fatalf("error = %v, want FieldError(%s)", err, want)
	}
}

func TestLLMOmittedKeepsLegacySnapshotBytesAndDigest(t *testing.T) {
	cfg, err := config.Decode([]byte(configBase(t)))
	if err != nil {
		t.Fatal(err)
	}
	// Frozen pre-LLM wire shape, independent of EffectiveConfig's future fields.
	legacy := map[string]any{
		"schema_version": "cpgen.config/v1",
		"storage":        map[string]any{"state_root": cfg.Paths.StateRoot},
		"sqlite":         map[string]any{"busy_timeout": "5s", "max_readers": 4},
		"runtime":        map[string]any{"lock_poll_interval": "25ms", "control_poll_interval": "100ms", "accounting_heartbeat": "1s", "cleanup_wait": "10s"},
		"fake_workflow":  map[string]any{"scenario": "review"},
		"paths": map[string]any{
			"state_root": cfg.Paths.StateRoot, "database": filepath.Join(cfg.Paths.StateRoot, "workflow.db"),
			"artifacts": filepath.Join(cfg.Paths.StateRoot, "artifacts"), "runtime": filepath.Join(cfg.Paths.StateRoot, "runtime"),
			"locks": filepath.Join(cfg.Paths.StateRoot, "runtime", "locks"), "temporary": filepath.Join(cfg.Paths.StateRoot, "artifacts", "tmp"),
			"quarantine": filepath.Join(cfg.Paths.StateRoot, "artifacts", "quarantine"), "trash": filepath.Join(cfg.Paths.StateRoot, "artifacts", "trash"),
			"work": filepath.Join(cfg.Paths.StateRoot, "work"),
		},
	}
	want, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cfg.Effective()
	if err != nil || !bytes.Equal(got, want) || cfg.Digest() != domain.SumBytes(want) {
		t.Fatalf("legacy snapshot/digest changed: %s, %v", got, err)
	}
}

func TestLLMDefaultsAndProgrammaticValidation(t *testing.T) {
	base := configBase(t)
	minimal := "llm:\n  base_url: https://api.deepseek.com\n  model: deepseek-v4-flash\n  api_key_env: CPGEN_TEST_PROVIDER_KEY\n"
	cfg, err := config.Decode([]byte(base + minimal))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM == nil || cfg.LLM.Timeout != 30*time.Second || cfg.LLM.MaxOutputTokens != 4096 || cfg.LLM.MaxResponseBytes != 1048576 {
		t.Fatalf("unexpected defaults: %+v", cfg.LLM)
	}
	explicit, err := config.Decode([]byte(base + providerYAML))
	if err != nil || explicit.Digest() != cfg.Digest() {
		t.Fatalf("explicit and implicit defaults differ: %v", err)
	}
	before := cfg.Digest()
	cfg.LLM.Model = "changed"
	if cfg.Digest() == before {
		t.Fatal("digest returned stale provider policy")
	}
	view, err := cfg.EffectiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	view.LLM.Model = "view-only"
	if cfg.LLM.Model != "changed" {
		t.Fatal("effective view aliases mutable configuration")
	}
	for name, mutate := range map[string]func(*config.LLMConfig){
		"base_url":           func(c *config.LLMConfig) { c.BaseURL = "http://api.example.com" },
		"model":              func(c *config.LLMConfig) { c.Model = "" },
		"api_key_env":        func(c *config.LLMConfig) { c.APIKeyEnv = "BAD-ENV" },
		"timeout":            func(c *config.LLMConfig) { c.Timeout = 0 },
		"max_output_tokens":  func(c *config.LLMConfig) { c.MaxOutputTokens = 0 },
		"max_response_bytes": func(c *config.LLMConfig) { c.MaxResponseBytes = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := cfg
			provider := *cfg.LLM
			bad.LLM = &provider
			mutate(bad.LLM)
			var fieldErr *config.FieldError
			if err := bad.Validate(); !errors.As(err, &fieldErr) || fieldErr.Field != "llm."+name {
				t.Fatalf("programmatic validation: %v", err)
			}
			if _, err := bad.Effective(); err == nil || bad.Digest() != "" {
				t.Fatal("invalid provider policy produced an effective snapshot/digest")
			}
		})
	}
}

func TestLLMDeepSeekExampleLoadsWithoutCredentials(t *testing.T) {
	raw, err := os.ReadFile("../../config/deepseek.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	// The example requires users to select their own absolute private root.
	raw = bytes.Replace(raw, []byte("D:/cpgen-private/state"), []byte(filepath.ToSlash(filepath.Join(t.TempDir(), "state"))), 1)
	t.Setenv("CPGEN_DEEPSEEK_API_KEY", "")
	cfg, err := config.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLM == nil || cfg.LLM.BaseURL != "https://api.deepseek.com" || cfg.LLM.Model != "deepseek-v4-flash" || cfg.LLM.APIKeyEnv != "CPGEN_DEEPSEEK_API_KEY" || cfg.FakeWorkflow.Scenario != "review" {
		t.Fatalf("example drifted from the configured integration target: %+v", cfg.LLM)
	}
}
