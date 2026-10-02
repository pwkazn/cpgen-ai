package port

import (
	"context"
	"time"

	"cpgen/internal/domain"
)

// ContentRetryStore closes a failed attempt and schedules its bounded source
// regeneration atomically. It is required only by the V2 generation workflow.
type ContentRetryStore interface {
	FinishContentRetry(context.Context, domain.FinishContentRetryCommand) (domain.RunSnapshot, error)
}

// UnsentSandboxRecoveryStore interrupts only a verification attempt whose
// retained ledger proves no Docker dispatch and no execution record. Otherwise
// it returns the unchanged run. The proof and interruption must be atomic.
type UnsentSandboxRecoveryStore interface {
	InterruptUnsentSandboxStage(context.Context, domain.InterruptStageCommand) (domain.RunSnapshot, error)
}

type RuntimeStore interface {
	CreateRun(context.Context, domain.CreateRunRequest) (domain.RunSnapshot, error)
	GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
	StageSequence(context.Context, domain.RunID) ([]domain.StageName, error)
	ListRuns(context.Context, domain.RunFilter) ([]domain.RunSummary, error)
	Events(context.Context, domain.RunID, int64) ([]domain.RunEvent, error)
	BeginStage(context.Context, domain.BeginStageCommand) (domain.StageAttempt, error)
	FinishStage(context.Context, domain.FinishStageCommand) (domain.RunSnapshot, error)
	InterruptStage(context.Context, domain.InterruptStageCommand) (domain.RunSnapshot, error)
	RequestCancel(context.Context, domain.CancelRequest) (domain.ControlRequest, error)
	PendingCancel(context.Context, domain.RunID) (*domain.ControlRequest, error)
	FinalizeCancel(context.Context, domain.FinalizeCancelCommand) (domain.RunSnapshot, error)
	AccountActiveTime(context.Context, domain.ActiveTimeCommand) (domain.ActiveTimeResult, error)
}

type ReviewStore interface {
	CreateReview(context.Context, domain.CreateReviewRequest) (domain.ReviewDecision, error)
	PendingReview(context.Context, domain.RunID) (*domain.ReviewDecision, error)
	ApplyReview(context.Context, domain.ApplyReviewCommand) (domain.RunSnapshot, error)
}

// ReviewBindingReader returns the durable stage input, failure evidence and
// policy digests that a new review decision must bind exactly.
type ReviewBindingReader interface {
	ReadReviewBinding(context.Context, domain.RunID, domain.StageName) (domain.Digest, domain.Digest, domain.Digest, error)
}

// DraftRetryFeedbackReader returns the latest retry/revision diagnostic that
// was effective when an attempt started. The time bound lets readers
// reconstruct committed requests after later reviews have added feedback.
type DraftRetryFeedbackReader interface {
	ReadDraftRetryFeedbackBefore(context.Context, domain.RunID, domain.StageName, time.Time) (*domain.DraftRetryFeedback, error)
}
