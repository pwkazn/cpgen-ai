package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func TestInterruptUnsentSandboxStageRequiresClosedNoSendEvidence(t *testing.T) {
	for _, boundary := range []string{"unsent", "open", "prepared", "dispatching", "sent", "unknown", "completed", "aborted_after_dispatch", "unfinished_logical", "execution_planned", "execution_cleaned", "foreign_provider", "unsettled_sibling", "wrong_stage"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			stage := domain.StageName("solution_verify")
			if boundary == "wrong_stage" {
				stage = "data_verify"
			}
			f := unsentSandboxFixture(t, stage)
			open := openCallRequest(f, 1, domain.CallSandboxCompile)
			open.Provider = "docker"
			if boundary == "foreign_provider" {
				open.Provider = "other-docker"
			}
			call, err := f.store.OpenCall(ctx, open)
			if err != nil {
				t.Fatal(err)
			}
			var before domain.PreparedCalls
			if boundary != "open" {
				prepare := prepareOneRequest(f, call, 1, domain.PhysicalDockerContainerCreate, domain.BudgetDockerContainerCreates, 1)
				prepare.Calls[0].Provider = open.Provider
				p, err := f.store.PrepareCalls(ctx, prepare)
				if err != nil {
					t.Fatal(err)
				}
				physical := p.PhysicalCalls[0]
				crossed := boundary == "dispatching" || boundary == "sent" || boundary == "unknown" || boundary == "completed" || boundary == "aborted_after_dispatch"
				if crossed {
					grant := mustBeginDispatch(t, f, call.ID, physical.ID, "unsent boundary")
					if boundary == "sent" || boundary == "completed" {
						if err := f.store.MarkSent(ctx, grant, f.now); err != nil {
							t.Fatal(err)
						}
					}
				}
				if boundary == "execution_planned" || boundary == "execution_cleaned" {
					retainUnsentExecution(t, f, physical.ID, boundary == "execution_cleaned")
				}
				if boundary != "prepared" && boundary != "dispatching" && boundary != "sent" {
					complete := domain.CompletePhysicalRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, State: domain.PhysicalAbortedNoDispatch, Outcome: domain.PhysicalOutcomeNoSend, Failure: &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}, IdempotencyKey: meteringID("test", "complete_unsent"), At: f.now}
					if boundary == "unknown" {
						complete.State, complete.Outcome = domain.PhysicalUnknown, domain.PhysicalOutcomeUnknown
						complete.Failure = &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
					}
					if boundary == "completed" {
						complete.State, complete.Outcome, complete.Failure = domain.PhysicalCompleted, domain.PhysicalOutcomeSuccess, nil
						complete.ProviderRequestID, complete.ResponseDigest = "container-one", digestPointer("created")
					}
					if err := f.store.CompletePhysical(ctx, complete); err != nil {
						t.Fatal(err)
					}
					if boundary != "unfinished_logical" {
						finish := domain.FinishCallRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID, CallRecordID: call.ID, DispatchKind: domain.DispatchNone, Failure: complete.Failure, IdempotencyKey: meteringID("test", "finish_unsent"), At: f.now}
						if boundary == "unknown" || boundary == "completed" {
							finish.DispatchKind, finish.ResultAttemptCallID = domain.DispatchDispatched, &physical.ID
						}
						if _, err := f.store.FinishCall(ctx, finish); err != nil {
							t.Fatal(err)
						}
					}
				}
				before, err = f.store.LoadCall(ctx, call.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "unsettled_sibling" {
				other := openCallRequest(f, 2, domain.CallSandboxCompile)
				other.Provider = "blob"
				if _, err := f.store.OpenCall(ctx, other); err != nil {
					t.Fatal(err)
				}
			}
			budget, err := f.store.BudgetSnapshot(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			command := domain.InterruptStageCommand{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID, Cause: domain.CauseRevisionInvalidated, IdempotencyKey: meteringID("test", "interrupt_unsent"), At: f.now.Add(time.Second)}
			result, err := f.store.InterruptUnsentSandboxStage(ctx, command)
			if err != nil {
				t.Fatal(err)
			}
			wantState, wantVersion := domain.RunRunning, int64(2)
			if boundary == "unsent" {
				wantState, wantVersion = domain.RunCreated, 3
			}
			if result.State != wantState || result.Version != wantVersion {
				t.Fatalf("unsafe restart decision: %+v", result)
			}
			if boundary != "open" {
				after, err := f.store.LoadCall(ctx, call.ID)
				if err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("original call evidence changed: %v", err)
				}
			}
			afterBudget, err := f.store.BudgetSnapshot(ctx, f.runID)
			if err != nil || !reflect.DeepEqual(budget, afterBudget) {
				t.Fatalf("budget reset: %+v %v", afterBudget, err)
			}
			if boundary == "unsent" {
				again, err := f.store.InterruptUnsentSandboxStage(ctx, command)
				if err != nil || !reflect.DeepEqual(result, again) {
					t.Fatalf("interruption replay: %+v %v", again, err)
				}
				// Crash after interruption but before BeginStage keeps the old
				// attempt closed; the ordinary next attempt consumes its ordinal.
				fresh := mustBeginStage(t, f.store, f.runID, "attempt_000000000000000000000000000000d2", result.Version, f.stage, domain.SumBytes([]byte("unsent input")), f.now.Add(2*time.Second), meteringID("test", "begin_recovered"))
				if fresh.Ordinal != 2 {
					t.Fatalf("attempt budget reset: %+v", fresh)
				}
			}
		})
	}
}

