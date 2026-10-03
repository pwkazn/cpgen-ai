package application

import (
	"context"
	"errors"
	"time"

	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

// TryCancelUnstarted completes a pending cancellation without constructing
// providers or Docker only when storage proves the run has never executed.
// A false handled result leaves cancellation to the ordinary recovery path.
func TryCancelUnstarted(ctx context.Context, cfg config.Config, runtime port.RuntimeStore, locks *runlock.Manager, runID domain.RunID) (domain.RunSnapshot, bool, error) {
	if ctx == nil || runtime == nil || locks == nil {
		return domain.RunSnapshot{}, false, errors.New("unstarted cancellation requires context, runtime, and locks")
	}
	if err := ctx.Err(); err != nil {
		return domain.RunSnapshot{}, false, err
	}
	if err := runID.Validate(); err != nil {
		return domain.RunSnapshot{}, false, err
	}
	store, ok := runtime.(port.UnstartedCancellationStore)
	reader, readable := runtime.(frozenConfigReader)
	if !ok || !readable {
		return domain.RunSnapshot{}, false, nil
	}
	guard, err := locks.TryAcquireRun(runID, runlock.Exclusive)
	if errors.Is(err, runlock.ErrBusy) {
		return domain.RunSnapshot{}, false, nil
	}
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	defer guard.Close()
	artifactGuard, err := locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	defer artifactGuard.Close()

	frozen, err := ConfigForRun(ctx, cfg, runID, reader)
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	revision := workflow.FakeRevision
	if frozen.Workflow != nil {
		revision = frozen.Workflow.Revision
	}
	graph, err := newCompiledRunGraph(revision)
	if err != nil {
		return domain.RunSnapshot{}, false, err
	}
	snapshot, err := runtime.GetRun(ctx, runID)
	if err != nil {
		return snapshot, false, err
	}
	if err := validateRunPersistence(ctx, runtime, graph, frozen.EffectiveDigest(), snapshot); err != nil {
		return snapshot, false, err
	}
	if snapshot.State == domain.RunCancelled {
		return snapshot, true, nil
	}
	if snapshot.State != domain.RunCreated {
		return snapshot, false, nil
	}
	pending, err := runtime.PendingCancel(ctx, runID)
	if err != nil || pending == nil {
		return snapshot, false, err
	}
	return store.TryFinalizeUnstartedCancel(ctx, domain.CancelUnstartedCommand{
		RunID: runID, ExpectedRunVersion: snapshot.Version, ControlRequestID: pending.ID,
		IdempotencyKey: stableServiceID("cancel-unstarted", runID, snapshot.Version), At: time.Now().UTC(),
	})
}
