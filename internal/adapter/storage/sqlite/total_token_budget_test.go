package sqlite

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func TestTotalTokenBudgetCombinesReservationsAndRejectsOverflow(t *testing.T) {
	for _, test := range []struct {
		name               string
		cap, input, output int64
	}{
		{"combined", 10, 6, 5},
		{"explicit zero", 0, 1, 1},
		{"overflow", math.MaxInt64, math.MaxInt64, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newMeteringFixture(t, "a1", totalTokenLimits(t, test.cap))
			record := mustOpenMeteringCall(t, f, 1, domain.CallLLMGenerate)
			request := totalTokenPlan(f, record, 1, test.input, test.output)
			prepared, err := f.store.PrepareCalls(context.Background(), request)
			if err != nil || prepared.Failure == nil || prepared.Failure.Code != domain.FailureBudgetExhausted || len(prepared.PhysicalCalls) != 0 {
				t.Fatalf("PrepareCalls = %+v, %v", prepared, err)
			}
			replayed, err := f.store.PrepareCalls(context.Background(), request)
			if err != nil || !reflect.DeepEqual(replayed, prepared) {
				t.Fatalf("replay = %+v, %v; want %+v", replayed, err, prepared)
			}
			assertAccountValues(t, f.store, f.runID, map[domain.BudgetDimension][2]int64{
				domain.BudgetLLMInputTokens: {0, 0}, domain.BudgetLLMOutputTokens: {0, 0},
			})
		})
	}
}

