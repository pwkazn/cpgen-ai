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

func TestRunServiceRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := application.NewRunService(application.RunServiceConfig{}); err == nil {
		t.Fatal("incomplete run service configuration was accepted")
	}
}

func TestRunServiceInterfaceHasFixedLifecycle(t *testing.T) {
	var _ application.RunService = (*application.LocalRunService)(nil)
	_ = context.Background()
	_ = workflow.Slice1WorkflowRevision
}

func TestRunServiceGenerateStopsAtSlice1Review(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "locks"), 0700); err != nil {
		t.Fatal(err)
	}
	source := clock.NewFake(now)
	store, err := sqlite.OpenWithClock(context.Background(), sqlite.Config{Path: filepath.Join(root, "run.db"), BusyTimeout: time.Second, MaxReaders: 4}, source)
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
		fake.NewPrepareStep(workflow.PrepareCapabilities{}), fake.NewExerciseStep(workflow.ExerciseCapabilities{}), fake.NewCheckpointStep(workflow.CheckpointCapabilities{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewRunService(application.RunServiceConfig{Runtime: store, Locks: locks, Clock: source, Pipeline: pipeline, ActiveTimeInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := service.Generate(context.Background(), domain.RunRequest{SchemaVersion: "cpgen.request/v1", Mode: "offline", Brief: "demo", Language: "cpp", Difficulty: "easy", TimeLimitMilliseconds: 1000, MemoryLimitMegabytes: 64, SolutionLanguage: "go", VerificationProfile: "default", BudgetLimits: domain.BudgetLimits{MaxActiveTimeMilliseconds: 5000}})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != domain.RunNeedsReview || snapshot.CurrentStage != "checkpoint" {
		t.Fatalf("snapshot = %+v", snapshot)
	}
	if snapshot.State == domain.RunReady {
		t.Fatal("slice1 produced READY")
	}
}

func TestRunServiceResumeBlockedCreatesFreshAttempt(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "locks"), 0700); err != nil {
		t.Fatal(err)
	}
	source := clock.NewFake(now)
	store, err := sqlite.OpenWithClock(context.Background(), sqlite.Config{Path: filepath.Join(root, "run.db"), BusyTimeout: time.Second, MaxReaders: 4}, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	locks, err := runlock.NewManager(filepath.Join(root, "locks"), runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locks.Close() })
	pipeline, err := workflow.NewSlice1Pipeline(fake.NewPrepareStep(workflow.PrepareCapabilities{}), fake.NewExerciseStep(workflow.ExerciseCapabilities{}), fake.NewCheckpointStep(workflow.CheckpointCapabilities{}))
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewRunService(application.RunServiceConfig{Runtime: store, Locks: locks, Clock: source, Pipeline: pipeline, ActiveTimeInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Generate(context.Background(), domain.RunRequest{SchemaVersion: "cpgen.request/v1", Mode: "blocked", Brief: "demo", Language: "cpp", Difficulty: "easy", TimeLimitMilliseconds: 1000, MemoryLimitMegabytes: 64, SolutionLanguage: "go", VerificationProfile: "default", BudgetLimits: domain.BudgetLimits{MaxActiveTimeMilliseconds: 5000}})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != domain.RunBlocked {
		t.Fatalf("first snapshot = %+v", first)
	}
	resumed, err := service.Resume(context.Background(), first.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != domain.RunNeedsReview || resumed.CurrentStage != "checkpoint" {
		t.Fatalf("resumed snapshot = %+v", resumed)
	}
}
