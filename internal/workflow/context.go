package workflow

import (
	"context"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

// WithStageTimeout uses the injected clock and records a stage deadline cause,
// keeping it distinct from a sandbox program hard deadline (TLE).
func WithStageTimeout(parent context.Context, source clock.Clock, duration time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancelCause := context.WithCancelCause(parent)
	timeout := source.After(duration)
	go func() {
		select {
		case <-timeout:
			cancelCause(domain.ExecutionInterrupted{Cause: domain.CauseStepDeadline})
		case <-ctx.Done():
		}
	}()
	return ctx, func() { cancelCause(context.Canceled) }
}
