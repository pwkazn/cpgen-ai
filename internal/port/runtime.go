package port

import (
	"context"

	"cpgen/internal/domain"
)

// ContentRetryStore closes a failed attempt and schedules its bounded source
// regeneration atomically. It is required only by the V2 generation workflow.
type ContentRetryStore interface {
	FinishContentRetry(context.Context, domain.FinishContentRetryCommand) (domain.RunSnapshot, error)
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
