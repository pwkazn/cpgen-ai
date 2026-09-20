package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

type snapshotRuntime struct {
	port.RuntimeStore
	run domain.RunSnapshot
	raw []byte
}

func (s *snapshotRuntime) GetRun(_ context.Context, runID domain.RunID) (domain.RunSnapshot, error) {
	if runID != s.run.RunID {
		return domain.RunSnapshot{}, errors.New("unexpected run id")
	}
	return s.run, nil
}

func (s *snapshotRuntime) RunViewDocuments(_ context.Context, runID domain.RunID) ([]byte, []byte, error) {
	if runID != s.run.RunID {
		return nil, nil, errors.New("unexpected run id")
	}
	return nil, s.raw, nil
}

func TestRestoreToolchainSnapshotPreservesNewAndLegacyBindings(t *testing.T) {
	root := t.TempDir()
	lockRaw, err := os.ReadFile(filepath.Join("..", "..", "config", "toolchains", "docker-v1.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	lock, err := toolchain.LoadLock(bytes.NewReader(lockRaw))
	if err != nil {
		t.Fatal(err)
	}
	lockDigest, err := lock.Digest()
	if err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile(filepath.Join("..", "..", "config", "mvp.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	source := strings.NewReplacer(
		"D:/cpgen-private/mvp", filepath.ToSlash(filepath.Join(root, "state")),
		"D:/cpgen-private/toolchains/docker-v1.lock.json", filepath.ToSlash(filepath.Join(root, "missing.lock.json")),
		"sha256:"+strings.Repeat("0", 64), string(lockDigest),
	).Replace(string(example))
	cfg, err := config.Decode([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := lock.MarshalIndent()
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("run_00000000000000000000000000000001")
	for _, tc := range []struct {
		name     string
		withLock bool
	}{
		{name: "new", withLock: true},
		{name: "legacy", withLock: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := cfg
			if tc.withLock {
				sandbox := *stored.Sandbox
				sandbox.ToolchainLockSnapshot = canonical
				stored.Sandbox = &sandbox
			}
			raw, err := stored.Effective()
			if err != nil {
				t.Fatal(err)
			}
			app := &application.Application{Runtime: &snapshotRuntime{run: domain.RunSnapshot{RunID: runID, ConfigDigest: domain.SumBytes(raw)}, raw: raw}}
			deps := Dependencies{BootstrapLocal: func(context.Context, config.Config) (*application.Application, error) { return app, nil }}
			restored, err := restoreToolchainSnapshot(context.Background(), cfg, runID, deps)
			if err != nil {
				t.Fatal(err)
			}
			if tc.withLock {
				if !bytes.Equal(restored.Sandbox.ToolchainLockSnapshot, canonical) {
					t.Fatal("new run did not restore its bound snapshot")
				}
			} else if restored.Sandbox.ToolchainLockSnapshot == nil || len(restored.Sandbox.ToolchainLockSnapshot) != 0 {
				t.Fatal("legacy run was not marked as snapshot-free")
			}
		})
	}
	stored := cfg
	sandbox := *stored.Sandbox
	sandbox.ToolchainLockSnapshot = canonical
	stored.Sandbox = &sandbox
	raw, err := stored.Effective()
	if err != nil {
		t.Fatal(err)
	}
	app := &application.Application{Runtime: &snapshotRuntime{run: domain.RunSnapshot{RunID: runID, ConfigDigest: domain.SumBytes(raw)}, raw: raw}}
	deps := Dependencies{BootstrapLocal: func(context.Context, config.Config) (*application.Application, error) { return app, nil }}
	for _, args := range [][]string{
		{"run", "cancel", string(runID), "--reason", "run_00000000000000000000000000000002"},
		{"review", "retry", string(runID), "--reviewer", "test", "--reason", "run_00000000000000000000000000000002", "--evidence", string(domain.SumBytes([]byte("evidence")))},
	} {
		command, parseErr := parseStatefulCommand(args)
		if parseErr != nil {
			t.Fatal(parseErr.err)
		}
		if _, err := restoreToolchainSnapshot(context.Background(), cfg, command.runID, deps); err != nil {
			t.Fatalf("restore args %v: %v", args, err)
		}
	}
	corrupt := stored
	corruptSandbox := *corrupt.Sandbox
	corruptSandbox.ToolchainLockSnapshot = []byte("malformed lock")
	corrupt.Sandbox = &corruptSandbox
	corruptRaw, err := corrupt.Effective()
	if err != nil {
		t.Fatal(err)
	}
	corruptApp := &application.Application{Runtime: &snapshotRuntime{run: domain.RunSnapshot{RunID: runID, ConfigDigest: domain.SumBytes(corruptRaw)}, raw: corruptRaw}}
	corruptDeps := Dependencies{BootstrapLocal: func(context.Context, config.Config) (*application.Application, error) { return corruptApp, nil }}
	if _, err := restoreToolchainSnapshot(context.Background(), cfg, runID, corruptDeps); err == nil {
		t.Fatal("accepted corrupt snapshot before execution bootstrap")
	}
	drift := cfg
	driftSandbox := *drift.Sandbox
	driftSandbox.EngineEndpoint = "unix:///different/docker.sock"
	drift.Sandbox = &driftSandbox
	if _, err := restoreToolchainSnapshot(context.Background(), drift, runID, deps); err == nil {
		t.Fatal("accepted current configuration drift")
	}
}
