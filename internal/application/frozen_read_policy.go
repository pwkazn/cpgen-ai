package application

import (
	"context"
	"errors"
	"strings"

	docker "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

type FrozenReadPolicyStore interface {
	GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
	RunViewDocuments(context.Context, domain.RunID) ([]byte, []byte, error)
	ReadCommittedSandboxStage(context.Context, domain.RunID, domain.StageName) (port.CommittedPrivateStage, error)
	port.SandboxLifecycleReader
}

// NewFrozenReadPolicy restores only the policy inputs needed to rebuild proof.
// The caller holds the run and artifact read locks. The returned sandbox value
// is verification policy, not an executable session: no Engine or Watchdog is
// constructed. New runs retain a validated lock snapshot; old runs retain only
// a toolchain path/digest and therefore fail closed when that lock is missing.
func NewFrozenReadPolicy(ctx context.Context, store FrozenReadPolicyStore, runID domain.RunID) (config.Config, SandboxReadPolicy, error) {
	var empty config.Config
	var sandbox SandboxReadPolicy
	if ctx == nil || store == nil {
		return empty, sandbox, errors.New("frozen policy requires context and committed store")
	}
	if err := runID.Validate(); err != nil {
		return empty, sandbox, err
	}
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return empty, sandbox, err
	}
	_, raw, err := store.RunViewDocuments(ctx, runID)
	if err != nil {
		return empty, sandbox, err
	}
	if domain.SumBytes(raw) != run.ConfigDigest {
		return empty, sandbox, errors.New("frozen configuration differs from run binding")
	}
	cfg, err := config.DecodeEffective(raw)
	if err != nil {
		return empty, sandbox, err
	}
	if cfg.Workflow == nil || cfg.Workflow.Revision != run.WorkflowRevision || run.WorkflowRevision != workflow.GenerationRevision || run.WorkflowDigest != domain.SumBytes([]byte(run.WorkflowRevision)) || cfg.Sandbox == nil {
		return empty, sandbox, errors.New("package read policy requires the original MVP configuration")
	}
	lock, err := LoadConfiguredToolchainLock(cfg)
	if err != nil {
		return empty, sandbox, err
	}
	stage, err := store.ReadCommittedSandboxStage(ctx, runID, "solution_verify")
	if err != nil {
		return empty, sandbox, err
	}
	if stage.Attempt.Validate() != nil || stage.Attempt.RunID != runID || stage.Attempt.StageName != "solution_verify" || stage.Attempt.State != domain.StageAttemptSucceeded {
		return empty, sandbox, errors.New("frozen engine policy has no committed Solution verification")
	}
	var engine domain.Digest
	for _, item := range stage.Artifacts {
		path := string(item.Blob.LogicalPath)
		if !strings.HasPrefix(path, "sandbox/") || !strings.HasSuffix(path, "/result.json") {
			continue
		}
		id := domain.SandboxExecutionID(strings.TrimSuffix(strings.TrimPrefix(path, "sandbox/"), "/result.json"))
		if err := id.Validate(); err != nil {
			return empty, sandbox, err
		}
		execution, err := store.GetSandboxExecution(ctx, id)
		if err != nil {
			return empty, sandbox, err
		}
		if execution.ID != id || execution.RunID != runID || execution.StageName != stage.Attempt.StageName || execution.AttemptID != stage.Attempt.AttemptID || execution.State != domain.SandboxExecutionCleaned || execution.EngineIdentityDigest.Validate() != nil {
			return empty, sandbox, errors.New("frozen engine policy differs from committed execution")
		}
		if engine != "" && engine != execution.EngineIdentityDigest {
			return empty, sandbox, errors.New("committed verification spans different engines")
		}
		engine = execution.EngineIdentityDigest
	}
	if engine == "" {
		return empty, sandbox, errors.New("committed verification lacks engine evidence")
	}
	sandbox.Lock, sandbox.EngineIdentity = lock, engine
	sandbox.Config = docker.Config{EngineEndpoint: cfg.Sandbox.EngineEndpoint, APIVersion: docker.RequiredAPIVersion, BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID), ExecutionProtocol: docker.ExecutionProtocolDockerDirectV2}
	sandbox.Limits = docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: cfg.Runtime.CleanupWait}
	return cfg, sandbox, nil
}
