package application_test

import (
	"bytes"
	"context"
	"encoding/json"
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
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

func TestRunGraphRejectsIncompatiblePersistenceBeforeMutation(t *testing.T) {
	for name, alter := range map[string]func(*domain.CreateRunRequest){
		"revision": func(c *domain.CreateRunRequest) {
			c.WorkflowRevision = "uncompiled.v9"
			c.WorkflowDigest = domain.SumBytes([]byte(c.WorkflowRevision))
		},
		"workflow digest": func(c *domain.CreateRunRequest) { c.WorkflowDigest = domain.SumBytes([]byte("wrong workflow")) },
		"schema": func(c *domain.CreateRunRequest) {
			c.SchemaVersion = "cpgen.request/v2"
			c.SubmittedRequestJSON = bytes.ReplaceAll(c.SubmittedRequestJSON, []byte(domain.RequestSchemaV1), []byte(c.SchemaVersion))
			c.SubmittedRequestDigest = domain.SumBytes(c.SubmittedRequestJSON)
		},
		"stage order": func(c *domain.CreateRunRequest) {
			c.StageSequence = []domain.StageName{"exercise", "prepare", "checkpoint"}
		},
		"later stage order": func(c *domain.CreateRunRequest) {
			c.StageSequence = []domain.StageName{"prepare", "checkpoint", "exercise"}
		},
		"extra stage": func(c *domain.CreateRunRequest) {
			c.StageSequence = append(c.StageSequence, "package")
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newRunGraphFixture(t)
			create := f.create(t)
			alter(&create)
			initial, err := f.store.CreateRun(context.Background(), create)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.Resume(context.Background(), initial.RunID); err == nil {
				t.Error("incompatible run was resumed")
			}
			after, err := f.store.GetRun(context.Background(), initial.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(initial, after) {
				t.Fatalf("incompatible run was mutated: before=%+v after=%+v", initial, after)
			}
		})
	}
}

func TestRunGraphCommitFailurePreservesCurrentStageAndResumeSkipsCommittedStage(t *testing.T) {
	f := newRunGraphFixture(t)
	f.runtime.failStage = "exercise"
	result, err := f.service.Generate(context.Background(), f.request)
	if !errors.Is(err, errGraphStageCommit) || result.CurrentStage != "exercise" || result.State != domain.RunRunning {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if !reflect.DeepEqual(f.runtime.finishes, []domain.StageName{"prepare", "exercise"}) {
		t.Fatalf("finishes=%v", f.runtime.finishes)
	}
	f.runtime.failStage = ""
	result, err = f.service.Resume(context.Background(), result.RunID)
	if err != nil || result.State != domain.RunNeedsReview || result.CurrentStage != "checkpoint" {
		t.Fatalf("result=%+v error=%v", result, err)
	}
	if !reflect.DeepEqual(f.runtime.finishes, []domain.StageName{"prepare", "exercise", "exercise", "checkpoint"}) {
		t.Fatalf("committed prepare was repeated: %v", f.runtime.finishes)
	}
}

func TestRunGraphRejectsUnsupportedSlice1InputBeforeCreate(t *testing.T) {
	f := newRunGraphFixture(t)
	request := f.request
	request.Mode, request.Brief = domain.RequestModeRandom, ""
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.Generate(context.Background(), request); err == nil {
		t.Fatal("Slice 1 accepted an unsupported empty typed input")
	}
	runs, err := f.store.ListRuns(context.Background(), domain.RunFilter{})
	if err != nil || len(runs) != 0 {
		t.Fatalf("unsupported Slice 1 input persisted a run: %+v %v", runs, err)
	}
}

func TestRunGraphCancelsBetweenCommittedStagesWithoutStartingNextAttempt(t *testing.T) {
	for _, mode := range []string{"cancel", "resume"} {
		t.Run(mode, func(t *testing.T) {
			f := newRunGraphFixture(t)
			ctx := context.Background()
			create := f.create(t)
			if _, err := f.store.CreateRun(ctx, create); err != nil {
				t.Fatal(err)
			}
			attempt := domain.AttemptID("attempt_00000000000000000000000000001901")
			if _, err := f.store.BeginStage(ctx, domain.BeginStageCommand{RunID: create.RunID, ExpectedRunVersion: 1, StageName: "prepare", AttemptID: attempt, InputDigest: create.SubmittedRequestDigest, IdempotencyKey: coordinatorID("begin", "cancel-gap"), At: f.now}); err != nil {
				t.Fatal(err)
			}
			output := domain.SumBytes([]byte("committed predecessor"))
			committed, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: create.RunID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: attempt, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, IdempotencyKey: coordinatorID("finish", "cancel-gap"), At: f.now})
			if err != nil {
				t.Fatal(err)
			}
			request := domain.CancelRequest{ID: "control_00000000000000000000000000001901", RunID: create.RunID, ExpectedRunVersion: committed.Version, Reason: "cancel between stages", IdempotencyKey: coordinatorID("cancel", "cancel-gap"), At: f.now}
			var result domain.RunSnapshot
			if mode == "cancel" {
				result, err = f.service.Cancel(ctx, request)
			} else {
				if _, err := f.store.RequestCancel(ctx, request); err != nil {
					t.Fatal(err)
				}
				result, err = f.service.Resume(ctx, create.RunID)
			}
			if err != nil || result.State != domain.RunCancelled || result.CurrentStage != "exercise" {
				t.Fatalf("cancel gap=%+v err=%v", result, err)
			}
			if _, err := f.store.CurrentStageAttempt(ctx, create.RunID, "exercise"); !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatalf("next attempt was started: %v", err)
			}
			previous, err := f.store.CurrentStageAttempt(ctx, create.RunID, "prepare")
			if err != nil || previous.AttemptID != attempt || previous.State != domain.StageAttemptSucceeded || previous.OutputDigest == nil || *previous.OutputDigest != output {
				t.Fatal("cancellation changed committed predecessor")
			}
			pending, err := f.store.PendingCancel(ctx, create.RunID)
			if err != nil || pending != nil || len(f.runtime.finishes) != 0 {
				t.Fatal("cancellation retained control or ran the next stage")
			}
			replayed, err := f.service.Resume(ctx, create.RunID)
			if err != nil || !reflect.DeepEqual(result, replayed) {
				t.Fatal("terminal cancel replay changed projection")
			}
		})
	}
}

