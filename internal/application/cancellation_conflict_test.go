package application_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/runlock"
)

func TestRunServiceFinishesCancellationRacingStageWrites(t *testing.T) {
	for _, boundary := range []string{"begin", "start", "stop", "finish", "between_stages", "stop_at_budget_limit"} {
		t.Run(boundary, func(t *testing.T) {
			store, service, reconciler, request := newCancellationConflictFixture(t, boundary)
			if boundary == "stop_at_budget_limit" {
				store.boundary = "stop"
				request.BudgetLimits.MaxActiveTimeMilliseconds = 1000
			}
			ctx := context.Background()
			result, err := service.Generate(ctx, request)
			if err != nil || result.State != domain.RunCancelled {
				t.Fatalf("accepted cancellation was stranded: result=%+v error=%v", result, err)
			}
			if !store.injected || reconciler.calls != 1 || result.ActiveStartedAt != nil {
				t.Fatalf("cancellation skipped cleanup or accounting: injected=%t reconciles=%d result=%+v", store.injected, reconciler.calls, result)
			}
			pending, err := store.PendingCancel(ctx, result.RunID)
			if err != nil || pending != nil {
				t.Fatalf("cancellation remains pending: %+v %v", pending, err)
			}
			wantElapsed := time.Second
			if boundary == "begin" || boundary == "start" {
				wantElapsed = 0
			}
			budget, err := store.BudgetSnapshot(ctx, result.RunID)
			wantRemaining := request.BudgetLimits.MaxActiveTimeMilliseconds*int64(time.Millisecond) - int64(wantElapsed)
			if err != nil || result.ActiveElapsed != wantElapsed || budget.Remaining[domain.BudgetActiveTimeNS] != wantRemaining {
				t.Fatalf("active time was lost or charged twice: elapsed=%v budget=%+v error=%v", result.ActiveElapsed, budget, err)
			}
			attempt, err := store.CurrentStageAttempt(ctx, result.RunID, result.CurrentStage)
			if boundary == "begin" || boundary == "between_stages" {
				if !errors.Is(err, sqlite.ErrNotFound) {
					t.Fatalf("cancellation invented an attempt: %+v %v", attempt, err)
				}
			} else if err != nil || attempt.State != domain.StageAttemptCancelled {
				t.Fatalf("active attempt was not cancelled: %+v %v", attempt, err)
			}
			if boundary == "between_stages" {
				previous, err := store.CurrentStageAttempt(ctx, result.RunID, "prepare")
				if err != nil || previous.State != domain.StageAttemptSucceeded || result.CurrentStage != "exercise" {
					t.Fatalf("cancellation changed the committed predecessor: %+v %v", previous, err)
				}
			}
			replayed, err := service.Resume(ctx, result.RunID)
			if err != nil || !reflect.DeepEqual(result, replayed) || reconciler.calls != 1 {
				t.Fatalf("terminal replay repeated cancellation: %+v %v", replayed, err)
			}
		})
	}
}

func TestRunServiceCancellationConflictPreservesFailures(t *testing.T) {
	accountingErr := errors.New("accounting storage failed")
	cleanupErr := errors.New("sandbox inspection failed")
	for _, tc := range []struct {
		name          string
		cancel        bool
		writeErr      error
		cleanupErr    error
		wantErr       error
		wantReconcile int
	}{
		{name: "unrelated version conflict", writeErr: sqlite.ErrVersionConflict, wantErr: sqlite.ErrVersionConflict},
		{name: "storage error with pending cancellation", cancel: true, writeErr: accountingErr, wantErr: accountingErr},
		{name: "cleanup failure", cancel: true, cleanupErr: cleanupErr, wantErr: cleanupErr, wantReconcile: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, service, reconciler, request := newCancellationConflictFixture(t, "stop")
			store.cancel, store.writeErr, reconciler.err = tc.cancel, tc.writeErr, tc.cleanupErr
			result, err := service.Generate(context.Background(), request)
			if !errors.Is(err, tc.wantErr) || result.State != domain.RunRunning || reconciler.calls != tc.wantReconcile {
				t.Fatalf("failure was hidden or finalized: result=%+v reconciles=%d error=%v", result, reconciler.calls, err)
			}
			pending, err := store.PendingCancel(context.Background(), result.RunID)
			if err != nil || (pending != nil) != tc.cancel {
				t.Fatalf("control request changed on failure: %+v %v", pending, err)
			}
		})
	}
}

