package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestContentRetryAtomicRewindReplayAndPersistentLimit(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "retry.db")
	store := openRuntimeStore(t, path, clock.NewFake(testNow))
	create := testCreateRunRequest(testRunID, testNow, time.Minute)
	create.WorkflowRevision = workflow.RetryingGenerationRevision
	create.WorkflowDigest = domain.SumBytes([]byte(create.WorkflowRevision))
	definition, _ := workflow.DefinitionFor(create.WorkflowRevision)
	create.StageSequence = definition.Stages()
	run := mustCreateRun(t, store, create)
	now := testNow
	serial := 0
	begin := func() domain.StageAttempt {
		t.Helper()
		serial++
		now = now.Add(time.Second)
		input, err := store.ReadStageInputDigest(ctx, run.RunID, run.CurrentStage)
		if err != nil {
			t.Fatal(err)
		}
		attempt, err := store.BeginStage(ctx, domain.BeginStageCommand{RunID: run.RunID, ExpectedRunVersion: run.Version, StageName: run.CurrentStage, AttemptID: domain.AttemptID(fmt.Sprintf("attempt_%032x", serial)), InputDigest: input, IdempotencyKey: fmt.Sprintf("begin_%032x", serial), At: now})
		if err != nil {
			t.Fatal(err)
		}
		run, err = store.GetRun(ctx, run.RunID)
		if err != nil {
			t.Fatal(err)
		}
		return attempt
	}
	for round := 0; round < 3; round++ {
		for run.CurrentStage != "solution_decision" {
			attempt := begin()
			digest := domain.SumBytes([]byte(fmt.Sprintf("output-%d", serial)))
			now = now.Add(time.Second)
			var err error
			run, err = store.FinishStage(ctx, domain.FinishStageCommand{RunID: run.RunID, ExpectedRunVersion: run.Version, StageName: run.CurrentStage, AttemptID: attempt.AttemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &digest, NextInputDigest: &digest, NextStage: create.StageSequence[run.CurrentStageOrdinal], At: now, IdempotencyKey: fmt.Sprintf("finish_%032x", serial)})
			if err != nil {
				t.Fatal(err)
			}
		}
		attempt := begin()
		now = now.Add(time.Second)
		evidence := domain.SumBytes([]byte(fmt.Sprintf("bad-sample-%d", round)))
		command := domain.FinishContentRetryCommand{Finish: domain.FinishStageCommand{RunID: run.RunID, ExpectedRunVersion: run.Version, StageName: run.CurrentStage, AttemptID: attempt.AttemptID, AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview, ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &run.ConfigDigest, At: now, IdempotencyKey: fmt.Sprintf("retry_%032x", round)}, Reason: "solution_requires_review:sample.2.SOLUTION.WA"}
		before, err := store.BudgetSnapshot(ctx, run.RunID)
		if err != nil {
			t.Fatal(err)
		}
		run, err = store.FinishContentRetry(ctx, command)
		if err != nil {
			t.Fatal(err)
		}
		replay, err := store.FinishContentRetry(ctx, command)
		if err != nil || !reflect.DeepEqual(run, replay) {
			t.Fatalf("replay changed result: %+v %v", replay, err)
		}
		after, err := store.BudgetSnapshot(ctx, run.RunID)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatalf("retry changed budgets: %v", err)
		}
		var retries, attempts, reviews int
		for query, target := range map[string]*int{"SELECT count(*) FROM content_retries": &retries, "SELECT count(*) FROM stage_attempts": &attempts, "SELECT count(*) FROM review_decisions": &reviews} {
			if err := store.db.QueryRowContext(ctx, query).Scan(target); err != nil {
				t.Fatal(err)
			}
		}
		if retries != min(round+1, 2) || attempts != serial || reviews != 0 {
			t.Fatalf("counts retry=%d attempts=%d review=%d", retries, attempts, reviews)
		}
		if round < 2 {
			if run.State != domain.RunRunning || run.CurrentStage != "statement" {
				t.Fatalf("retry did not rewind: %+v", run)
			}
			var stale int
			if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM stage_records WHERE ordinal>=2 AND (state<>'PENDING' OR output_digest IS NOT NULL OR current_attempt_id IS NOT NULL OR review_evidence_digest IS NOT NULL)`).Scan(&stale); err != nil || stale != 0 {
				t.Fatalf("suffix not invalidated: %d %v", stale, err)
			}
			if _, err := store.ReadCommittedLLMStage(ctx, run.RunID, "statement"); err == nil {
				t.Fatal("invalidated draft remained current")
			}
			// Restart between failure and regeneration; the next retry must use
			// the persisted count and new attempt ordinals, not process memory.
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store = openRuntimeStore(t, path, clock.NewFake(now))
		} else if run.State != domain.RunNeedsReview || run.CurrentStage != "solution_decision" {
			t.Fatalf("retry limit did not stop: %+v", run)
		}
	}
	idea, err := store.CurrentStageAttempt(ctx, run.RunID, "idea")
	if err != nil || idea.Ordinal != 1 || idea.State != domain.StageAttemptSucceeded {
		t.Fatalf("unaffected Idea changed: %+v %v", idea, err)
	}
}

func TestContentRetryRevisionCanFinalizeVerifiedPackage(t *testing.T) {
	store, command := packageCommitFixture(t, workflow.RetryingGenerationRevision)
	run, err := store.FinalizeVerifiedPackage(context.Background(), command)
	if err != nil || run.State != domain.RunReady {
		t.Fatalf("new workflow cannot finish: %+v %v", run, err)
	}
	replay, err := store.FinalizeVerifiedPackage(context.Background(), command)
	if err != nil || !reflect.DeepEqual(run, replay) {
		t.Fatalf("package replay changed: %+v %v", replay, err)
	}
}

func TestContentRetryRejectsCancellationAndUnsupportedFailureAtomically(t *testing.T) {
	for _, mode := range []string{"cancel", "unsupported", "legacy"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			store := openRuntimeStore(t, filepath.Join(t.TempDir(), "retry.db"), clock.NewFake(testNow))
			create := testCreateRunRequest(testRunID, testNow, time.Minute)
			create.WorkflowRevision = workflow.RetryingGenerationRevision
			if mode == "legacy" {
				create.WorkflowRevision = workflow.GenerationRevision
			}
			create.WorkflowDigest = domain.SumBytes([]byte(create.WorkflowRevision))
			definition, _ := workflow.DefinitionFor(create.WorkflowRevision)
			create.StageSequence = definition.Stages()
			run := mustCreateRun(t, store, create)
			input, err := store.ReadStageInputDigest(ctx, run.RunID, "idea")
			if err != nil {
				t.Fatal(err)
			}
			mustBeginStage(t, store, run.RunID, testAttemptID, run.Version, "idea", input, testNow, meteringID("begin", mode))
			run, err = store.GetRun(ctx, run.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				_, err = store.RequestCancel(ctx, domain.CancelRequest{ID: "control_00000000000000000000000000001902", RunID: run.RunID, ExpectedRunVersion: run.Version, Reason: "stop before retry", At: testNow, IdempotencyKey: meteringID("cancel", mode)})
				if err != nil {
					t.Fatal(err)
				}
				run, err = store.GetRun(ctx, run.RunID)
				if err != nil {
					t.Fatal(err)
				}
			}
			evidence := domain.SumBytes([]byte("bad content"))
			command := domain.FinishContentRetryCommand{Finish: domain.FinishStageCommand{RunID: run.RunID, ExpectedRunVersion: run.Version, StageName: "idea", AttemptID: testAttemptID, AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview, ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &run.ConfigDigest, At: testNow, IdempotencyKey: meteringID("finish", mode)}, Reason: "idea_binding_rejected"}
			if mode == "unsupported" {
				command.Reason = "llm_budget_exhausted"
			}
			_, err = store.FinishContentRetry(ctx, command)
			if err == nil || (mode == "cancel" && !errors.Is(err, ErrCancelPending)) {
				t.Fatalf("retry accepted %s: %v", mode, err)
			}
			after, err := store.GetRun(ctx, run.RunID)
			if err != nil || !reflect.DeepEqual(run, after) {
				t.Fatalf("rejected retry changed projection: %+v %v", after, err)
			}
			attempt, err := store.CurrentStageAttempt(ctx, run.RunID, "idea")
			if err != nil || attempt.State != domain.StageAttemptRunning {
				t.Fatalf("failed transaction finished attempt: %+v %v", attempt, err)
			}
			var count int
			if err := store.db.QueryRowContext(ctx, `SELECT count(*) FROM content_retries`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("failed transaction spent allowance: %d %v", count, err)
			}
		})
	}
}