var errGraphStageCommit = errors.New("fixture graph stage commit failure")

func TestRunGraphCancelAfterCommitIgnoresPreviousOwnerAttempt(t *testing.T) {
	for _, mode := range []string{"cancel", "resume"} {
		t.Run(mode, func(t *testing.T) {
			f := newRunGraphFixture(t)
			f.runtime.interruptAfterStage = "prepare"
			ctx := context.Background()
			result, err := f.service.Generate(ctx, f.request)
			if !errors.Is(err, errGraphStageCommit) {
				t.Fatalf("expected interruption after commit: %v", err)
			}
			current, err := f.store.GetRun(ctx, result.RunID)
			if err != nil || current.CurrentStage != "exercise" {
				t.Fatalf("missing committed successor: %+v %v", current, err)
			}
			request := domain.CancelRequest{ID: "control_00000000000000000000000000001902", RunID: current.RunID, ExpectedRunVersion: current.Version, Reason: "cancel after committed response", IdempotencyKey: coordinatorID("cancel", "cached-cancel-gap"), At: f.now}
			if mode == "cancel" {
				result, err = f.service.Cancel(ctx, request)
			} else {
				if _, err := f.store.RequestCancel(ctx, request); err != nil {
					t.Fatal(err)
				}
				result, err = f.service.Resume(ctx, current.RunID)
			}
			if err != nil || result.State != domain.RunCancelled || !reflect.DeepEqual(f.runtime.finishes, []domain.StageName{"prepare"}) {
				t.Fatalf("previous cached attempt affected cancel: %+v %v", result, err)
			}
			if _, err := f.store.CurrentStageAttempt(ctx, current.RunID, "exercise"); !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatal("cancellation invented a successor attempt")
			}
		})
	}
}

