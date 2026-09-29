package port

import (
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestSandboxLifecycleCommandsRequirePositiveVersion(t *testing.T) {
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
}
