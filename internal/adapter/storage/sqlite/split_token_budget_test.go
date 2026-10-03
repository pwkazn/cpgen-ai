package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func TestSplitTokenBudgetReservationsCannotBorrowAcrossCaps(t *testing.T) {
	for _, test := range []struct {
		name                string
		inputCap, outputCap int64
		input, output       int64
		exhausted           bool
	}{
		{name: "input exceeds cap with output room", inputCap: 3, outputCap: 20, input: 4, output: 1, exhausted: true},
		{name: "output exceeds cap with input room", inputCap: 20, outputCap: 3, input: 1, output: 4, exhausted: true},
		{name: "explicit zero input", inputCap: 0, outputCap: 20, input: 1, output: 1, exhausted: true},
		{name: "explicit zero output", inputCap: 20, outputCap: 0, input: 1, output: 1, exhausted: true},
		{name: "both exact caps are available", inputCap: 3, outputCap: 5, input: 3, output: 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			limits := splitTokenLimits(t, test.inputCap, test.outputCap)
			f := newMeteringFixture(t, "b1", limits)
			assertSplitTokenProjection(t, f, limits, [2]int64{}, [2]int64{})
			record := mustOpenMeteringCall(t, f, 1, domain.CallLLMGenerate)
			request := totalTokenPlan(f, record, 1, test.input, test.output)
			prepared, err := f.store.PrepareCalls(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if test.exhausted {
				if prepared.Failure == nil || prepared.Failure.Code != domain.FailureBudgetExhausted || len(prepared.PhysicalCalls) != 0 {
					t.Fatalf("expected budget rejection: %+v", prepared)
				}
				assertSplitTokenProjection(t, f, limits, [2]int64{}, [2]int64{})
			} else {
				if prepared.Failure != nil || len(prepared.PhysicalCalls) != 1 {
					t.Fatalf("expected reservation admission: %+v", prepared)
				}
				assertSplitTokenProjection(t, f, limits, [2]int64{}, [2]int64{test.input, test.output})
			}
			replayed, err := f.store.PrepareCalls(context.Background(), request)
			if err != nil || !reflect.DeepEqual(prepared, replayed) {
				t.Fatalf("PrepareCalls replay = %+v, %v; want %+v", replayed, err, prepared)
			}
		})
	}
}

