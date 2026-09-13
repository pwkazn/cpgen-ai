package workflow_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestWithStageTimeoutUsesInjectedClockAndTypedCause(t *testing.T) {
	t.Parallel()
	fakeClock := clock.NewFake(time.Date(2026, 8, 30, 0, 0, 0, 0, time.UTC))
	ctx, cancel := workflow.WithStageTimeout(context.Background(), fakeClock, time.Minute)
	defer cancel()

	fakeClock.Advance(59 * time.Second)
	select {
	case <-ctx.Done():
		t.Fatal("stage timed out too early")
	default:
	}

	fakeClock.Advance(time.Second)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("stage timeout was not delivered")
	}
	var interrupted domain.ExecutionInterrupted
	if !errors.As(context.Cause(ctx), &interrupted) {
		t.Fatalf("cause = %v, want ExecutionInterrupted", context.Cause(ctx))
	}
	if interrupted.Cause != domain.CauseStepDeadline {
		t.Fatalf("cause = %q, want step_deadline", interrupted.Cause)
	}
}
