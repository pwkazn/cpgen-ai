package application

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/clock"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

// Application is the complete local Slice 1 composition. There is no
// server, worker pool, or background workflow daemon hidden behind it.
type Application struct {
	Runs        RunService
	Runtime     port.RuntimeStore
	Reviews     port.ReviewStore
	Maintenance *ArtifactMaintenance
	Locks       *runlock.Manager

	closeOnce    sync.Once
	closeErr     error
	closeStorage func() error
}

// StorageResources is the narrow adapter bundle needed by the local
// composition. Keeping its factory outside this package avoids a dependency
// cycle with storage's in-package tests while allowing the CLI (the process
// composition root) to select SQLite explicitly.
type StorageResources struct {
	Runtime        port.RuntimeStore
	Reviews        port.ReviewStore
	Metadata       port.GCMetadataStore
	Ledger         port.ArtifactLedger
	BlobStore      *blob.Store
	SandboxRuntime interface {
		UnfinishedSandboxExecutions(context.Context, domain.RunID) ([]domain.SandboxExecution, error)
	}
	Close func() error
}

type StorageFactory func(context.Context, config.Config) (StorageResources, error)

var storageFactory StorageFactory

// RegisterStorageFactory installs the process composition's durable storage
// adapter. It is called by the CLI composition root; tests can register a
// fake or SQLite factory without introducing an application/storage cycle.
func RegisterStorageFactory(factory StorageFactory) {
	storageFactory = factory
}

// Bootstrap creates the private local application from a validated config.
// The pipeline is deliberately Fake-only until Task 10's explicit Docker
// harness is selected by the caller.
func Bootstrap(ctx context.Context, cfg config.Config) (*Application, error) {
	if ctx == nil {
		return nil, errors.New("bootstrap context is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	effective, err := cfg.EffectiveConfig()
	if err != nil {
		return nil, err
	}
	paths := effective.Paths
	for _, directory := range []string{paths.StateRoot, paths.Runtime, paths.Locks, paths.Work} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create private state directory %q: %w", directory, err)
		}
	}
	if storageFactory == nil {
		return nil, errors.New("no local storage factory is registered")
	}
	resources, err := storageFactory(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("bootstrap storage: %w", err)
	}
	closeStore := true
	defer func() {
		if closeStore {
			if resources.Close != nil {
				_ = resources.Close()
			}
		}
	}()
	if resources.Runtime == nil || resources.Reviews == nil || resources.Metadata == nil || resources.Ledger == nil || resources.BlobStore == nil || resources.SandboxRuntime == nil {
		return nil, errors.New("bootstrap storage factory returned incomplete resources")
	}
	locks, err := runlock.NewManager(paths.Locks, runlock.Options{PollInterval: cfg.Runtime.LockPollInterval})
	if err != nil {
		return nil, fmt.Errorf("bootstrap run locks: %w", err)
	}
	closeLocks := true
	defer func() {
		if closeLocks {
			_ = locks.Close()
		}
	}()
	maintenance, err := NewArtifactMaintenance(locks, resources.Metadata, resources.BlobStore)
	if err != nil {
		return nil, fmt.Errorf("bootstrap artifact maintenance: %w", err)
	}
	pipeline, err := workflow.NewSlice1Pipeline(
		fake.NewPrepareStep(workflow.PrepareCapabilities{}),
		fake.NewExerciseStep(workflow.ExerciseCapabilities{}),
		fake.NewCheckpointStep(workflow.CheckpointCapabilities{}),
	)
	if err != nil {
		return nil, fmt.Errorf("bootstrap fake pipeline: %w", err)
	}
	reconciler := &localSandboxReconciler{runtime: resources.SandboxRuntime}
	runs, err := NewRunService(RunServiceConfig{
		Runtime: resources.Runtime, Reviews: resources.Reviews, Locks: locks, Pipeline: pipeline,
		Clock: clock.Real{}, ActiveTimeInterval: cfg.Runtime.AccountingHeartbeat,
		Reconciler: reconciler,
	})
	if err != nil {
		return nil, fmt.Errorf("bootstrap run service: %w", err)
	}
	app := &Application{Runs: runs, Runtime: resources.Runtime, Reviews: resources.Reviews, Maintenance: maintenance, Locks: locks, closeStorage: resources.Close}
	closeStore, closeLocks = false, false
	return app, nil
}

// Close is safe to call repeatedly. Foreground RunService calls join their
// own pollers before returning, so closing the application only releases the
// resources built by Bootstrap, in reverse order.
func (a *Application) Close() error {
	if a == nil {
		return nil
	}
	a.closeOnce.Do(func() {
		var errs []error
		if closer, ok := a.Runs.(interface{ Close() error }); ok {
			errs = append(errs, closer.Close())
		}
		if a.Locks != nil {
			errs = append(errs, a.Locks.Close())
		}
		if a.closeStorage != nil {
			errs = append(errs, a.closeStorage())
		}
		a.closeErr = errors.Join(errs...)
	})
	return a.closeErr
}

// localSandboxReconciler is intentionally narrow. It is allowed to report an
// empty result for a clean Fake run, but refuses to claim cleanup for any
// durable real sandbox execution that might have been left by a prior
// process. Docker reconciliation is supplied by Task 10 instead.
type localSandboxReconciler struct {
	runtime interface {
		UnfinishedSandboxExecutions(context.Context, domain.RunID) ([]domain.SandboxExecution, error)
	}
}

func (r *localSandboxReconciler) ReconcileRun(ctx context.Context, runID domain.RunID) (domain.SandboxReconcileReport, error) {
	if err := runID.Validate(); err != nil {
		return domain.SandboxReconcileReport{}, err
	}
	unfinished, err := r.runtime.UnfinishedSandboxExecutions(ctx, runID)
	if err != nil {
		return domain.SandboxReconcileReport{RunID: runID}, fmt.Errorf("inspect unfinished sandbox executions: %w", err)
	}
	if len(unfinished) != 0 {
		return domain.SandboxReconcileReport{RunID: runID, Executions: unfinished, Pending: len(unfinished), Completed: false}, errors.New("real sandbox executions require the explicit Docker reconciler")
	}
	return domain.SandboxReconcileReport{RunID: runID, Completed: true}, nil
}