func TestSplitTokenBudgetSettlesAndReleasesEachAllowanceIndependently(t *testing.T) {
	ctx := context.Background()
	limits := splitTokenLimits(t, 10, 10)
	f := newMeteringFixture(t, "b2", limits)
	record := mustOpenMeteringCall(t, f, 1, domain.CallLLMGenerate)
	request := totalTokenPlan(f, record, 1, 6, 3)
	request.Calls = append(request.Calls, llmPhysicalPlan(2, 2, []int64{1, 4, 5, 1}))
	prepared, err := f.store.PrepareCalls(ctx, request)
	if err != nil || prepared.Failure != nil {
		t.Fatalf("PrepareCalls = %+v, %v", prepared, err)
	}
	assertSplitTokenProjection(t, f, limits, [2]int64{}, [2]int64{10, 8})
	grant := mustBeginDispatch(t, f, record.ID, prepared.PhysicalCalls[0].ID, "split tokens")
	if err := f.store.MarkSent(ctx, grant, f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	complete := domain.CompletePhysicalRequest{
		RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID,
		CallRecordID: record.ID, AttemptCallID: grant.AttemptCallID,
		State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess,
		ProviderRequestID: "split-token-usage", ResponseDigest: digestPointer("split token response"),
		Usage: []domain.ReservationUsage{
			{ReservationID: reservationIDFor(t, prepared, grant.AttemptCallID, domain.BudgetLLMInputTokens), Dimension: domain.BudgetLLMInputTokens, Subkey: "input", Value: 3, Verified: true},
			{ReservationID: reservationIDFor(t, prepared, grant.AttemptCallID, domain.BudgetLLMOutputTokens), Dimension: domain.BudgetLLMOutputTokens, Subkey: "output", Value: 1, Verified: true},
		},
		IdempotencyKey: meteringID("complete", "split token usage"), At: f.now.Add(2 * time.Second),
	}
	for range 2 {
		if err := f.store.CompletePhysical(ctx, complete); err != nil {
			t.Fatal(err)
		}
	}
	assertSplitTokenProjection(t, f, limits, [2]int64{3, 1}, [2]int64{4, 5})
	finish := domain.FinishCallRequest{
		RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID,
		CallRecordID: record.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &grant.AttemptCallID,
		IdempotencyKey: meteringID("finish", "split token usage"), At: f.now.Add(3 * time.Second),
	}
	for range 2 {
		if _, err := f.store.FinishCall(ctx, finish); err != nil {
			t.Fatal(err)
		}
	}
	assertSplitTokenProjection(t, f, limits, [2]int64{3, 1}, [2]int64{})
	for i, amount := range [][2]int64{{8, 1}, {1, 10}, {7, 9}} {
		next := mustOpenMeteringCall(t, f, i+2, domain.CallLLMGenerate)
		prepared, err := f.store.PrepareCalls(ctx, totalTokenPlan(f, next, i+3, amount[0], amount[1]))
		if err != nil || (prepared.Failure != nil) != (i < 2) {
			t.Fatalf("reserve after settlement (%v) = %+v, %v", amount, prepared, err)
		}
	}
	assertSplitTokenProjection(t, f, limits, [2]int64{3, 1}, [2]int64{7, 9})
}

func TestSplitTokenBudgetReviewIncreasesOnlyInputAllowance(t *testing.T) {
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "split-token-review.db"), clock.NewFake(testNow))
	create := testCreateRunRequest(testRunID, testNow, 30*time.Second)
	create.BudgetLimits = splitTokenLimits(t, 0, 5)
	create.SubmittedRequestJSON = canonicalSQLiteRunRequestJSON(create.BudgetLimits)
	create.SubmittedRequestDigest = domain.SumBytes(create.SubmittedRequestJSON)
	snapshot, binding := driveRunRequestToNeedsReview(t, store, create, testAttemptID, false)
	request := testCreateReviewRequest(testRunID, snapshot.Version, domain.ReviewRetry, binding, 81)
	request.BudgetIncrease = domain.BudgetLimits{MaxLLMInputTokens: 7}
	decision, err := store.CreateReview(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	command := domain.ApplyReviewCommand{
		RunID: testRunID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID,
		StageName: binding.stage, StageInputDigest: binding.input, EvidenceDigest: binding.evidence, PolicyDigest: binding.policy,
		IdempotencyKey: "reviewapply_00000000000000000000000000000081", At: testNow.Add(4 * time.Second),
	}
	first, err := store.ApplyReview(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := store.ApplyReview(ctx, command)
	if err != nil || !reflect.DeepEqual(first, replayed) {
		t.Fatalf("ApplyReview replay = %+v, %v; want %+v", replayed, err, first)
	}
	want := create.BudgetLimits
	want.MaxLLMInputTokens = 7
	f := meteringFixture{store: store, runID: testRunID}
	assertSplitTokenProjection(t, f, want, [2]int64{}, [2]int64{})
	var submitted []byte
	if err := store.db.QueryRow(`SELECT submitted_request_json FROM runs WHERE run_id=?`, testRunID).Scan(&submitted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(submitted, create.SubmittedRequestJSON) {
		t.Fatal("review rewrote the admitted request")
	}
	var grants, applications int
	if err := store.db.QueryRow(`SELECT count(*) FROM review_budget_grants WHERE run_id=? AND field='max_llm_input_tokens'`, testRunID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM review_budget_applications WHERE run_id=? AND field='max_llm_input_tokens'`, testRunID).Scan(&applications); err != nil || grants != 1 || applications != 1 {
		t.Fatalf("input audit counts grants=%d applications=%d, %v", grants, applications, err)
	}
	attemptID := domain.AttemptID("attempt_00000000000000000000000000000082")
	mustBeginStage(t, store, testRunID, attemptID, first.Version, binding.stage, binding.input, testNow.Add(5*time.Second), "begin_00000000000000000000000000000082")
	f.attemptID, f.stage, f.now = attemptID, binding.stage, testNow.Add(6*time.Second)
	for i, amount := range [][2]int64{{8, 1}, {1, 6}, {7, 5}} {
		open := openCallRequest(f, i+1, domain.CallLLMGenerate)
		open.ExpectedRunVersion = first.Version + 1
		record, err := store.OpenCall(ctx, open)
		if err != nil {
			t.Fatal(err)
		}
		plan := totalTokenPlan(f, record, i+1, amount[0], amount[1])
		plan.ExpectedRunVersion = first.Version + 1
		prepared, err := store.PrepareCalls(ctx, plan)
		if err != nil || (prepared.Failure != nil) != (i < 2) {
			t.Fatalf("reserve after input allowance increase (%v) = %+v, %v", amount, prepared, err)
		}
	}
	assertSplitTokenProjection(t, f, want, [2]int64{}, [2]int64{7, 5})
}

func splitTokenLimits(t *testing.T, input, output int64) domain.BudgetLimits {
	t.Helper()
	limits, err := domain.NewSplitTokenBudgetLimits(input, output)
	if err != nil {
		t.Fatal(err)
	}
	return limits
}

func assertSplitTokenProjection(t *testing.T, f meteringFixture, limits domain.BudgetLimits, used, reserved [2]int64) {
	t.Helper()
	budget, err := f.store.BudgetSnapshot(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	workbench, err := f.store.ReadWorkbenchRun(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if budget.Limits != limits || workbench.Budget.Limits != limits || !budget.Limits.SplitTokenBudget || budget.Limits.UsesTokenBudget() {
		t.Fatalf("budget projection lost independent limits: snapshot=%+v workbench=%+v want=%+v", budget.Limits, workbench.Budget.Limits, limits)
	}
	for index, dimension := range []domain.BudgetDimension{domain.BudgetLLMInputTokens, domain.BudgetLLMOutputTokens} {
		cap := []int64{limits.MaxLLMInputTokens, limits.MaxLLMOutputTokens}[index]
		remaining := cap - used[index] - reserved[index]
		if budget.Remaining[dimension] != remaining || workbench.Budget.Remaining[dimension] != remaining || workbench.BudgetUsed[dimension] != used[index] || workbench.BudgetReserved[dimension] != reserved[index] {
			t.Fatalf("%s projection: remaining=%d workbench remaining=%d used=%d reserved=%d; want remaining=%d used=%d reserved=%d", dimension, budget.Remaining[dimension], workbench.Budget.Remaining[dimension], workbench.BudgetUsed[dimension], workbench.BudgetReserved[dimension], remaining, used[index], reserved[index])
		}
	}
	if _, ok := budget.Remaining[domain.BudgetLLMTokens]; ok {
		t.Fatal("independent allowances exposed a shared remaining cap")
	}
	if _, ok := workbench.Budget.Remaining[domain.BudgetLLMTokens]; ok {
		t.Fatal("workbench exposed a shared remaining cap for independent allowances")
	}
}
