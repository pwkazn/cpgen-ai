package config_test

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func solutionConfigYAML(t *testing.T) string {
	t.Helper()
	return configBase(t) + providerYAML + similarityYAML +
		strings.Replace(explicitWorkflowYAML(), workflow.LegacySimilarityCheckpointRevision, workflow.LegacySolutionCheckpointRevision, 1) +
		"sandbox:\n  engine_endpoint: unix:///var/run/docker.sock\n  toolchain_lock_path: " + filepath.ToSlash(filepath.Join(t.TempDir(), "toolchain.lock.json")) +
		"\n  toolchain_lock_digest: " + string(domain.SumBytes([]byte("fixture lock"))) + "\n"
}

func TestSolutionConfigurationRequiresClosedPinnedLocalSandbox(t *testing.T) {
	raw := solutionConfigYAML(t)
	cfg, err := config.Decode([]byte(raw))
	if err != nil || cfg.Workflow.Revision != workflow.LegacySolutionCheckpointRevision || cfg.Sandbox == nil {
		t.Fatalf("forward configuration: %+v %v", cfg, err)
	}
	assertConfigField(t, raw[:strings.Index(raw, "sandbox:\n")], "sandbox")
	mvp := strings.Replace(raw, workflow.LegacySolutionCheckpointRevision, workflow.GenerationRevision, 1)
	if parsed, err := config.Decode([]byte(mvp)); err != nil || parsed.Workflow.Revision != workflow.GenerationRevision || parsed.Sandbox == nil {
		t.Fatalf("MVP configuration: %+v %v", parsed, err)
	}
	assertConfigField(t, mvp[:strings.Index(mvp, "sandbox:\n")], "sandbox")
	for _, test := range []struct{ old, replacement, field string }{
		{"unix:///var/run/docker.sock", "tcp://127.0.0.1:2375", "sandbox.engine_endpoint"},
		{filepath.ToSlash(cfg.Sandbox.ToolchainLockPath), "relative/lock.json", "sandbox.toolchain_lock_path"},
		{string(cfg.Sandbox.ToolchainLockDigest), "latest", "sandbox.toolchain_lock_digest"},
		{"unix:///var/run/docker.sock", "${DOCKER_HOST}", "sandbox.engine_endpoint"},
		{"unix:///var/run/docker.sock", `"unix:///var/run/docker.sock\n"`, "sandbox.engine_endpoint"},
	} {
		assertConfigField(t, strings.Replace(raw, test.old, test.replacement, 1), test.field)
	}
	for _, item := range []struct{ field, value string }{
		{"engine_endpoint", cfg.Sandbox.EngineEndpoint},
		{"toolchain_lock_path", filepath.ToSlash(cfg.Sandbox.ToolchainLockPath)},
		{"toolchain_lock_digest", string(cfg.Sandbox.ToolchainLockDigest)},
	} {
		for _, bad := range []string{"null", "true", "123", "[]", "{}"} {
			assertConfigField(t, strings.Replace(raw, item.field+": "+item.value, item.field+": "+bad, 1), "sandbox."+item.field)
		}
	}
	assertConfigField(t, raw+"  arbitrary_command: execute\n", "sandbox.arbitrary_command")
}

func TestSolutionSandboxIsFrozenAndAbsentFromHistoricalSnapshots(t *testing.T) {
	legacy, err := config.Decode([]byte(configBase(t)))
	if err != nil {
		t.Fatal(err)
	}
	old, err := legacy.Effective()
	if err != nil || bytes.Contains(old, []byte(`"sandbox"`)) {
		t.Fatalf("historical config acquired sandbox: %v", err)
	}
	raw := solutionConfigYAML(t)
	cfg, err := config.Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	initial := cfg.EffectiveDigest()
	effective, err := cfg.EffectiveConfig()
	if err != nil {
		t.Fatal(err)
	}
	effective.Sandbox.ToolchainLockDigest = domain.SumBytes([]byte("other lock"))
	if cfg.EffectiveDigest() != initial {
		t.Fatal("effective sandbox aliases mutable configuration")
	}
	for _, change := range []func(*config.SandboxConfig){
		func(c *config.SandboxConfig) { c.EngineEndpoint = "unix:///another/docker.sock" },
		func(c *config.SandboxConfig) { c.ToolchainLockPath = filepath.Join(t.TempDir(), "other.json") },
		func(c *config.SandboxConfig) { c.ToolchainLockDigest = domain.SumBytes([]byte("other lock")) },
	} {
		copy, err := config.Decode([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		change(copy.Sandbox)
		if digest := copy.EffectiveDigest(); digest == "" || digest == initial {
			t.Fatal("sandbox setting was not bound to effective identity")
		}
	}
}