func TestRunServiceFinishesCancellationRacingRecoveryWrites(t *testing.T) {
	for _, boundary := range []string{"recover", "interrupt"} {
		t.Run(boundary, func(t *testing.T) {
			store, service, reconciler, request := newCancellationConflictFixture(t, "stop")
			interrupted := errors.New("owner exited before accounting stopped")
			store.cancel, store.writeErr = false, interrupted
			ctx := context.Background()
			initial, err := service.Generate(ctx, request)
			if !errors.Is(err, interrupted) || initial.State != domain.RunRunning || initial.ActiveStartedAt == nil {
				t.Fatalf("interrupted run fixture: %+v %v", initial, err)
			}
			store.boundary, store.injected, store.cancel, store.writeErr = boundary, false, true, nil
			result, err := service.Resume(ctx, initial.RunID)
			if err != nil || result.State != domain.RunCancelled || result.ActiveStartedAt != nil || result.ActiveElapsed != time.Second {
				t.Fatalf("recovery stranded accepted cancellation: %+v %v", result, err)
			}
			pending, err := store.PendingCancel(ctx, result.RunID)
			if err != nil || pending != nil || !store.injected || reconciler.calls != 2 {
				t.Fatalf("recovery skipped cancellation reconciliation: pending=%+v reconciles=%d error=%v", pending, reconciler.calls, err)
			}
			attempt, err := store.CurrentStageAttempt(ctx, result.RunID, result.CurrentStage)
			if err != nil || attempt.State != domain.StageAttemptCancelled || attempt.Ordinal != 1 {
				t.Fatalf("recovery created another attempt: %+v %v", attempt, err)
			}
		})
	}
}

