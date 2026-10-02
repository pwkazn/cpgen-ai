package application

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	artifact "cpgen/internal/artifact"
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
	Packages    PackageArchiveReader
	WebReads    *WorkbenchReader
	Runtime     port.RuntimeStore
	Reviews     port.ReviewStore
	Maintenance *artifact.Maintenance
	Locks       *runlock.Manager

	closeOnce      sync.Once
	closeErr       error
	closeStorage   func() error
	closeExecution func() error
}

// Bootstrap creates the private local application from a validated config.
// The default remains the Fake pipeline. An explicit compiled workflow selector
// composes durable generation and, for the Solution revision, a pinned local
// Docker engine with detached cleanup. Each revision keeps its own boundary.
func Bootstrap(ctx context.Context, cfg config.Config) (*Application, error) {
	return bootstrap(ctx, cfg, true, nil)
}

// BootstrapWithSimilarityHTTPClient composes a live workflow with a trusted
// caller-supplied similarity transport. It is used by local development
// fixtures; ordinary CLI and web bootstraps retain the policy-controlled
// provider transport.
func BootstrapWithSimilarityHTTPClient(ctx context.Context, cfg config.Config, client *http.Client) (*Application, error) {
	if client == nil {
		return nil, errors.New("similarity HTTP client is required")
	}
	return bootstrap(ctx, cfg, true, client)
}

// BootstrapLocal opens local persistence and verified package readers without
// constructing stage executors, provider clients, or the Docker runtime.
func BootstrapLocal(ctx context.Context, cfg config.Config) (*Application, error) {
	return bootstrap(ctx, cfg, false, nil)
}

func bootstrap(ctx context.Context, cfg config.Config, execution bool, similarityHTTPClient *http.Client) (*Application, error) {
	if ctx == nil {
		return nil, errors.New("bootstrap context is nil")
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if execution && cfg.Workflow != nil && workflow.HasSolutionStages(cfg.Workflow.Revision) {
		if err := bindToolchainLockSnapshot(&cfg); err != nil {
			return nil, err
		}
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
	closeStore := true
	defer func() {
		if closeStore {
			_ = store.Close()
		}
	}()
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
	maintenance, err := artifact.NewMaintenance(locks, store, blobs)
	if err != nil {
		return nil, fmt.Errorf("bootstrap artifact maintenance: %w", err)
	}
	reader, err := NewCommittedPackageReader(store, blobs)
	if err != nil {
		return nil, fmt.Errorf("bootstrap package reader: %w", err)
	}
	webReads, err := NewWorkbenchReader(store, blobs)
	if err != nil {
		return nil, fmt.Errorf("bootstrap workbench reader: %w", err)
	}
	app := &Application{Runtime: store, Reviews: store, Maintenance: maintenance, Locks: locks, closeStorage: store.Close,
		Packages: &lockedPackageReader{locks: locks, reader: reader}, WebReads: webReads}
	if !execution {
		closeStore, closeLocks = false, false
		return app, nil
	}
	reconciler := &localSandboxReconciler{runtime: store}
	var runs *LocalRunService
	if cfg.Workflow != nil {
		runs, app.closeExecution, err = bootstrapGenerationRunService(ctx, cfg, store, blobs, locks, reconciler, effectiveJSON, similarityHTTPClient)
	} else {
		pipeline, pipelineErr := fake.NewPipeline(
			fake.NewPrepareStep(fake.PrepareCapabilities{}),
			fake.NewExerciseStep(fake.ExerciseCapabilities{}),
			fake.NewCheckpointStep(fake.CheckpointCapabilities{}),
		)
		if pipelineErr != nil {
			return nil, fmt.Errorf("bootstrap fake pipeline: %w", pipelineErr)
		}
		runs, err = NewRunService(RunServiceConfig{
			Runtime: store, Reviews: store, Locks: locks, Pipeline: pipeline,
			Clock: clock.Real{}, ActiveTimeInterval: cfg.Runtime.AccountingHeartbeat, ControlPollInterval: cfg.Runtime.ControlPollInterval,
			Reconciler: reconciler, EffectiveConfigJSON: effectiveJSON,
			EffectiveConfigDigest: cfg.EffectiveDigest(), Scenario: cfg.FakeWorkflow.Scenario,
		})
	}
	if err != nil {
		return nil, fmt.Errorf("bootstrap run service: %w", err)
	}
	app.Runs = runs
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
		if a.closeExecution != nil {
			errs = append(errs, a.closeExecution())
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
