package application

import (
	"bytes"
	"context"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
	"cpgen/internal/workflow"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type frozenPolicyFixture struct {
	FrozenReadPolicyStore
	run       domain.RunSnapshot
	raw       []byte
	stage     port.CommittedPrivateStage
	execution domain.SandboxExecution
}

func (f *frozenPolicyFixture) GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error) {
	return f.run, nil
}
func (f *frozenPolicyFixture) RunViewDocuments(context.Context, domain.RunID) ([]byte, []byte, error) {
	return nil, f.raw, nil
}
func (f *frozenPolicyFixture) ReadCommittedSandboxStage(context.Context, domain.RunID, domain.StageName) (port.CommittedPrivateStage, error) {
	return f.stage, nil
}
func (f *frozenPolicyFixture) GetSandboxExecution(context.Context, domain.SandboxExecutionID) (domain.SandboxExecution, error) {
	return f.execution, nil
}
func TestFrozenReadPolicyRestoresOfflinePolicyAndRejectsBrokenBindings(t *testing.T) {
	root := t.TempDir()
	lockBytes, err := os.ReadFile("../../config/toolchains/docker-v1.lock.json")
	if err != nil {
		t.Fatal(err)
	}
	lock, err := toolchain.LoadLock(bytes.NewReader(lockBytes))
	if err != nil {
		t.Fatal(err)
	}
	lockDigest, err := lock.Digest()
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(root, "lock.json")
	if err := os.WriteFile(lockPath, lockBytes, 0600); err != nil {
		t.Fatal(err)
	}
	example, err := os.ReadFile("../../config/mvp.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.NewReplacer("D:/cpgen-private/mvp", filepath.ToSlash(filepath.Join(root, "state")), "D:/cpgen-private/toolchains/docker-v1.lock.json", filepath.ToSlash(lockPath), "sha256:"+strings.Repeat("0", 64), string(lockDigest)).Replace(string(example))
	cfg, err := config.Decode([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := cfg.Effective()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	output := domain.SumBytes([]byte("report"))
	runID := domain.RunID("run_00000000000000000000000000000001")
	attemptID := domain.AttemptID("attempt_00000000000000000000000000000001")
	executionID := domain.SandboxExecutionID("sandbox_00000000000000000000000000000001")
	base := frozenPolicyFixture{
		run: domain.RunSnapshot{RunID: runID, ConfigDigest: domain.SumBytes(raw), WorkflowRevision: workflow.GenerationRevision, WorkflowDigest: domain.SumBytes([]byte(workflow.GenerationRevision))}, raw: raw,
		stage:     port.CommittedPrivateStage{Attempt: domain.StageAttempt{RunID: runID, AttemptID: attemptID, StageName: "solution_verify", Ordinal: 1, State: domain.StageAttemptSucceeded, InputDigest: domain.SumBytes([]byte("draft")), OutputDigest: &output, StartedAt: now, FinishedAt: &now}, Artifacts: []port.CommittedPrivateStageArtifact{{Blob: domain.CacheBlob{LogicalPath: domain.SafeRelPath("sandbox/" + string(executionID) + "/result.json")}}}},
		execution: domain.SandboxExecution{ID: executionID, RunID: runID, AttemptID: attemptID, StageName: "solution_verify", State: domain.SandboxExecutionCleaned, EngineIdentityDigest: domain.SumBytes([]byte("offline-engine"))},
	}
	for _, tc := range []struct {
		name   string
		change func(*frozenPolicyFixture)
	}{
		{"valid", func(*frozenPolicyFixture) {}},
		{"config_digest", func(f *frozenPolicyFixture) { f.run.ConfigDigest = output }},
		{"foreign_run", func(f *frozenPolicyFixture) { f.execution.RunID = "run_00000000000000000000000000000002" }},
		{"old_attempt", func(f *frozenPolicyFixture) { f.execution.AttemptID = "attempt_00000000000000000000000000000002" }},
		{"uncleaned", func(f *frozenPolicyFixture) { f.execution.State = domain.SandboxExecutionState("PREPARED") }},
		{"missing_receipt", func(f *frozenPolicyFixture) { f.stage.Artifacts = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := base
			tc.change(&f)
			restored, policy, err := NewFrozenReadPolicy(context.Background(), &f, runID)
			if tc.name != "valid" {
				if err == nil {
					t.Fatal("accepted broken binding")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if restored.EffectiveDigest() != cfg.EffectiveDigest() || policy.EngineIdentity != base.execution.EngineIdentityDigest {
				t.Fatal("offline policy changed or constructed execution capability")
			}
		})
	}
	if err := os.Remove(lockPath); err != nil {
		t.Fatal(err)
	}
	if _, _, err := NewFrozenReadPolicy(context.Background(), &base, runID); err == nil {
		t.Fatal("accepted missing frozen lock")
	}
}