func TestRunServiceFinishesCancellationRacingReviewApplication(t *testing.T) {
	store, service, reconciler, request := newCancellationConflictFixture(t, "review")
	ctx := context.Background()
	initial, err := service.Generate(ctx, request)
	if err != nil || initial.State != domain.RunNeedsReview {
		t.Fatalf("review fixture: %+v %v", initial, err)
	}
	input, evidence, policy, err := store.ReadReviewBinding(ctx, initial.RunID, initial.CurrentStage)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.CreateReview(ctx, domain.CreateReviewRequest{
		ID: "review_0000000000000000000000000000cafe", RunID: initial.RunID, ExpectedRunVersion: initial.Version,
		Kind: domain.ReviewRetry, WorkflowRevision: initial.WorkflowRevision, StageName: initial.CurrentStage,
		StageInputDigest: input, EvidenceDigest: evidence, PolicyDigest: policy,
		BudgetIncrease: domain.BudgetLimits{MaxActiveTimeMilliseconds: 1000}, Reviewer: "fixture", Reason: "extend time",
		IdempotencyKey: "review_0000000000000000000000000000cafe", At: store.source.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Resume(ctx, initial.RunID)
	if err != nil || result.State != domain.RunCancelled || result.ActiveElapsed != initial.ActiveElapsed || reconciler.calls != 1 {
		t.Fatalf("review application stranded accepted cancellation: %+v %v", result, err)
	}
	budget, err := store.BudgetSnapshot(ctx, result.RunID)
	if err != nil || budget.Limits != request.BudgetLimits {
		t.Fatalf("cancelled review applied its budget grant: %+v %v", budget, err)
	}
	pendingReview, err := store.PendingReview(ctx, result.RunID)
	if err != nil || pendingReview != nil {
		t.Fatalf("cancelled review remains pending: %+v %v", pendingReview, err)
	}
	pendingCancel, err := store.PendingCancel(ctx, result.RunID)
	if err != nil || pendingCancel != nil || !store.injected {
		t.Fatalf("review application retained control request: %+v %v", pendingCancel, err)
	}
}

type cancellationConflictStore struct {
	*sqlite.Store
	service  *application.LocalRunService
	source   *clock.Fake
	boundary string
	injected bool
	cancel   bool
	writeErr error
}

func (s *cancellationConflictStore) inject(ctx context.Context, boundary string, runID domain.RunID, version int64, at time.Time) error {
	if boundary != s.boundary || s.injected {
		return nil
	}
	s.injected = true
	if s.cancel {
		result, err := s.service.Cancel(ctx, domain.CancelRequest{
			ID: "control_0000000000000000000000000000cafe", RunID: runID, ExpectedRunVersion: version,
			Reason: "simultaneous cancellation", IdempotencyKey: "cancel_0000000000000000000000000000cafe", At: at,
		})
		if err != nil {
			return err
		}
		if result.State == domain.RunCancelled {
			return errors.New("cancellation did not encounter the owner's run lock")
		}
	}
	return s.writeErr
}

func (s *cancellationConflictStore) BeginStage(ctx context.Context, command domain.BeginStageCommand) (domain.StageAttempt, error) {
	if err := s.inject(ctx, "begin", command.RunID, command.ExpectedRunVersion, command.At); err != nil {
		return domain.StageAttempt{}, err
	}
	return s.Store.BeginStage(ctx, command)
}

func (s *cancellationConflictStore) AccountActiveTime(ctx context.Context, command domain.ActiveTimeCommand) (domain.ActiveTimeResult, error) {
	boundary := map[domain.ActiveTimeAction]string{domain.ActiveTimeStart: "start", domain.ActiveTimeStop: "stop", domain.ActiveTimeRecover: "recover"}[command.Action]
	if err := s.inject(ctx, boundary, command.RunID, command.ExpectedRunVersion, command.At); err != nil {
		return domain.ActiveTimeResult{}, err
	}
	result, err := s.Store.AccountActiveTime(ctx, command)
	if err == nil && command.Action == domain.ActiveTimeStart {
		// Advance before pollers start so each executed stage consumes exactly
		// one second without introducing timer scheduling into the race fixture.
		s.source.Advance(time.Second)
	}
	return result, err
}

func (s *cancellationConflictStore) InterruptStage(ctx context.Context, command domain.InterruptStageCommand) (domain.RunSnapshot, error) {
	if err := s.inject(ctx, "interrupt", command.RunID, command.ExpectedRunVersion, command.At); err != nil {
		return domain.RunSnapshot{}, err
	}
	return s.Store.InterruptStage(ctx, command)
}

func (s *cancellationConflictStore) ApplyReview(ctx context.Context, command domain.ApplyReviewCommand) (domain.RunSnapshot, error) {
	if err := s.inject(ctx, "review", command.RunID, command.ExpectedRunVersion, command.At); err != nil {
		return domain.RunSnapshot{}, err
	}
	return s.Store.ApplyReview(ctx, command)
}

func (s *cancellationConflictStore) FinishStage(ctx context.Context, command domain.FinishStageCommand) (domain.RunSnapshot, error) {
	if err := s.inject(ctx, "finish", command.RunID, command.ExpectedRunVersion, command.At); err != nil {
		return domain.RunSnapshot{}, err
	}
	result, err := s.Store.FinishStage(ctx, command)
	if err == nil {
		err = s.inject(ctx, "between_stages", result.RunID, result.Version, command.At)
	}
	return result, err
}

type cancellationConflictReconciler struct {
	err   error
	calls int
}

func (r *cancellationConflictReconciler) ReconcileRun(_ context.Context, runID domain.RunID) (domain.SandboxReconcileReport, error) {
	r.calls++
	return domain.SandboxReconcileReport{RunID: runID, Completed: r.err == nil}, r.err
}

func newCancellationConflictFixture(t *testing.T, boundary string) (*cancellationConflictStore, *application.LocalRunService, *cancellationConflictReconciler, domain.RunRequest) {
	t.Helper()
	source := clock.NewFake(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	store, err := openFreshApplicationSQLite(t, sqlite.Config{Path: filepath.Join(t.TempDir(), "run.db"), BusyTimeout: time.Second, MaxReaders: 4}, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	locks, err := runlock.NewManager(t.TempDir(), runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locks.Close() })
	pipeline, err := fake.NewPipeline(fake.NewPrepareStep(fake.PrepareCapabilities{}), fake.NewExerciseStep(fake.ExerciseCapabilities{}), fake.NewCheckpointStep(fake.CheckpointCapabilities{}))
	if err != nil {
		t.Fatal(err)
	}
	wrapper := &cancellationConflictStore{Store: store, source: source, boundary: boundary, cancel: true}
	reconciler := &cancellationConflictReconciler{}
	service, err := application.NewRunService(application.RunServiceConfig{Runtime: wrapper, Reviews: wrapper, Locks: locks, Clock: source, Pipeline: pipeline, Reconciler: reconciler})
	if err != nil {
		t.Fatal(err)
	}
	wrapper.service = service
	request := domain.RunRequest{SchemaVersion: domain.RequestSchemaV1, Mode: "offline", Brief: "cancellation race", Language: "cpp", Difficulty: "easy", TimeLimitMilliseconds: 1000, MemoryLimitMegabytes: 64, SolutionLanguage: "go", VerificationProfile: "default", BudgetLimits: domain.BudgetLimits{MaxActiveTimeMilliseconds: 5000}}
	return wrapper, service, reconciler, request
}
