package application

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/clock"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

// Application is the local foreground workflow composition. There is no
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

// StorageResources is retained as a compatibility type for callers that used
// the old test composition hook. Bootstrap no longer consumes a process
// global factory: the local application always composes its own SQLite and
// blob stores from the validated effective paths below.
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

// RegisterStorageFactory is kept source-compatible for older tests and
// embedders. It is deliberately ignored by Bootstrap; relying on mutable
// process-global registration would make the required application contract
// depend on import order and CLI initialization.
func RegisterStorageFactory(StorageFactory) {}

// Bootstrap creates the private local application from a validated config.
// The default remains the Fake pipeline. An explicit compiled workflow selector
// composes durable generation and, for the Solution revision, a pinned local
// Docker engine with detached cleanup. Each revision keeps its own boundary.
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
	effectiveJSON, err := cfg.Effective()
	if err != nil {
		return nil, err
	}
	paths := effective.Paths
	for _, directory := range []string{paths.StateRoot, paths.Runtime, paths.Locks, paths.Work} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create private state directory %q: %w", directory, err)
		}
	}
	store, err := sqlite.Open(ctx, sqlite.Config{Path: paths.Database, BusyTimeout: cfg.SQLite.BusyTimeout, MaxReaders: cfg.SQLite.MaxReaders})
	if err != nil {
		return nil, fmt.Errorf("bootstrap sqlite: %w", err)
	}
	blobs, err := blob.NewStore(paths.Artifacts)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("bootstrap artifacts: %w", err)
	}
	if err := blobs.AttachCorruptionLedger(store); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("bootstrap artifact ledger: %w", err)
	}
	resources := StorageResources{
		Runtime: store, Reviews: store, Metadata: store, Ledger: store,
		BlobStore: blobs, SandboxRuntime: store, Close: store.Close,
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
	reconciler := &localSandboxReconciler{runtime: resources.SandboxRuntime}
	var runs *LocalRunService
	if cfg.Workflow != nil {
		runs, err = bootstrapSlice2RunService(ctx, cfg, store, blobs, locks, reconciler, effectiveJSON)
	} else {
		pipeline, pipelineErr := workflow.NewSlice1Pipeline(
			fake.NewPrepareStep(workflow.PrepareCapabilities{}),
			fake.NewExerciseStep(workflow.ExerciseCapabilities{}),
			fake.NewCheckpointStep(workflow.CheckpointCapabilities{}),
		)
		if pipelineErr != nil {
			return nil, fmt.Errorf("bootstrap fake pipeline: %w", pipelineErr)
		}
		runs, err = NewRunService(RunServiceConfig{
			Runtime: resources.Runtime, Reviews: resources.Reviews, Locks: locks, Pipeline: pipeline,
			Clock: clock.Real{}, ActiveTimeInterval: cfg.Runtime.AccountingHeartbeat,
			Reconciler: reconciler, EffectiveConfigJSON: effectiveJSON,
			EffectiveConfigDigest: cfg.EffectiveDigest(), Scenario: cfg.FakeWorkflow.Scenario,
		})
	}
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