func TestTotalTokenBudgetConcurrentReservationsShareOneCap(t *testing.T) {
	f := newMeteringFixture(t, "a2", totalTokenLimits(t, 10))
	requests := make([]domain.PrepareCallsRequest, 2)
	for i := range requests {
		record := mustOpenMeteringCall(t, f, i+1, domain.CallLLMGenerate)
		requests[i] = totalTokenPlan(f, record, i+1, 3, 4)
	}
	var workers sync.WaitGroup
	start := make(chan struct{})
	results := make(chan domain.PreparedCalls, 2)
	for _, request := range requests {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			prepared, err := f.store.PrepareCalls(context.Background(), request)
			if err != nil {
				t.Errorf("PrepareCalls: %v", err)
			}
			results <- prepared
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	admitted, exhausted := 0, 0
	for result := range results {
		if result.Failure != nil && result.Failure.Code == domain.FailureBudgetExhausted {
			exhausted++
		} else if len(result.PhysicalCalls) == 1 {
			admitted++
		}
	}
	if admitted != 1 || exhausted != 1 {
		t.Fatalf("admitted=%d exhausted=%d", admitted, exhausted)
	}
	assertTotalTokenProjection(t, f, 10, 0, 7, 3)
	if _, err := f.store.db.Exec(`UPDATE budget_accounts SET reserved_value=reserved_value+4,account_version=account_version+1 WHERE run_id=? AND dimension='LLM_OUTPUT_TOKENS'`, f.runID); err == nil {
		t.Fatal("direct usage update bypassed the shared token cap")
	}
}

func TestTotalTokenBudgetSettlesUsageAndReleasesUnusedRetries(t *testing.T) {
	f := newMeteringFixture(t, "a3", totalTokenLimits(t, 10))
	record := mustOpenMeteringCall(t, f, 1, domain.CallLLMGenerate)
	request := totalTokenPlan(f, record, 1, 3, 2)
	request.Calls = append(request.Calls, llmPhysicalPlan(2, 2, []int64{1, 3, 2, 1}))
	prepared, err := f.store.PrepareCalls(context.Background(), request)
	if err != nil || prepared.Failure != nil {
		t.Fatalf("PrepareCalls = %+v, %v", prepared, err)
	}
	assertTotalTokenProjection(t, f, 10, 0, 10, 0)
	grant := mustBeginDispatch(t, f, record.ID, prepared.PhysicalCalls[0].ID, "total tokens")
	if err := f.store.MarkSent(context.Background(), grant, f.now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	complete := domain.CompletePhysicalRequest{
		RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID,
		CallRecordID: record.ID, AttemptCallID: grant.AttemptCallID,
		State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess,
		ProviderRequestID: "total-token-usage", ResponseDigest: digestPointer("total token response"),
		Usage: []domain.ReservationUsage{
			{ReservationID: reservationIDFor(t, prepared, grant.AttemptCallID, domain.BudgetLLMInputTokens), Dimension: domain.BudgetLLMInputTokens, Subkey: "input", Value: 2, Verified: true},
			{ReservationID: reservationIDFor(t, prepared, grant.AttemptCallID, domain.BudgetLLMOutputTokens), Dimension: domain.BudgetLLMOutputTokens, Subkey: "output", Value: 1, Verified: true},
		},
		IdempotencyKey: meteringID("complete", "total token usage"), At: f.now.Add(2 * time.Second),
	}
	for range 2 {
		if err := f.store.CompletePhysical(context.Background(), complete); err != nil {
			t.Fatal(err)
		}
	}
	assertTotalTokenProjection(t, f, 10, 3, 5, 2)
	finish := domain.FinishCallRequest{
		RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID,
		CallRecordID: record.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &grant.AttemptCallID,
		IdempotencyKey: meteringID("finish", "total token usage"), At: f.now.Add(3 * time.Second),
	}
	for range 2 {
		if _, err := f.store.FinishCall(context.Background(), finish); err != nil {
			t.Fatal(err)
		}
	}
	assertTotalTokenProjection(t, f, 10, 3, 0, 7)
	next := mustOpenMeteringCall(t, f, 2, domain.CallLLMGenerate)
	accepted, err := f.store.PrepareCalls(context.Background(), totalTokenPlan(f, next, 3, 1, 6))
	if err != nil || accepted.Failure != nil {
		t.Fatalf("reuse released tokens = %+v, %v", accepted, err)
	}
	assertTotalTokenProjection(t, f, 10, 3, 7, 0)
}

func TestTotalTokenBudgetReviewIncreasesZeroCapExactlyOnce(t *testing.T) {
	ctx := context.Background()
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "token-review.db"), clock.NewFake(testNow))
	create := testCreateRunRequest(testRunID, testNow, 30*time.Second)
	create.BudgetLimits = totalTokenLimits(t, 0)
	create.SubmittedRequestJSON = canonicalSQLiteRunRequestJSON(create.BudgetLimits)
	create.SubmittedRequestDigest = domain.SumBytes(create.SubmittedRequestJSON)
	snapshot, binding := driveRunRequestToNeedsReview(t, store, create, testAttemptID, false)
	request := testCreateReviewRequest(testRunID, snapshot.Version, domain.ReviewRetry, binding, 71)
	request.BudgetIncrease = domain.BudgetLimits{MaxLLMTokens: 7}
	decision, err := store.CreateReview(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	command := domain.ApplyReviewCommand{
		RunID: testRunID, ExpectedRunVersion: decision.RunVersion, ReviewDecisionID: decision.ID,
		StageName: binding.stage, StageInputDigest: binding.input, EvidenceDigest: binding.evidence, PolicyDigest: binding.policy,
		IdempotencyKey: "reviewapply_00000000000000000000000000000071", At: testNow.Add(4 * time.Second),
	}
	first, err := store.ApplyReview(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.ApplyReview(ctx, command)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("review replay = %+v, %v", second, err)
	}
	budget, err := store.BudgetSnapshot(ctx, testRunID)
	if err != nil || budget.Limits.MaxLLMTokens != 7 || !budget.Limits.TokenBudget || budget.Remaining[domain.BudgetLLMTokens] != 7 {
		t.Fatalf("increased budget = %+v, %v", budget, err)
	}
	var submitted []byte
	var grants, applications int
	if err := store.db.QueryRow(`SELECT submitted_request_json FROM runs WHERE run_id=?`, testRunID).Scan(&submitted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(submitted, create.SubmittedRequestJSON) {
		t.Fatal("review rewrote the admitted request")
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM review_budget_grants WHERE run_id=? AND field='max_llm_tokens'`, testRunID).Scan(&grants); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRow(`SELECT count(*) FROM review_budget_applications WHERE run_id=? AND field='max_llm_tokens'`, testRunID).Scan(&applications); err != nil || applications != 1 || grants != 1 {
		t.Fatalf("audit counts grants=%d applications=%d, %v", grants, applications, err)
	}
	if _, err := store.db.Exec(`UPDATE runs SET max_llm_tokens=max_llm_tokens+1 WHERE run_id=?`, testRunID); err == nil {
		t.Fatal("direct shared cap increase bypassed approval")
	}
	if _, err := store.db.Exec(`UPDATE runs SET token_budget=0 WHERE run_id=?`, testRunID); err == nil {
		t.Fatal("direct token mode change succeeded")
	}
	// Resume through the ordinary execution path, so an approved cap must be
	// usable by metering and still stop a plan beyond the new shared amount.
	attemptID := domain.AttemptID("attempt_00000000000000000000000000000072")
	mustBeginStage(t, store, testRunID, attemptID, first.Version, binding.stage, binding.input, testNow.Add(5*time.Second), "begin_00000000000000000000000000000072")
	f := meteringFixture{store: store, runID: testRunID, attemptID: attemptID, stage: binding.stage, now: testNow.Add(6 * time.Second)}
	for i, amount := range []int64{8, 7} {
		open := openCallRequest(f, i+1, domain.CallLLMGenerate)
		open.ExpectedRunVersion = first.Version + 1
		record, err := store.OpenCall(ctx, open)
		if err != nil {
			t.Fatal(err)
		}
		plan := totalTokenPlan(f, record, i+1, 1, amount-1)
		plan.ExpectedRunVersion = first.Version + 1
		prepared, err := store.PrepareCalls(ctx, plan)
		if err != nil || (prepared.Failure != nil) != (amount > 7) {
			t.Fatalf("prepare after shared token increase (%d) = %+v, %v", amount, prepared, err)
		}
	}
}

func TestTotalTokenBudgetReviewRejectsLegacyRun(t *testing.T) {
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "legacy-token-review.db"), clock.NewFake(testNow))
	snapshot, binding := driveRunToNeedsReview(t, store, testRunID, testAttemptID)
	request := testCreateReviewRequest(testRunID, snapshot.Version, domain.ReviewRetry, binding, 72)
	request.BudgetIncrease = domain.BudgetLimits{MaxLLMTokens: 7}
	if _, err := store.CreateReview(context.Background(), request); !errors.Is(err, ErrConsistency) {
		t.Fatalf("legacy total token increase = %v", err)
	}
}

func totalTokenLimits(t *testing.T, tokens int64) domain.BudgetLimits {
	t.Helper()
	limits, err := domain.NewTokenBudgetLimits(tokens)
	if err != nil {
		t.Fatal(err)
	}
	return limits
}

func totalTokenPlan(f meteringFixture, record domain.CallRecord, ordinal int, input, output int64) domain.PrepareCallsRequest {
	plan := llmPhysicalPlan(ordinal, 1, []int64{1, input, output, 1})
	plan.Ordinal = 1
	return domain.PrepareCallsRequest{
		RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID,
		CallRecordID: record.ID, PlanDigest: domain.SumBytes([]byte(plan.ID)), Calls: []domain.PhysicalCallPlan{plan},
		IdempotencyKey: meteringID("prepare", string(record.ID)), At: f.now,
	}
}

func assertTotalTokenProjection(t *testing.T, f meteringFixture, limit, used, reserved, remaining int64) {
	t.Helper()
	budget, err := f.store.BudgetSnapshot(context.Background(), f.runID)
	if err != nil || budget.Limits.MaxLLMTokens != limit || budget.Remaining[domain.BudgetLLMTokens] != remaining {
		t.Fatalf("BudgetSnapshot = %+v, %v", budget, err)
	}
	workbench, err := f.store.ReadWorkbenchRun(context.Background(), f.runID)
	if err != nil || workbench.BudgetUsed[domain.BudgetLLMTokens] != used || workbench.BudgetReserved[domain.BudgetLLMTokens] != reserved || workbench.Budget.Remaining[domain.BudgetLLMTokens] != remaining {
		t.Fatalf("workbench total = used:%d reserved:%d remaining:%d, %v", workbench.BudgetUsed[domain.BudgetLLMTokens], workbench.BudgetReserved[domain.BudgetLLMTokens], workbench.Budget.Remaining[domain.BudgetLLMTokens], err)
	}
}