func TestInterruptUnsentSandboxStageRejectsStaleVersion(t *testing.T) {
	f := unsentSandboxFixture(t, "solution_verify")
	_, err := f.store.InterruptUnsentSandboxStage(context.Background(), domain.InterruptStageCommand{RunID: f.runID, ExpectedRunVersion: 1, StageName: f.stage, AttemptID: f.attemptID, Cause: domain.CauseRevisionInvalidated, IdempotencyKey: meteringID("test", "stale_unsent"), At: f.now})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("stale recovery: %v", err)
	}
}

func unsentSandboxFixture(t *testing.T, stage domain.StageName) meteringFixture {
	t.Helper()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "unsent.db"), clock.NewFake(testNow))
	request := testCreateRunRequest(testRunID, testNow, time.Minute)
	request.StageSequence = []domain.StageName{stage, "done"}
	mustCreateRun(t, store, request)
	mustBeginStage(t, store, testRunID, testAttemptID, 1, stage, domain.SumBytes([]byte("unsent input")), testNow, meteringID("test", "begin_unsent"))
	return meteringFixture{store: store, runID: testRunID, attemptID: testAttemptID, stage: stage, now: testNow}
}

func retainUnsentExecution(t *testing.T, f meteringFixture, physical domain.AttemptCallID, cleaned bool) {
	t.Helper()
	ctx := context.Background()
	id := domain.SandboxExecutionID("sandbox_000000000000000000000000000000d1")
	engine := domain.SumBytes([]byte("engine"))
	resource := domain.SandboxResource{ID: "resource_000000000000000000000000000000d1", ExecutionID: id, PlanOrdinal: 0, Kind: "CONTAINER", Role: "TARGET", PhysicalCallID: &physical, DeterministicName: "cpgen-unsent-target", ExpectedLabelsDigest: domain.SumBytes([]byte("labels")), EngineIdentityDigest: engine, Phase: domain.SandboxResourcePlanned, Version: 1, CreatedAt: f.now, UpdatedAt: f.now}
	e, err := f.store.PrepareExecution(ctx, domain.PrepareExecutionRequest{ExecutionID: id, RunID: f.runID, AttemptID: f.attemptID, StageName: f.stage, LogicalOperationID: "unsent-execution", ScopeDigest: domain.SumBytes([]byte("scope")), PlanDigest: domain.SumBytes([]byte("plan")), EngineIdentityDigest: engine, Resources: []domain.SandboxResource{resource}, WatchdogControlRef: "unsent-control", WatchdogTokenDigest: domain.SumBytes([]byte("token")), SafetyDeadlineUTC: f.now.Add(time.Minute), CleanupDeadlineUTC: f.now.Add(2 * time.Minute), IdempotencyKey: meteringID("test", "prepare_unsent_execution"), At: f.now})
	if err != nil {
		t.Fatal(err)
	}
	if !cleaned {
		return
	}
	e, err = f.store.MarkCleanupPending(ctx, domain.MarkCleanupPendingCommand{ExecutionID: id, ExpectedVersion: e.LifecycleVersion, Reason: "test no-create", IdempotencyKey: meteringID("test", "pending_unsent"), At: f.now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.RecordResourceInterrupted(ctx, domain.RecordResourceInterruptedCommand{ExecutionID: id, ResourceID: resource.ID, ExpectedVersion: 1, ReasonDigest: domain.SumBytes([]byte("no create")), At: f.now})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.FinishCleanup(ctx, domain.FinishCleanupCommand{ExecutionID: id, ExpectedVersion: e.LifecycleVersion, ReconciliationDigest: domain.SumBytes([]byte("cleaned")), IdempotencyKey: meteringID("test", "cleaned_unsent"), At: f.now})
	if err != nil {
		t.Fatal(err)
	}
}
