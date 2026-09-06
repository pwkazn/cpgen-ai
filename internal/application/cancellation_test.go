package application_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

func TestRunServiceOwnerCancelFinalizesWithValidDeterministicKeys(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "locks"), 0o700); err != nil {
		t.Fatal(err)
	}
	clockSource := clock.NewFake(now)
	store, err := sqlite.OpenWithClock(ctx, sqlite.Config{Path: filepath.Join(root, "run.db"), BusyTimeout: time.Second, MaxReaders: 4}, clockSource)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	locks, err := runlock.NewManager(filepath.Join(root, "locks"), runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locks.Close() })
	pipeline, err := workflow.NewSlice1Pipeline(
		fake.NewPrepareStep(workflow.PrepareCapabilities{}),
		fake.NewExerciseStep(workflow.ExerciseCapabilities{}),
		fake.NewCheckpointStep(workflow.CheckpointCapabilities{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewRunService(application.RunServiceConfig{
		Runtime: store, Locks: locks, Clock: clockSource, Pipeline: pipeline,
		ActiveTimeInterval: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := domain.RunRequest{
		SchemaVersion: "cpgen.request/v1", Mode: "offline", Brief: "owner cancel",
		Language: "cpp", Difficulty: "easy", TimeLimitMilliseconds: 1000,
		MemoryLimitMegabytes: 64, SolutionLanguage: "go", VerificationProfile: "default",
		BudgetLimits: domain.BudgetLimits{MaxActiveTimeMilliseconds: 5000},
	}
	snapshot, err := service.Generate(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != domain.RunNeedsReview {
		t.Fatalf("generated snapshot = %+v, want NEEDS_REVIEW", snapshot)
	}
	cancelID := domain.ControlRequestID("control_00000000000000000000000000000001")
	cancelled, err := service.Cancel(ctx, domain.CancelRequest{
		ID: cancelID, RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version,
		Reason: "owner-side regression", IdempotencyKey: string(cancelID), At: now,
	})
	if err != nil {
		t.Fatalf("owner-side cancel: %v", err)
	}
	if cancelled.State != domain.RunCancelled {
		t.Fatalf("cancelled snapshot = %+v, want CANCELLED", cancelled)
	}
	events, err := store.Events(ctx, snapshot.RunID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 || events[len(events)-1].Type != domain.EventCancelFinalized {
		t.Fatalf("terminal cancellation event = %+v", events)
	}
	for _, event := range events {
		if err := (domain.ControlRequestID(event.IdempotencyKey)).Validate(); err != nil {
			t.Fatalf("event %d has invalid deterministic idempotency key %q: %v", event.Version, event.IdempotencyKey, err)
		}
	}
}
