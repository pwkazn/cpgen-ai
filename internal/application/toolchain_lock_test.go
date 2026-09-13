package application

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/toolchain"
)

func TestToolchainSnapshotBindingAndFallback(t *testing.T) {
	root := t.TempDir()
	lockPath := filepath.Join(root, "lock.json")
	raw, err := os.ReadFile("../../config/toolchains/docker-v1.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := toolchain.LoadLock(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := lock.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../config/mvp.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.NewReplacer(
		"D:/cpgen-private/mvp", filepath.ToSlash(filepath.Join(root, "state")),
		"D:/cpgen-private/toolchains/docker-v1.lock.json", filepath.ToSlash(lockPath),
		"sha256:"+strings.Repeat("0", 64), string(digest),
	).Replace(string(example))
	cfg, err := config.Decode([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if err := bindToolchainLockSnapshot(&cfg); err != nil {
		t.Fatal(err)
	}
	canonical, err := lock.MarshalIndent()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cfg.Sandbox.ToolchainLockSnapshot, canonical) {
		t.Fatal("binding did not save canonical lock bytes")
	}
	if err := os.WriteFile(lockPath, []byte("changed external lock"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfiguredToolchainLock(cfg); err != nil {
		t.Fatalf("bound snapshot did not take precedence: %v", err)
	}

	legacy := cfg
	legacySandbox := *cfg.Sandbox
	legacySandbox.ToolchainLockSnapshot = []byte{}
	legacy.Sandbox = &legacySandbox
	if err := bindToolchainLockSnapshot(&legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Sandbox.ToolchainLockSnapshot == nil || len(legacy.Sandbox.ToolchainLockSnapshot) != 0 {
		t.Fatal("legacy sentinel was upgraded")
	}
	if err := os.WriteFile(lockPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfiguredToolchainLock(legacy); err != nil {
		t.Fatalf("legacy path fallback failed: %v", err)
	}

	for _, snapshot := range [][]byte{[]byte("malformed lock"), canonical} {
		broken := cfg
		brokenSandbox := *cfg.Sandbox
		brokenSandbox.ToolchainLockSnapshot = snapshot
		brokenSandbox.ToolchainLockDigest = domain.SumBytes([]byte("wrong lock"))
		broken.Sandbox = &brokenSandbox
		if _, err := LoadConfiguredToolchainLock(broken); err == nil {
			t.Fatal("accepted malformed or wrong-digest snapshot")
		}
	}
}