func TestRunGraphResumesAfterCommitWithoutInterruptingPreviousAttempt(t *testing.T) {
	f := newRunGraphFixture(t)
	f.runtime.interruptAfterStage = "prepare"
	ctx := context.Background()
	result, err := f.service.Generate(ctx, f.request)
	if !errors.Is(err, errGraphStageCommit) {
		t.Fatalf("expected interruption after commit: %v", err)
	}
	resumed, err := f.service.Resume(ctx, result.RunID)
	if err != nil || resumed.State != domain.RunNeedsReview || resumed.CurrentStage != "checkpoint" || !reflect.DeepEqual(f.runtime.finishes, []domain.StageName{"prepare", "exercise", "checkpoint"}) {
		t.Fatalf("committed predecessor was interrupted or rerun: %+v %v", resumed, err)
	}
}

type graphRuntime struct {
	*sqlite.Store
	failStage           domain.StageName
	interruptAfterStage domain.StageName
	finishes            []domain.StageName
}

func (r *graphRuntime) FinishStage(ctx context.Context, command domain.FinishStageCommand) (domain.RunSnapshot, error) {
	r.finishes = append(r.finishes, command.StageName)
	if command.StageName == r.failStage {
		return domain.RunSnapshot{}, errGraphStageCommit
	}
	result, err := r.Store.FinishStage(ctx, command)
	if err == nil && command.StageName == r.interruptAfterStage {
		return result, errGraphStageCommit
	}
	return result, err
}

var _ port.RuntimeStore = (*graphRuntime)(nil)

type runGraphFixture struct {
	store   *sqlite.Store
	runtime *graphRuntime
	service *application.LocalRunService
	request domain.RunRequest
	now     time.Time
}

func newRunGraphFixture(t *testing.T) runGraphFixture {
	t.Helper()
	now := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	source := clock.NewFake(now)
	store, err := sqlite.OpenWithClock(context.Background(), sqlite.Config{Path: filepath.Join(t.TempDir(), "graph.db"), BusyTimeout: time.Second, MaxReaders: 4}, source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	locks, err := runlock.NewManager(t.TempDir(), runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locks.Close() })
	pipeline, err := workflow.NewSlice1Pipeline(fake.NewPrepareStep(workflow.PrepareCapabilities{}), fake.NewExerciseStep(workflow.ExerciseCapabilities{}), fake.NewCheckpointStep(workflow.CheckpointCapabilities{}))
	if err != nil {
		t.Fatal(err)
	}
	runtime := &graphRuntime{Store: store}
	service, err := application.NewRunService(application.RunServiceConfig{Runtime: runtime, Locks: locks, Clock: source, Pipeline: pipeline, ActiveTimeInterval: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	request := domain.RunRequest{SchemaVersion: domain.RequestSchemaV1, Mode: "offline", Brief: "graph integration", Language: "cpp", Difficulty: "easy", TimeLimitMilliseconds: 1000, MemoryLimitMegabytes: 64, SolutionLanguage: "go", VerificationProfile: "default", BudgetLimits: domain.BudgetLimits{MaxActiveTimeMilliseconds: 5000}}
	return runGraphFixture{store: store, runtime: runtime, service: service, request: request, now: now}
}

func (f runGraphFixture) create(t *testing.T) domain.CreateRunRequest {
	t.Helper()
	raw, err := json.Marshal(f.request)
	if err != nil {
		t.Fatal(err)
	}
	var fields any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return domain.CreateRunRequest{RunID: "run_00000000000000000000000000000011", SubmittedRequestJSON: raw, SubmittedRequestDigest: domain.SumBytes(raw), EffectiveSeed: 1, RedactedEffectiveConfigJSON: raw, RedactedEffectiveConfigDigest: domain.SumBytes(raw), WorkflowRevision: workflow.Slice1WorkflowRevision, SchemaVersion: domain.RequestSchemaV1, WorkflowDigest: domain.SumBytes([]byte(workflow.Slice1WorkflowRevision)), BudgetLimits: f.request.BudgetLimits, StageSequence: []domain.StageName{"prepare", "exercise", "checkpoint"}, CreatedAt: f.now, IdempotencyKey: "create_00000000000000000000000000000011"}
}
