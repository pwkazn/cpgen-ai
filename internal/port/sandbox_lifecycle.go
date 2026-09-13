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

// SandboxLifecycleReader is implemented by durable stores that can resume a
// previously prepared execution. Keeping this read capability separate lets
// Slice 0 fakes continue to compile while the production Runner requires it.
type SandboxLifecycleReader interface {
	GetSandboxExecution(context.Context, domain.SandboxExecutionID) (domain.SandboxExecution, error)
}

// SandboxWatchdogReader exposes immutable fsynced control evidence for replay
// checks. A durable runner must compare this row before reusing an execution
// identity after a crash.
type SandboxWatchdogReader interface {
	GetSandboxWatchdogControl(context.Context, domain.SandboxExecutionID) (domain.SandboxWatchdogControl, error)
}

// SandboxCleanupRecorder contains the named proof commands. These methods are
// deliberately not part of AdvanceResource: cleanup state must be backed by
// persisted stop/kill/wait and removal evidence.
type SandboxCleanupRecorder interface {
	RecordResourceStopProof(context.Context, domain.RecordResourceStopProofCommand) (domain.SandboxResource, error)
	RecordResourceCleaned(context.Context, domain.RecordResourceCleanedCommand) (domain.SandboxResource, error)
	RecordResourceInterrupted(context.Context, domain.RecordResourceInterruptedCommand) (domain.SandboxResource, error)
}
