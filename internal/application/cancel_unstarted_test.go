package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

func newUnstartedCancellation(t *testing.T, cfg config.Config, alter func(*domain.CreateRunRequest)) (*application.Application, domain.RunSnapshot) {
	t.Helper()
	ctx := context.Background()
	app, err := application.BootstrapLocal(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	rawConfig, err := cfg.Effective()
	if err != nil {
		t.Fatal(err)
	}
	request := domain.RunRequest{SchemaVersion: domain.RequestSchemaV1, Mode: domain.RequestModeManual, Brief: "cancel before execution", Language: "en", Difficulty: "hard", TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: domain.BudgetLimits{MaxActiveTimeMilliseconds: 60000}}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var object any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	revision := workflow.FakeRevision
	if cfg.Workflow != nil {
		revision = cfg.Workflow.Revision
	}
	definition, err := workflow.DefinitionFor(revision)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	create := domain.CreateRunRequest{
		RunID: "run_000000000000000000000000000001c1", SubmittedRequestJSON: raw, SubmittedRequestDigest: domain.SumBytes(raw),
		RedactedEffectiveConfigJSON: rawConfig, RedactedEffectiveConfigDigest: domain.SumBytes(rawConfig),
		WorkflowRevision: revision, WorkflowDigest: domain.SumBytes([]byte(revision)), SchemaVersion: domain.RequestSchemaV1,
		BudgetLimits: request.BudgetLimits, StageSequence: definition.Stages(), CreatedAt: now, IdempotencyKey: "create_000000000000000000000000000001c1",
	}
	if alter != nil {
		alter(&create)
	}
	run, err := app.Runtime.CreateRun(ctx, create)
	if err != nil {
		t.Fatal(err)
	}
	return app, run
}

func pendingUnstartedCancellation(t *testing.T, app *application.Application, run domain.RunSnapshot) domain.RunSnapshot {
	t.Helper()
	if _, err := app.Runtime.RequestCancel(context.Background(), domain.CancelRequest{
		ID: "control_000000000000000000000000000001c1", RunID: run.RunID, ExpectedRunVersion: run.Version,
		Reason: "cancel before bootstrap", IdempotencyKey: "cancel_000000000000000000000000000001c1", At: run.CreatedAt.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}
	updated, err := app.Runtime.GetRun(context.Background(), run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func TestTryCancelUnstartedUsesFrozenConfigAndLocalComposition(t *testing.T) {
	ctx := context.Background()
	cfg := explicitSlice2ApplicationConfig(t)
	app, run := newUnstartedCancellation(t, cfg, nil)
	if app.Runs != nil {
		t.Fatal("fixture unexpectedly constructed execution clients")
	}
	run = pendingUnstartedCancellation(t, app, run)
	// A later workflow setting must not replace this run's frozen settings.
	cfg.Workflow.IdeaCount++
	observed := &cancellationLockObserver{Store: app.Runtime.(*sqlite.Store), locks: app.Locks, t: t}
	got, handled, err := application.TryCancelUnstarted(ctx, cfg, observed, app.Locks, run.RunID)
	if err != nil || !handled || got.State != domain.RunCancelled || got.ConfigDigest != run.ConfigDigest || !observed.called {
		t.Fatalf("cancelled=%+v handled=%v guarded=%v err=%v", got, handled, observed.called, err)
	}
	again, handled, err := application.TryCancelUnstarted(ctx, cfg, observed, app.Locks, run.RunID)
	if err != nil || !handled || !reflect.DeepEqual(got, again) {
		t.Fatalf("replayed=%+v handled=%v err=%v", again, handled, err)
	}
}

type cancellationLockObserver struct {
	*sqlite.Store
	locks  *runlock.Manager
	t      *testing.T
	called bool
}

func (s *cancellationLockObserver) TryFinalizeUnstartedCancel(ctx context.Context, command domain.CancelUnstartedCommand) (domain.RunSnapshot, bool, error) {
	s.called = true
	for _, acquire := range []func() (*runlock.Guard, error){
		func() (*runlock.Guard, error) { return s.locks.TryAcquireRun(command.RunID, runlock.Exclusive) },
		func() (*runlock.Guard, error) { return s.locks.TryAcquireArtifacts(runlock.Exclusive) },
	} {
		guard, err := acquire()
		if guard != nil {
			_ = guard.Close()
		}
		if !errors.Is(err, runlock.ErrBusy) {
			s.t.Fatalf("cancellation mutation is missing its lock: %v", err)
		}
	}
	return s.Store.TryFinalizeUnstartedCancel(ctx, command)
}

func TestTryCancelUnstartedRejectsIncompatiblePersistenceBeforeMutation(t *testing.T) {
	for name, alter := range map[string]func(*domain.CreateRunRequest){
		"revision": func(c *domain.CreateRunRequest) {
			c.WorkflowRevision = "uncompiled.v9"
			c.WorkflowDigest = domain.SumBytes([]byte(c.WorkflowRevision))
		},
		"workflow digest": func(c *domain.CreateRunRequest) { c.WorkflowDigest = domain.SumBytes([]byte("different workflow")) },
		"stage sequence": func(c *domain.CreateRunRequest) {
			c.StageSequence[1], c.StageSequence[2] = c.StageSequence[2], c.StageSequence[1]
		},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := explicitSlice2ApplicationConfig(t)
			app, run := newUnstartedCancellation(t, cfg, alter)
			before := pendingUnstartedCancellation(t, app, run)
			if _, handled, err := application.TryCancelUnstarted(context.Background(), cfg, app.Runtime, app.Locks, run.RunID); err == nil || handled {
				t.Fatalf("incompatible persistence was accepted: handled=%v err=%v", handled, err)
			}
			after, err := app.Runtime.GetRun(context.Background(), run.RunID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("incompatible persistence was mutated: %+v err=%v", after, err)
			}
		})
	}
}

type cancellationConfigMismatch struct{ *sqlite.Store }

func (s cancellationConfigMismatch) GetRun(ctx context.Context, id domain.RunID) (domain.RunSnapshot, error) {
	run, err := s.Store.GetRun(ctx, id)
	run.ConfigDigest = domain.SumBytes([]byte("changed frozen config"))
	return run, err
}

func TestTryCancelUnstartedRequiresPendingControlAndRunLock(t *testing.T) {
	ctx := context.Background()
	cfg := explicitSlice2ApplicationConfig(t)
	app, run := newUnstartedCancellation(t, cfg, nil)
	if got, handled, err := application.TryCancelUnstarted(ctx, cfg, app.Runtime, app.Locks, run.RunID); err != nil || handled || got.State != domain.RunCreated {
		t.Fatalf("cancelled without pending control: %+v handled=%v err=%v", got, handled, err)
	}
	before := pendingUnstartedCancellation(t, app, run)
	guard, err := app.Locks.TryAcquireRun(run.RunID, runlock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	if _, handled, err := application.TryCancelUnstarted(ctx, cfg, app.Runtime, app.Locks, run.RunID); err != nil || handled {
		t.Fatalf("cancelled an owned run: handled=%v err=%v", handled, err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	corrupt := cancellationConfigMismatch{app.Runtime.(*sqlite.Store)}
	if _, handled, err := application.TryCancelUnstarted(ctx, cfg, corrupt, app.Locks, run.RunID); err == nil || handled {
		t.Fatalf("accepted mismatched frozen config: handled=%v err=%v", handled, err)
	}
	after, err := app.Runtime.GetRun(ctx, run.RunID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected cancellation changed run: %+v err=%v", after, err)
	}
}
