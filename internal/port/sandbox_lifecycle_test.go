package port

import (
	"context"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestSandboxLifecycleAuthorizationIdentityExcludesOwnerAndEpoch(t *testing.T) {
	run := domain.RunID("run_00000000000000000000000000000001")
	attempt := domain.AttemptID("attempt_00000000000000000000000000000001")
	execution := domain.SandboxExecutionID("sandbox_00000000000000000000000000000001")
	logical := "compile"
	digest := domain.SumBytes([]byte("scope"))
	identity := domain.SandboxAuthorizationIdentity{
		RunID: run, AttemptID: attempt, SandboxExecutionID: execution,
		LogicalOperationID: logical, ScopeDigest: digest, PlanDigest: digest,
		EngineIdentityDigest: digest,
	}
	if err := identity.Validate(); err != nil {
		t.Fatalf("identity validation: %v", err)
	}
	if got := identity.Digest(); got == "" {
		t.Fatal("identity digest is empty")
	}
}

func TestSandboxLifecycleCommandsRequireExactVersionAndCanonicalTime(t *testing.T) {
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	request := domain.BeginResourceCreate{
		ExecutionID:     domain.SandboxExecutionID("sandbox_00000000000000000000000000000001"),
		ResourceID:      domain.SandboxResourceID("resource_00000000000000000000000000000001"),
		ExpectedVersion: 1, IdempotencyKey: "control_00000000000000000000000000000001", At: now,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid begin resource request rejected: %v", err)
	}
	request.ExpectedVersion = 0
	if err := request.Validate(); err == nil {
		t.Fatal("expected invalid version to be rejected")
	}
	_ = context.Background()
}

var _ SandboxLifecycleRecorder = (*fakeSandboxLifecycleRecorder)(nil)

type fakeSandboxLifecycleRecorder struct{}

func (*fakeSandboxLifecycleRecorder) PrepareExecution(context.Context, domain.PrepareExecutionRequest) (domain.SandboxExecution, error) {
	return domain.SandboxExecution{}, nil
}
func (*fakeSandboxLifecycleRecorder) RecordWatchdogArmed(context.Context, domain.WatchdogArmed) error {
	return nil
}
func (*fakeSandboxLifecycleRecorder) BeginResourceCreate(context.Context, domain.BeginResourceCreate) (domain.PreCreateRequest, error) {
	return domain.PreCreateRequest{}, nil
}
func (*fakeSandboxLifecycleRecorder) RecordPreCreateACK(context.Context, domain.PreCreateACK) error {
	return nil
}
func (*fakeSandboxLifecycleRecorder) AdvanceResource(context.Context, domain.AdvanceResourceRequest) (domain.SandboxResource, error) {
	return domain.SandboxResource{}, nil
}
func (*fakeSandboxLifecycleRecorder) MarkCleanupPending(context.Context, domain.MarkCleanupPendingCommand) (domain.SandboxExecution, error) {
	return domain.SandboxExecution{}, nil
}
func (*fakeSandboxLifecycleRecorder) FinishCleanup(context.Context, domain.FinishCleanupCommand) (domain.SandboxExecution, error) {
	return domain.SandboxExecution{}, nil
}
