package port

import (
	"context"

	"cpgen/internal/domain"
)

// SandboxLifecycleRecorder is the small durable boundary around Docker
// resources. Implementations must keep each method a short named transaction;
// no callback or arbitrary state setter is exposed here.
type SandboxLifecycleRecorder interface {
	PrepareExecution(context.Context, domain.PrepareExecutionRequest) (domain.SandboxExecution, error)
	RecordWatchdogArmed(context.Context, domain.WatchdogArmed) error
	BeginResourceCreate(context.Context, domain.BeginResourceCreate) (domain.PreCreateRequest, error)
	RecordPreCreateACK(context.Context, domain.PreCreateACK) error
	AdvanceResource(context.Context, domain.AdvanceResourceRequest) (domain.SandboxResource, error)
	MarkCleanupPending(context.Context, domain.MarkCleanupPendingCommand) (domain.SandboxExecution, error)
	FinishCleanup(context.Context, domain.FinishCleanupCommand) (domain.SandboxExecution, error)
}
