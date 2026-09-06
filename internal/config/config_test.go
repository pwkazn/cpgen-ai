package config_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/config"
	"cpgen/internal/domain"
)

func TestDecodeStrictConfigDerivesPrivatePathsAndStableDigest(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	data := []byte("storage:\n  state_root: " + root + "\nsqlite:\n  busy_timeout: 5s\n  max_readers: 4\nruntime:\n  lock_poll_interval: 25ms\n  control_poll_interval: 100ms\n  accounting_heartbeat: 1s\n  cleanup_wait: 10s\nfake_workflow:\n  scenario: review\n")
	first, err := config.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	second, err := config.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if first.EffectiveDigest() == "" || first.EffectiveDigest() != second.EffectiveDigest() {
		t.Fatalf("unstable config digest: %q vs %q", first.EffectiveDigest(), second.EffectiveDigest())
	}
	if err := first.EffectiveDigest().Validate(); err != nil {
		t.Fatal(err)
	}
	paths := first.RootPaths()
	for _, path := range []string{paths.Database, paths.Artifacts, paths.Locks, paths.Temporary, paths.Quarantine, paths.Trash, paths.Work} {
		if !strings.HasPrefix(path, paths.StateRoot+string(filepath.Separator)) {
			t.Fatalf("derived path escaped state root: %q", path)
		}
	}
	effective, err := first.Effective()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(effective), "TOKEN") {
		t.Fatal("effective config exposed a raw credential")
	}
}

func TestDecodeRejectsDuplicateUnknownAndTrailingDocuments(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	base := "storage:\n  state_root: " + root + "\n"
	for name, data := range map[string]string{
		"unknown":   base + "unexpected: true\n",
		"duplicate": "storage:\n  state_root: " + root + "\nstorage:\n  state_root: " + root + "\n",
		"trailing":  base + "---\nstorage:\n  state_root: " + root + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := config.Decode([]byte(data)); err == nil {
				t.Fatal("invalid config was accepted")
			} else {
				var fieldErr *config.FieldError
				if !errors.As(err, &fieldErr) || fieldErr.Field == "" {
					t.Fatalf("error is not field-qualified: %v", err)
				}
			}
		})
	}
}

func TestDecodeRejectsInvalidRootDurationAndScenarioWithTypedField(t *testing.T) {
	for name, data := range map[string]string{
		"relative root": "storage:\n  state_root: relative\n",
		"zero timeout":  "storage:\n  state_root: " + filepath.Join(t.TempDir(), "state") + "\nsqlite:\n  busy_timeout: 0s\n",
		"bad relation":  "storage:\n  state_root: " + filepath.Join(t.TempDir(), "state") + "\nruntime:\n  lock_poll_interval: 2s\n  control_poll_interval: 1s\n",
		"scenario":      "storage:\n  state_root: " + filepath.Join(t.TempDir(), "state") + "\nfake_workflow:\n  scenario: remote\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := config.Decode([]byte(data))
			if err == nil {
				t.Fatal("invalid config was accepted")
			}
			var fieldErr *config.FieldError
			if !errors.As(err, &fieldErr) {
				t.Fatalf("error is not field-qualified: %v", err)
			}
		})
	}
}

func TestConfigDigestIsAValidDomainDigest(t *testing.T) {
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.Join(t.TempDir(), "state") + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.Digest(cfg.Digest()).Validate(); err != nil {
		t.Fatal(err)
	}
}
