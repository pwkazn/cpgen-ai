package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

type meteringFixture struct {
	store     *Store
	runID     domain.RunID
	attemptID domain.AttemptID
	stage     domain.StageName
	now       time.Time
}

func TestBudgetAccountsBackfillAndActiveTimeAuthority(t *testing.T) {
	t.Run("new run gets every immutable account", func(t *testing.T) {
		fixture := newMeteringFixture(t, "01", testCreateRunRequest(testRunID, testNow, 30*time.Second).BudgetLimits)
		got := readBudgetAccounts(t, fixture.store, fixture.runID)
		want := map[domain.BudgetDimension]int64{
			domain.BudgetLLMCalls:                 3,
			domain.BudgetLLMInputTokens:           1000,
			domain.BudgetLLMOutputTokens:          1000,
			domain.BudgetExternalCostMicroUSD:     5000,
			domain.BudgetSimilarityCalls:          2,
			domain.BudgetSimilarityCostMicroUSD:   7000,
			domain.BudgetDockerContainerCreates:   4,
			domain.BudgetArtifactPhysicalNewBytes: 1 << 20,
			domain.BudgetActiveTimeNS:             int64(30 * time.Second),
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("budget account limits = %#v, want %#v", got, want)
		}
	})

	t.Run("historical elapsed projection is preserved by M1 M2 M3", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "historical.db")
		fixture := seedHistoricalWorkflowDatabase(t, path)
		raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatalf("open historical database: %v", err)
		}
		if _, err := raw.Exec(`UPDATE runs SET active_elapsed_ns = ? WHERE run_id = ?`, int64(7*time.Second), fixture.legacyCreateRequest.RunID); err != nil {
			_ = raw.Close()
			t.Fatalf("seed historical active elapsed: %v", err)
		}
		if err := raw.Close(); err != nil {
			t.Fatalf("close historical database: %v", err)
		}
		store, err := Open(context.Background(), Config{Path: path, BusyTimeout: time.Second, MaxReaders: 2})
		if err != nil {
			t.Fatalf("upgrade historical database: %v", err)
		}
		defer store.Close()
		var limit, consumed, projected int64
		var requestDigest string
		if err := store.db.QueryRow(`
			SELECT account.limit_value, account.consumed_value, run.active_elapsed_ns, account.request_snapshot_digest
			FROM budget_accounts account JOIN runs run ON run.run_id = account.run_id
			WHERE account.run_id = ? AND account.dimension = ?`, fixture.legacyCreateRequest.RunID, domain.BudgetActiveTimeNS,
		).Scan(&limit, &consumed, &projected, &requestDigest); err != nil {
			t.Fatalf("read upgraded active account: %v", err)
		}
		if limit != int64(30*time.Second) || consumed != int64(7*time.Second) || projected != consumed {
			t.Fatalf("upgraded active account = limit:%d consumed:%d projection:%d", limit, consumed, projected)
		}
		if requestDigest != string(fixture.legacyCreateRequest.SubmittedRequestDigest) {
			t.Fatalf("request digest = %q, want %q", requestDigest, fixture.legacyCreateRequest.SubmittedRequestDigest)
		}
		var similarityCostLimit int64
		if err := store.db.QueryRow(`SELECT limit_value FROM budget_accounts
			WHERE run_id = ? AND dimension = ?`, fixture.legacyCreateRequest.RunID,
			domain.BudgetSimilarityCostMicroUSD).Scan(&similarityCostLimit); err != nil {
			t.Fatalf("read historical similarity cost account: %v", err)
		}
		if similarityCostLimit != 0 {
			t.Fatalf("historical similarity cost limit = %d, want zero/no allowance", similarityCostLimit)
		}
		assertMigrationHistory(t, store, 16)
	})

	t.Run("heartbeat replay and recovery keep account equal to projection", func(t *testing.T) {
		source := clock.NewFake(testNow)
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "active.db"), source)
		request := testCreateRunRequest(testRunID, testNow, 10*time.Second)
		mustCreateRun(t, store, request)
		mustBeginStage(t, store, testRunID, testAttemptID, 1, "prepare", domain.SumBytes([]byte("active")), testNow, meteringID("begin", "active authority"))
		commands := []domain.ActiveTimeCommand{
			{RunID: testRunID, ExpectedRunVersion: 2, Action: domain.ActiveTimeStart, IdempotencyKey: meteringID("active", "authority start"), At: source.Now()},
		}
		start := mustAccount(t, store, commands[0])
		assertActiveAuthority(t, store, start)
		source.Advance(2 * time.Second)
		heartbeat := domain.ActiveTimeCommand{RunID: testRunID, ExpectedRunVersion: 3, Action: domain.ActiveTimeHeartbeat, IdempotencyKey: meteringID("active", "authority heartbeat"), At: source.Now()}
		got := mustAccount(t, store, heartbeat)
		assertActiveAuthority(t, store, got)
		replayed := mustAccount(t, store, heartbeat)
		if !reflect.DeepEqual(replayed, got) {
			t.Fatalf("active replay = %+v, want %+v", replayed, got)
		}
		assertActiveAuthority(t, store, replayed)
		source.Advance(time.Hour)
		recovered := mustAccount(t, store, domain.ActiveTimeCommand{
			RunID: testRunID, ExpectedRunVersion: 4, Action: domain.ActiveTimeRecover,
			HeartbeatInterval: 3 * time.Second, IdempotencyKey: meteringID("active", "authority recover"), At: source.Now(),
		})
		if recovered.ActiveElapsed != 5*time.Second {
			t.Fatalf("recovered active elapsed = %v, want 5s", recovered.ActiveElapsed)
		}
		assertActiveAuthority(t, store, recovered)
	})
}

func TestBudgetReservationRaceAndPreRejection(t *testing.T) {
	for index, test := range []struct {
		dimension domain.BudgetDimension
		kind      domain.PhysicalCallKind
	}{
		{domain.BudgetLLMCalls, domain.PhysicalLLMRequest},
		{domain.BudgetLLMInputTokens, domain.PhysicalLLMRequest},
		{domain.BudgetLLMOutputTokens, domain.PhysicalLLMRequest},
		{domain.BudgetExternalCostMicroUSD, domain.PhysicalLLMRequest},
		{domain.BudgetSimilarityCalls, domain.PhysicalSimilarityRequest},
		{domain.BudgetSimilarityCostMicroUSD, domain.PhysicalSimilarityRequest},
		{domain.BudgetDockerContainerCreates, domain.PhysicalDockerContainerCreate},
		{domain.BudgetArtifactPhysicalNewBytes, domain.PhysicalLocalArtifactWrite},
	} {
		t.Run(string(test.dimension), func(t *testing.T) {
			limits := domain.BudgetLimits{}
			setRequiredDimensionLimits(&limits, test.kind, 2)
			setDimensionLimit(&limits, test.dimension, 1)
			fixture := newMeteringFixture(t, fmt.Sprintf("%02x", index+16), limits)
			callKind := domain.CallLLMGenerate
			if test.kind == domain.PhysicalSimilarityRequest {
				callKind = domain.CallSimilaritySearch
			}
			if test.kind == domain.PhysicalDockerContainerCreate {
				callKind = domain.CallSandboxRun
			}
			if test.kind == domain.PhysicalLocalArtifactWrite {
				callKind = domain.CallSandboxCompile
			}
			first := mustOpenMeteringCall(t, fixture, 1, callKind)
			second := mustOpenMeteringCall(t, fixture, 2, callKind)
			requests := []domain.PrepareCallsRequest{
				prepareOneRequest(fixture, first, 1, test.kind, test.dimension, 1),
				prepareOneRequest(fixture, second, 2, test.kind, test.dimension, 1),
			}
			var wg sync.WaitGroup
			results := make([]domain.PreparedCalls, 2)
			errs := make([]error, 2)
			for i := range requests {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					results[i], errs[i] = fixture.store.PrepareCalls(context.Background(), requests[i])
				}(i)
			}
			wg.Wait()
			prepared, rejected := 0, 0
			for i := range results {
				if errs[i] != nil {
					t.Fatalf("PrepareCalls %d: %v", i, errs[i])
				}
				if results[i].Failure == nil {
					prepared++
				} else {
					rejected++
					if results[i].CallTrace == nil || results[i].CallTrace.DispatchKind != domain.DispatchNone || len(results[i].PhysicalCalls) != 0 {
						t.Fatalf("budget rejection = %+v", results[i])
					}
				}
			}
			if prepared != 1 || rejected != 1 {
				t.Fatalf("prepared=%d rejected=%d, want one each", prepared, rejected)
			}
			account := readBudgetAccount(t, fixture.store, fixture.runID, test.dimension)
			if account.Reserved != 1 || account.Consumed != 0 || account.Remaining() != 0 {
				t.Fatalf("final unit account = %+v", account)
			}
		})
	}

	t.Run("active time final unit is charged once", func(t *testing.T) {
		source := clock.NewFake(testNow)
		store := openRuntimeStore(t, filepath.Join(t.TempDir(), "active-race.db"), source)
		mustCreateRun(t, store, testCreateRunRequest(testRunID, testNow, time.Millisecond))
		mustBeginStage(t, store, testRunID, testAttemptID, 1, "prepare", domain.SumBytes([]byte("active race")), testNow, meteringID("begin", "active race"))
		mustAccount(t, store, domain.ActiveTimeCommand{RunID: testRunID, ExpectedRunVersion: 2, Action: domain.ActiveTimeStart, IdempotencyKey: meteringID("active", "race start"), At: source.Now()})
		source.Advance(time.Millisecond)
		commands := []domain.ActiveTimeCommand{
			{RunID: testRunID, ExpectedRunVersion: 3, Action: domain.ActiveTimeHeartbeat, IdempotencyKey: meteringID("active", "race one"), At: source.Now()},
			{RunID: testRunID, ExpectedRunVersion: 3, Action: domain.ActiveTimeHeartbeat, IdempotencyKey: meteringID("active", "race two"), At: source.Now()},
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range commands {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = store.AccountActiveTime(context.Background(), commands[i])
			}(i)
		}
		wg.Wait()
		successes := 0
		for _, err := range errs {
			if err == nil {
				successes++
			} else if !errors.Is(err, ErrVersionConflict) {
				t.Fatalf("active-time race error = %v", err)
			}
		}
		if successes != 1 {
			t.Fatalf("active-time successes = %d, want 1", successes)
		}
		account := readBudgetAccount(t, store, testRunID, domain.BudgetActiveTimeNS)
		if account.Consumed != int64(time.Millisecond) || account.Remaining() != 0 {
			t.Fatalf("active account after race = %+v", account)
		}
	})
}

func TestCallRecordLogicalAndPhysicalIdempotency(t *testing.T) {
	fixture := newMeteringFixture(t, "30", testCreateRunRequest(testRunID, testNow, time.Minute).BudgetLimits)
	request := openCallRequest(fixture, 1, domain.CallLLMGenerate)
	first, err := fixture.store.OpenCall(context.Background(), request)
	if err != nil {
		t.Fatalf("OpenCall: %v", err)
	}
	replayed, err := fixture.store.OpenCall(context.Background(), request)
	if err != nil || !reflect.DeepEqual(replayed, first) {
		t.Fatalf("OpenCall replay = %+v, %v; want %+v", replayed, err, first)
	}
	drifted := request
	drifted.RequestDigest = domain.SumBytes([]byte("changed request"))
	if _, err := fixture.store.OpenCall(context.Background(), drifted); !errors.Is(err, ErrConsistency) {
		t.Fatalf("OpenCall changed replay = %v, want ErrConsistency", err)
	}

	prepare := prepareOneRequest(fixture, first, 1, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1)
	prepared, err := fixture.store.PrepareCalls(context.Background(), prepare)
	if err != nil {
		t.Fatalf("PrepareCalls: %v", err)
	}
	physicalReplay, err := fixture.store.PrepareCalls(context.Background(), prepare)
	if err != nil || !reflect.DeepEqual(physicalReplay, prepared) {
		t.Fatalf("PrepareCalls replay = %+v, %v; want %+v", physicalReplay, err, prepared)
	}
	changedBound := prepare
	changedBound.Calls = append([]domain.PhysicalCallPlan(nil), prepare.Calls...)
	changedBound.Calls[0].Reservations = append([]domain.ReservationPlan(nil), prepare.Calls[0].Reservations...)
	changedBound.Calls[0].Reservations[1].UpperBound++
	if _, err := fixture.store.PrepareCalls(context.Background(), changedBound); !errors.Is(err, ErrConsistency) {
		t.Fatalf("PrepareCalls changed bound replay = %v, want ErrConsistency", err)
	}
}

func TestDispatchSettlementStateMachineAndConservativeUsage(t *testing.T) {
	fixture := newMeteringFixture(t, "40", domain.BudgetLimits{
		MaxLLMCalls: 2, MaxLLMInputTokens: 10, MaxLLMOutputTokens: 14, MaxLLMCostMicroUSD: 22,
		MaxActiveTimeMilliseconds: 1000,
	})
	record := mustOpenMeteringCall(t, fixture, 1, domain.CallLLMGenerate)
	prepare := domain.PrepareCallsRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, PlanDigest: domain.SumBytes([]byte("two-attempt plan")),
		Calls: []domain.PhysicalCallPlan{
			llmPhysicalPlan(1, 1, []int64{1, 5, 7, 11}),
			llmPhysicalPlan(2, 2, []int64{1, 5, 7, 11}),
		},
		IdempotencyKey: meteringID("prepare", "two attempts"), At: fixture.now,
	}
	prepared, err := fixture.store.PrepareCalls(context.Background(), prepare)
	if err != nil || prepared.Failure != nil {
		t.Fatalf("PrepareCalls = %+v, %v", prepared, err)
	}
	firstGrant := mustBeginDispatch(t, fixture, record.ID, prepared.PhysicalCalls[0].ID, "begin_dispatch_one")
	if err := fixture.store.MarkSent(context.Background(), firstGrant, fixture.now.Add(time.Second)); err != nil {
		t.Fatalf("MarkSent first: %v", err)
	}
	usage := []domain.ReservationUsage{
		{ReservationID: reservationIDFor(t, prepared, firstGrant.AttemptCallID, domain.BudgetLLMCalls), Dimension: domain.BudgetLLMCalls, Subkey: "calls", Value: 1, Verified: true},
		// Input token usage is omitted, so its upper bound must be charged.
		{ReservationID: reservationIDFor(t, prepared, firstGrant.AttemptCallID, domain.BudgetLLMOutputTokens), Dimension: domain.BudgetLLMOutputTokens, Subkey: "output", Value: 99, Verified: true},
		{ReservationID: reservationIDFor(t, prepared, firstGrant.AttemptCallID, domain.BudgetExternalCostMicroUSD), Dimension: domain.BudgetExternalCostMicroUSD, Subkey: "cost", Value: 2, Verified: false},
	}
	failure := domain.PortFailure{Code: domain.FailureRateLimited, Class: domain.FailureRetryable}
	if err := fixture.store.CompletePhysical(context.Background(), domain.CompletePhysicalRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, AttemptCallID: firstGrant.AttemptCallID,
		State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeRetryableFailure, Failure: &failure,
		ProviderRequestID: "provider-one", ResponseDigest: digestPointer("retry response"), Usage: usage,
		IdempotencyKey: meteringID("complete", "dispatch one"), At: fixture.now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("CompletePhysical first: %v", err)
	}
	secondGrant := mustBeginDispatch(t, fixture, record.ID, prepared.PhysicalCalls[1].ID, "begin_dispatch_two")
	if err := fixture.store.MarkSent(context.Background(), secondGrant, fixture.now.Add(3*time.Second)); err != nil {
		t.Fatalf("MarkSent second: %v", err)
	}
	if err := fixture.store.CompletePhysical(context.Background(), domain.CompletePhysicalRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, AttemptCallID: secondGrant.AttemptCallID,
		State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess,
		ProviderRequestID: "provider-two", ResponseDigest: digestPointer("success response"),
		Usage: []domain.ReservationUsage{
			{ReservationID: reservationIDFor(t, prepared, secondGrant.AttemptCallID, domain.BudgetLLMCalls), Dimension: domain.BudgetLLMCalls, Subkey: "calls", Value: 1, Verified: true},
			{ReservationID: reservationIDFor(t, prepared, secondGrant.AttemptCallID, domain.BudgetLLMInputTokens), Dimension: domain.BudgetLLMInputTokens, Subkey: "input", Value: 2, Verified: true},
			{ReservationID: reservationIDFor(t, prepared, secondGrant.AttemptCallID, domain.BudgetLLMOutputTokens), Dimension: domain.BudgetLLMOutputTokens, Subkey: "output", Value: 3, Verified: true},
			{ReservationID: reservationIDFor(t, prepared, secondGrant.AttemptCallID, domain.BudgetExternalCostMicroUSD), Dimension: domain.BudgetExternalCostMicroUSD, Subkey: "cost", Value: 4, Verified: true},
		},
		IdempotencyKey: meteringID("complete", "dispatch two"), At: fixture.now.Add(4 * time.Second),
	}); err != nil {
		t.Fatalf("CompletePhysical second: %v", err)
	}
	trace, err := fixture.store.FinishCall(context.Background(), domain.FinishCallRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &secondGrant.AttemptCallID,
		IdempotencyKey: meteringID("finish", "dispatched call"), At: fixture.now.Add(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("FinishCall: %v", err)
	}
	wantTrace := domain.CallTrace{LogicalOperationID: record.LogicalOperationID, DispatchKind: domain.DispatchDispatched,
		PhysicalAttemptCallIDs: []domain.AttemptCallID{firstGrant.AttemptCallID, secondGrant.AttemptCallID}, ResultAttemptCallID: &secondGrant.AttemptCallID}
	if !trace.Equal(wantTrace) {
		t.Fatalf("trace = %+v, want %+v", trace, wantTrace)
	}
	assertAccountValues(t, fixture.store, fixture.runID, map[domain.BudgetDimension][2]int64{
		domain.BudgetLLMCalls:             {0, 2},
		domain.BudgetLLMInputTokens:       {0, 7},
		domain.BudgetLLMOutputTokens:      {0, 10},
		domain.BudgetExternalCostMicroUSD: {0, 15},
	})
}

// TestSettlementChargesFixedCountWhenVerifiedUsageSaysZero catches trusting a
// contradictory provider report after the request crossed the send boundary.
func TestSettlementChargesFixedCountWhenVerifiedUsageSaysZero(t *testing.T) {
	fixture := newMeteringFixture(t, "41", domain.BudgetLimits{
		MaxLLMCalls: 1, MaxLLMInputTokens: 1, MaxLLMOutputTokens: 1, MaxLLMCostMicroUSD: 1,
		MaxActiveTimeMilliseconds: 1000,
	})
	record := mustOpenMeteringCall(t, fixture, 1, domain.CallLLMGenerate)
	prepared, err := fixture.store.PrepareCalls(context.Background(), prepareOneRequest(
		fixture, record, 1, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1,
	))
	if err != nil || prepared.Failure != nil {
		t.Fatalf("PrepareCalls = %+v, %v", prepared, err)
	}
	grant := mustBeginDispatch(t, fixture, record.ID, prepared.PhysicalCalls[0].ID, "fixed_zero")
	if err := fixture.store.MarkSent(context.Background(), grant, fixture.now.Add(time.Second)); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	if err := fixture.store.CompletePhysical(context.Background(), domain.CompletePhysicalRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, AttemptCallID: grant.AttemptCallID,
		State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess,
		ProviderRequestID: "provider-zero", ResponseDigest: digestPointer("zero usage"),
		Usage: []domain.ReservationUsage{{
			ReservationID: reservationIDFor(t, prepared, grant.AttemptCallID, domain.BudgetLLMCalls),
			Dimension:     domain.BudgetLLMCalls, Subkey: "request", Value: 0, Verified: true,
		}},
		IdempotencyKey: meteringID("complete", "fixed zero"), At: fixture.now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("CompletePhysical: %v", err)
	}
	account := readBudgetAccount(t, fixture.store, fixture.runID, domain.BudgetLLMCalls)
	if account.Consumed != 1 || account.Reserved != 0 {
		t.Fatalf("fixed call account = %+v, want one consumed", account)
	}
}

func TestSimilaritySettlementUsesIndependentCostAccount(t *testing.T) {
	fixture := newMeteringFixture(t, "42", domain.BudgetLimits{
		MaxSimilarityCalls: 1, MaxSimilarityCostMicroUSD: 100, MaxLLMCostMicroUSD: 200,
		MaxActiveTimeMilliseconds: 1000,
	})
	record := mustOpenMeteringCall(t, fixture, 1, domain.CallSimilaritySearch)
	prepared, err := fixture.store.PrepareCalls(context.Background(), prepareOneRequest(
		fixture, record, 1, domain.PhysicalSimilarityRequest, domain.BudgetSimilarityCostMicroUSD, 80,
	))
	if err != nil || prepared.Failure != nil {
		t.Fatalf("PrepareCalls similarity = %+v, %v", prepared, err)
	}
	grant := mustBeginDispatch(t, fixture, record.ID, prepared.PhysicalCalls[0].ID, "similarity_cost")
	if err := fixture.store.MarkSent(context.Background(), grant, fixture.now.Add(time.Second)); err != nil {
		t.Fatalf("MarkSent: %v", err)
	}
	if err := fixture.store.CompletePhysical(context.Background(), domain.CompletePhysicalRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, AttemptCallID: grant.AttemptCallID,
		State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess,
		ProviderRequestID: "similarity-provider", ResponseDigest: digestPointer("similarity response"),
		Usage: []domain.ReservationUsage{
			{ReservationID: reservationIDFor(t, prepared, grant.AttemptCallID, domain.BudgetSimilarityCalls), Dimension: domain.BudgetSimilarityCalls, Subkey: "request", Value: 1, Verified: true},
			{ReservationID: reservationIDFor(t, prepared, grant.AttemptCallID, domain.BudgetSimilarityCostMicroUSD), Dimension: domain.BudgetSimilarityCostMicroUSD, Subkey: "cost", Value: 25, Verified: true},
		},
		IdempotencyKey: meteringID("complete", "similarity cost"), At: fixture.now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("CompletePhysical: %v", err)
	}
	similarity := readBudgetAccount(t, fixture.store, fixture.runID, domain.BudgetSimilarityCostMicroUSD)
	llm := readBudgetAccount(t, fixture.store, fixture.runID, domain.BudgetExternalCostMicroUSD)
	if similarity.Consumed != 25 || similarity.Reserved != 0 || llm.Consumed != 0 || llm.Reserved != 0 {
		t.Fatalf("cost accounts after similarity settlement: similarity=%+v llm=%+v", similarity, llm)
	}
}

func TestPhysicalCallUnknownCannotAdvance(t *testing.T) {
	fixture := newMeteringFixture(t, "50", domain.BudgetLimits{
		MaxLLMCalls: 2, MaxLLMInputTokens: 2, MaxLLMOutputTokens: 2, MaxLLMCostMicroUSD: 2,
		MaxActiveTimeMilliseconds: 1000,
	})
	record := mustOpenMeteringCall(t, fixture, 1, domain.CallLLMGenerate)
	prepare := domain.PrepareCallsRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, PlanDigest: domain.SumBytes([]byte("unknown plan")),
		Calls: []domain.PhysicalCallPlan{
			oneReservationPhysicalPlan(1, 1, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1),
			oneReservationPhysicalPlan(2, 2, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1),
		}, IdempotencyKey: meteringID("prepare", "unknown"), At: fixture.now,
	}
	prepared, err := fixture.store.PrepareCalls(context.Background(), prepare)
	if err != nil || prepared.Failure != nil {
		t.Fatalf("PrepareCalls unknown = %+v, %v", prepared, err)
	}
	grant := mustBeginDispatch(t, fixture, record.ID, prepared.PhysicalCalls[0].ID, "begin_unknown")
	unknownFailure := domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
	if err := fixture.store.CompletePhysical(context.Background(), domain.CompletePhysicalRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, AttemptCallID: grant.AttemptCallID, State: domain.PhysicalUnknown,
		Outcome: domain.PhysicalOutcomeUnknown, Failure: &unknownFailure,
		IdempotencyKey: meteringID("complete", "unknown"), At: fixture.now.Add(time.Second),
	}); err != nil {
		t.Fatalf("CompletePhysical UNKNOWN: %v", err)
	}
	_, err = fixture.store.BeginDispatch(context.Background(), domain.BeginDispatchRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, AttemptCallID: prepared.PhysicalCalls[1].ID,
		IdempotencyKey: meteringID("begin", "after unknown"), At: fixture.now.Add(2 * time.Second),
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("BeginDispatch after UNKNOWN = %v, want ErrInvalidTransition", err)
	}
	trace, err := fixture.store.FinishCall(context.Background(), domain.FinishCallRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &grant.AttemptCallID,
		Failure: &unknownFailure, IdempotencyKey: meteringID("finish", "unknown"), At: fixture.now.Add(3 * time.Second),
	})
	if err != nil || len(trace.PhysicalAttemptCallIDs) != 1 || trace.PhysicalAttemptCallIDs[0] != grant.AttemptCallID {
		t.Fatalf("FinishCall UNKNOWN = %+v, %v", trace, err)
	}
	account := readBudgetAccount(t, fixture.store, fixture.runID, domain.BudgetLLMCalls)
	if account.Reserved != 0 || account.Consumed != 1 {
		t.Fatalf("UNKNOWN settlement = %+v, want one upper bound charged and unused retry released", account)
	}
}

// TestPhysicalCallUnknownBlocksDifferentRetryGroup catches predecessor checks
// that see only the new group and therefore miss an UNKNOWN in the same logical
// call record.
func TestPhysicalCallUnknownBlocksDifferentRetryGroup(t *testing.T) {
	fixture := newMeteringFixture(t, "51", domain.BudgetLimits{
		MaxLLMCalls: 2, MaxLLMInputTokens: 2, MaxLLMOutputTokens: 2, MaxLLMCostMicroUSD: 2,
		MaxActiveTimeMilliseconds: 1000,
	})
	record := mustOpenMeteringCall(t, fixture, 1, domain.CallLLMGenerate)
	first := oneReservationPhysicalPlan(1, 1, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1)
	second := oneReservationPhysicalPlan(2, 2, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1)
	first.RetryGroup, second.RetryGroup = "primary", "secondary"
	prepared, err := fixture.store.PrepareCalls(context.Background(), domain.PrepareCallsRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, PlanDigest: domain.SumBytes([]byte("multi-group unknown")),
		Calls:          []domain.PhysicalCallPlan{first, second},
		IdempotencyKey: meteringID("prepare", "multi-group unknown"), At: fixture.now,
	})
	if err != nil || prepared.Failure != nil {
		t.Fatalf("PrepareCalls = %+v, %v", prepared, err)
	}
	grant := mustBeginDispatch(t, fixture, record.ID, first.ID, "multi_group_unknown")
	unknown := domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
	if err := fixture.store.CompletePhysical(context.Background(), domain.CompletePhysicalRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, AttemptCallID: grant.AttemptCallID, State: domain.PhysicalUnknown,
		Outcome: domain.PhysicalOutcomeUnknown, Failure: &unknown,
		IdempotencyKey: meteringID("complete", "multi-group unknown"), At: fixture.now.Add(time.Second),
	}); err != nil {
		t.Fatalf("CompletePhysical UNKNOWN: %v", err)
	}
	_, err = fixture.store.BeginDispatch(context.Background(), domain.BeginDispatchRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, AttemptCallID: second.ID,
		IdempotencyKey: meteringID("begin", "different group after unknown"), At: fixture.now.Add(2 * time.Second),
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("BeginDispatch in another group after UNKNOWN = %v, want ErrInvalidTransition", err)
	}
}

func TestCallRecordCacheHitAndNoDispatchProjection(t *testing.T) {
	fixture := newMeteringFixture(t, "60", testCreateRunRequest(testRunID, testNow, time.Minute).BudgetLimits)
	source := mustOpenMeteringCall(t, fixture, 1, domain.CallLLMGenerate)
	sourceTrace, err := fixture.store.FinishCall(context.Background(), domain.FinishCallRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: source.ID, DispatchKind: domain.DispatchNone,
		Failure:        &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected},
		IdempotencyKey: meteringID("finish", "source no dispatch"), At: fixture.now.Add(time.Second),
	})
	if err != nil || sourceTrace.DispatchKind != domain.DispatchNone {
		t.Fatalf("source FinishCall = %+v, %v", sourceTrace, err)
	}
	hit := mustOpenMeteringCall(t, fixture, 2, domain.CallCacheReuse)
	hitTrace, err := fixture.store.FinishCall(context.Background(), domain.FinishCallRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: hit.ID, DispatchKind: domain.DispatchCacheHit,
		CacheSourceCallRecordID: &source.ID, CacheHitCallRecordID: &hit.ID,
		IdempotencyKey: meteringID("finish", "cache hit"), At: fixture.now.Add(2 * time.Second),
	})
	if err != nil {
		t.Fatalf("FinishCall cache hit: %v", err)
	}
	want := domain.CallTrace{LogicalOperationID: hit.LogicalOperationID, DispatchKind: domain.DispatchCacheHit,
		CacheSourceCallRecordID: &source.ID, CacheHitCallRecordID: &hit.ID}
	if !hitTrace.Equal(want) || len(hitTrace.PhysicalAttemptCallIDs) != 0 {
		t.Fatalf("cache trace = %+v, want %+v", hitTrace, want)
	}
	var physicalCount int
	if err := fixture.store.db.QueryRow(`SELECT count(*) FROM physical_calls WHERE call_record_id = ?`, hit.ID).Scan(&physicalCount); err != nil {
		t.Fatalf("count cache physical calls: %v", err)
	}
	if physicalCount != 0 {
		t.Fatalf("cache hit physical calls = %d, want 0", physicalCount)
	}
}

// TestCallRecordCacheHitRejectsAnyPhysicalRows catches silently converting
// prepared work into hidden ABORTED rows while returning a zero-physical trace.
func TestCallRecordCacheHitRejectsAnyPhysicalRows(t *testing.T) {
	fixture := newMeteringFixture(t, "61", testCreateRunRequest(testRunID, testNow, time.Minute).BudgetLimits)
	source := mustOpenMeteringCall(t, fixture, 1, domain.CallLLMGenerate)
	if _, err := fixture.store.FinishCall(context.Background(), domain.FinishCallRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: source.ID, DispatchKind: domain.DispatchNone,
		Failure:        &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected},
		IdempotencyKey: meteringID("finish", "cache source with physical test"), At: fixture.now,
	}); err != nil {
		t.Fatalf("finish source: %v", err)
	}
	hit := mustOpenMeteringCall(t, fixture, 2, domain.CallLLMGenerate)
	if _, err := fixture.store.PrepareCalls(context.Background(), prepareOneRequest(
		fixture, hit, 2, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1,
	)); err != nil {
		t.Fatalf("prepare hit physical row: %v", err)
	}
	_, err := fixture.store.FinishCall(context.Background(), domain.FinishCallRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: hit.ID, DispatchKind: domain.DispatchCacheHit,
		CacheSourceCallRecordID: &source.ID, CacheHitCallRecordID: &hit.ID,
		IdempotencyKey: meteringID("finish", "cache hit with physical"), At: fixture.now.Add(time.Second),
	})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("FinishCall CACHE_HIT with physical rows = %v, want ErrInvalidTransition", err)
	}
	var state string
	if err := fixture.store.db.QueryRow(`SELECT state FROM physical_calls WHERE call_record_id = ?`, hit.ID).Scan(&state); err != nil {
		t.Fatalf("read retained physical row: %v", err)
	}
	if state != string(domain.PhysicalPrepared) {
		t.Fatalf("rejected CACHE_HIT changed physical state to %s", state)
	}
}

func TestCallRecordAndPhysicalCallSQLConstraintsRejectDirectAttacks(t *testing.T) {
	fixture := newMeteringFixture(t, "70", domain.BudgetLimits{
		MaxLLMCalls: 3, MaxLLMInputTokens: 3, MaxLLMOutputTokens: 3, MaxLLMCostMicroUSD: 3,
		MaxArtifactBytes: 3, MaxActiveTimeMilliseconds: 1000,
	})
	record := mustOpenMeteringCall(t, fixture, 1, domain.CallLLMGenerate)
	prepared, err := fixture.store.PrepareCalls(context.Background(), prepareOneRequest(fixture, record, 1, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1))
	if err != nil || prepared.Failure != nil {
		t.Fatalf("prepare SQL fixture = %+v, %v", prepared, err)
	}
	physical := prepared.PhysicalCalls[0]
	if _, err := fixture.store.db.Exec(`UPDATE call_records SET state = 'TERMINAL', dispatch_kind = 'DISPATCHED',
		result_attempt_call_id = ?, completed_at = ?, finish_idempotency_key = ?, finish_command_digest = ?
		WHERE call_record_id = ?`, physical.ID, formatTime(fixture.now.Add(time.Second)),
		meteringID("finish", "prepared result attack"), domain.SumBytes([]byte("prepared result attack")), record.ID); err == nil {
		t.Fatal("direct SQL accepted PREPARED result for terminal DISPATCHED call")
	}

	attacks := []struct {
		name string
		sql  string
		args []any
	}{
		{"negative account", `UPDATE budget_accounts SET consumed_value = -1 WHERE run_id = ? AND dimension = ?`, []any{fixture.runID, domain.BudgetLLMCalls}},
		{"account addition overflow", `UPDATE budget_accounts SET limit_value = 9223372036854775807, reserved_value = 1, consumed_value = 9223372036854775807 WHERE run_id = ? AND dimension = ?`, []any{fixture.runID, domain.BudgetLLMCalls}},
		{"strict integer overflow", `UPDATE budget_accounts SET limit_value = '9223372036854775808' WHERE run_id = ? AND dimension = ?`, []any{fixture.runID, domain.BudgetLLMCalls}},
		{"cross attempt physical", `UPDATE physical_calls SET attempt_id = 'attempt_ffffffffffffffffffffffffffffffff' WHERE attempt_call_id = ?`, []any{physical.ID}},
		{"terminal call matrix", `UPDATE call_records SET state = 'TERMINAL', dispatch_kind = 'DISPATCHED', completed_at = ? WHERE call_record_id = ?`, []any{formatTime(fixture.now), record.ID}},
		{"duplicate physical ordinal", `INSERT INTO physical_calls(
			attempt_call_id, call_record_id, run_id, stage_name, attempt_id, ordinal, retry_group, retry_ordinal,
			physical_kind, provider, request_digest, idempotency_key, state, prepared_at
		) SELECT 'call_ffffffffffffffffffffffffffffffff', call_record_id, run_id, stage_name, attempt_id, ordinal,
			retry_group, retry_ordinal + 1, physical_kind, provider, request_digest, 'duplicate-ordinal', state, prepared_at
			FROM physical_calls WHERE attempt_call_id = ?`, []any{physical.ID}},
		{"wrong reservation dimension", `INSERT INTO budget_reservations(
			reservation_id, run_id, stage_name, attempt_id, call_record_id, attempt_call_id,
			physical_kind, dimension, subkey, upper_bound, settled_value, state, created_at
		) VALUES ('res_ffffffffffffffffffffffffffffffff', ?, ?, ?, ?, ?, ?, ?, 'wrong', 1, NULL, 'RESERVED', ?)`,
			[]any{fixture.runID, fixture.stage, fixture.attemptID, record.ID, physical.ID, domain.PhysicalLLMRequest, domain.BudgetArtifactPhysicalNewBytes, formatTime(fixture.now)}},
	}
	for _, attack := range attacks {
		t.Run(attack.name, func(t *testing.T) {
			if _, err := fixture.store.db.Exec(attack.sql, attack.args...); err == nil {
				t.Fatalf("direct SQL accepted %s", attack.name)
			}
		})
	}

	other := mustOpenMeteringCall(t, fixture, 2, domain.CallLLMGenerate)
	otherPrepared, err := fixture.store.PrepareCalls(context.Background(), prepareOneRequest(fixture, other, 2, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1))
	if err != nil || otherPrepared.Failure != nil {
		t.Fatalf("prepare other record = %+v, %v", otherPrepared, err)
	}
	if _, err := fixture.store.db.Exec(`
		UPDATE call_records SET state = 'TERMINAL', dispatch_kind = 'DISPATCHED', result_attempt_call_id = ?, completed_at = ?
		WHERE call_record_id = ?`, otherPrepared.PhysicalCalls[0].ID, formatTime(fixture.now.Add(time.Second)), record.ID); err == nil {
		t.Fatal("direct SQL accepted result physical call outside logical record")
	}

	foreignRunID := domain.RunID("run_00000000000000000000000000000071")
	foreignAttemptID := domain.AttemptID("attempt_00000000000000000000000000000071")
	foreignCreate := testCreateRunRequest(foreignRunID, fixture.now, time.Second)
	foreignCreate.IdempotencyKey = meteringID("create", "foreign cache source")
	mustCreateRun(t, fixture.store, foreignCreate)
	mustBeginStage(t, fixture.store, foreignRunID, foreignAttemptID, 1, fixture.stage,
		domain.SumBytes([]byte("foreign cache source")), fixture.now, meteringID("begin", "foreign cache source"))
	foreignRequest := openCallRequest(fixture, 4, domain.CallLLMGenerate)
	foreignRequest.RunID, foreignRequest.AttemptID = foreignRunID, foreignAttemptID
	foreignSource, err := fixture.store.OpenCall(context.Background(), foreignRequest)
	if err != nil {
		t.Fatalf("open foreign cache source: %v", err)
	}
	if _, err := fixture.store.FinishCall(context.Background(), domain.FinishCallRequest{
		RunID: foreignRunID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: foreignAttemptID,
		CallRecordID: foreignSource.ID, DispatchKind: domain.DispatchNone,
		Failure:        &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected},
		IdempotencyKey: meteringID("finish", "foreign cache source"), At: fixture.now,
	}); err != nil {
		t.Fatalf("finish foreign cache source: %v", err)
	}
	cacheHit := mustOpenMeteringCall(t, fixture, 3, domain.CallCacheReuse)
	if _, err := fixture.store.db.Exec(`
		UPDATE call_records SET state = 'TERMINAL', dispatch_kind = 'CACHE_HIT',
			cache_source_call_record_id = ?, cache_hit_call_record_id = call_record_id,
			completed_at = ?, finish_idempotency_key = ?, finish_command_digest = ?
		WHERE call_record_id = ?`,
		foreignSource.ID, formatTime(fixture.now.Add(2*time.Second)), meteringID("finish", "cross-run cache hit"),
		domain.SumBytes([]byte("cross-run cache hit")), cacheHit.ID,
	); err == nil {
		t.Fatal("direct SQL accepted CACHE_HIT source from another run")
	}
	localSource := mustOpenMeteringCall(t, fixture, 5, domain.CallLLMGenerate)
	if _, err := fixture.store.FinishCall(context.Background(), domain.FinishCallRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: localSource.ID, DispatchKind: domain.DispatchNone,
		Failure:        &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected},
		IdempotencyKey: meteringID("finish", "local SQL cache source"), At: fixture.now.Add(2 * time.Second),
	}); err != nil {
		t.Fatalf("finish local cache source: %v", err)
	}
	if _, err := fixture.store.db.Exec(`UPDATE call_records SET state = 'TERMINAL', dispatch_kind = 'CACHE_HIT',
		result_attempt_call_id = NULL, cache_source_call_record_id = ?, cache_hit_call_record_id = call_record_id,
		failure_code = NULL, failure_class = NULL, failure_json = NULL, completed_at = ?,
		finish_idempotency_key = ?, finish_command_digest = ? WHERE call_record_id = ?`,
		localSource.ID, formatTime(fixture.now.Add(3*time.Second)), meteringID("finish", "cache row SQL attack"),
		domain.SumBytes([]byte("cache row SQL attack")), record.ID); err == nil {
		t.Fatal("direct SQL accepted CACHE_HIT with an existing physical row")
	}
}

// TestTerminalCallInsertCannotDeferPreparedPhysicalResult guards the deferred
// result foreign key: a terminal logical DISPATCHED call must not be able to
// point at a PREPARED physical row inserted later in the same transaction.
func TestTerminalCallInsertCannotDeferPreparedPhysicalResult(t *testing.T) {
	fixture := newMeteringFixture(t, "91", domain.BudgetLimits{
		MaxLLMCalls: 3, MaxLLMInputTokens: 3, MaxLLMOutputTokens: 3, MaxLLMCostMicroUSD: 3,
		MaxArtifactBytes: 3, MaxActiveTimeMilliseconds: 1000,
	})
	record := mustOpenMeteringCall(t, fixture, 1, domain.CallLLMGenerate)
	physicalID := domain.AttemptCallID("call_00000000000000000000000000000091")

	tx, err := fixture.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin direct SQL transaction: %v", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM call_records WHERE call_record_id = ?`, record.ID); err != nil {
		t.Fatalf("replace open call record: %v", err)
	}
	if _, err := tx.Exec(`INSERT INTO call_records(
		call_record_id, run_id, stage_name, attempt_id, logical_operation_id, call_kind, provider,
		request_digest, policy_digest, retry_max_attempts, retry_initial_backoff_ns, retry_max_backoff_ns,
		retry_jitter_seed_digest, state, dispatch_kind, result_attempt_call_id,
		opened_at, completed_at, open_idempotency_key, open_command_digest, finish_idempotency_key, finish_command_digest
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'TERMINAL', 'DISPATCHED', ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.RunID, record.StageName, record.AttemptID, record.LogicalOperationID, record.Kind, record.Provider,
		record.RequestDigest, record.PolicyDigest, record.RetryPolicy.MaxAttempts,
		int64(record.RetryPolicy.InitialBackoff), int64(record.RetryPolicy.MaxBackoff), record.RetryPolicy.JitterSeedDigest,
		physicalID, formatTime(record.OpenedAt), formatTime(fixture.now.Add(time.Second)), record.IdempotencyKey,
		domain.SumBytes([]byte("terminal insert open")), "finish_terminal_insert", domain.SumBytes([]byte("terminal insert finish")),
	); err != nil {
		if !strings.Contains(err.Error(), "DISPATCHED result physical call is not terminal") {
			t.Fatalf("insert terminal logical call: %v", err)
		}
		if err := tx.Rollback(); err != nil {
			t.Fatalf("rollback rejected terminal logical call: %v", err)
		}
	} else {
		if _, err := tx.Exec(`INSERT INTO physical_calls(
			attempt_call_id, call_record_id, run_id, stage_name, attempt_id, ordinal, retry_group, retry_ordinal,
			physical_kind, provider, request_digest, idempotency_key, state, prepared_at
		) VALUES (?, ?, ?, ?, ?, 1, 'request', 1, 'LLM_REQUEST', ?, ?, ?, 'PREPARED', ?)`,
			physicalID, record.ID, record.RunID, record.StageName, record.AttemptID, record.Provider,
			domain.SumBytes([]byte("terminal insert physical")), "physical_terminal_insert", formatTime(record.OpenedAt),
		); err != nil {
			t.Fatalf("insert prepared physical result: %v", err)
		}
		if err := tx.Commit(); err == nil {
			t.Fatal("committed terminal logical call with a PREPARED physical result")
		}
	}

	// The reverse order is the normal direct-SQL shape: the physical row exists
	// first, but a nonterminal result must still be rejected when the logical
	// call is moved to its terminal projection.
	physicalID = domain.AttemptCallID("call_00000000000000000000000000000092")
	reverseTx, err := fixture.store.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin reverse-order SQL transaction: %v", err)
	}
	defer reverseTx.Rollback()
	if _, err := reverseTx.Exec(`INSERT INTO physical_calls(
		attempt_call_id, call_record_id, run_id, stage_name, attempt_id, ordinal, retry_group, retry_ordinal,
		physical_kind, provider, request_digest, idempotency_key, state, prepared_at
	) VALUES (?, ?, ?, ?, ?, 1, 'request', 1, 'LLM_REQUEST', ?, ?, ?, 'PREPARED', ?)`,
		physicalID, record.ID, record.RunID, record.StageName, record.AttemptID, record.Provider,
		domain.SumBytes([]byte("terminal insert reverse physical")), "physical_terminal_insert_reverse", formatTime(record.OpenedAt),
	); err != nil {
		t.Fatalf("insert reverse-order prepared physical result: %v", err)
	}
	if _, err := reverseTx.Exec(`UPDATE call_records SET state = 'TERMINAL', dispatch_kind = 'DISPATCHED',
		result_attempt_call_id = ?, completed_at = ?, finish_idempotency_key = ?, finish_command_digest = ?
		WHERE call_record_id = ?`, physicalID, formatTime(fixture.now.Add(time.Second)),
		"finish_terminal_insert_reverse", domain.SumBytes([]byte("terminal insert reverse finish")), record.ID); err == nil {
		_ = reverseTx.Rollback()
		t.Fatal("accepted terminal logical call with a PREPARED physical result")
	}
	if err := reverseTx.Rollback(); err != nil {
		t.Fatalf("rollback reverse-order terminal transition: %v", err)
	}
}

// TestBudgetAccountSQLBindsImmutableRunLimit catches direct SQL inflation and
// delete/reinsert remapping of a dimension to a different immutable run limit.
func TestBudgetAccountSQLBindsImmutableRunLimit(t *testing.T) {
	limits := domain.BudgetLimits{
		MaxLLMCalls: 3, MaxSimilarityCalls: 7, MaxLLMInputTokens: 11, MaxLLMOutputTokens: 13,
		MaxLLMCostMicroUSD: 17, MaxSimilarityCostMicroUSD: 19, MaxSandboxCreates: 23,
		MaxArtifactBytes: 29, MaxActiveTimeMilliseconds: 1000,
	}
	for index, attack := range []func(*Store, domain.RunID) error{
		func(store *Store, runID domain.RunID) error {
			_, err := store.db.Exec(`UPDATE runs SET max_llm_calls = max_llm_calls + 1 WHERE run_id = ?`, runID)
			return err
		},
		func(store *Store, runID domain.RunID) error {
			_, err := store.db.Exec(`UPDATE budget_accounts SET limit_value = limit_value + 1
				WHERE run_id = ? AND dimension = ?`, runID, domain.BudgetLLMCalls)
			return err
		},
		func(store *Store, runID domain.RunID) error {
			_, err := store.db.Exec(`UPDATE budget_accounts SET limit_value =
				(SELECT max_similarity_calls FROM runs WHERE run_id = ?)
				WHERE run_id = ? AND dimension = ?`, runID, runID, domain.BudgetLLMCalls)
			return err
		},
		func(store *Store, runID domain.RunID) error {
			tx, err := store.db.Begin()
			if err != nil {
				return err
			}
			defer tx.Rollback()
			if _, err = tx.Exec(`DELETE FROM budget_accounts WHERE run_id = ? AND dimension = ?`, runID, domain.BudgetLLMCalls); err != nil {
				return err
			}
			_, err = tx.Exec(`INSERT INTO budget_accounts(
				run_id, request_snapshot_digest, dimension, limit_value, reserved_value, consumed_value, account_version
			) SELECT run_id, submitted_request_digest, ?, max_similarity_calls, 0, 0, 1 FROM runs WHERE run_id = ?`,
				domain.BudgetLLMCalls, runID)
			return err
		},
	} {
		fixture := newMeteringFixture(t, fmt.Sprintf("8%d", index), limits)
		if err := attack(fixture.store, fixture.runID); err == nil {
			t.Fatalf("direct SQL budget binding attack %d succeeded", index)
		}
	}
}

func newMeteringFixture(t *testing.T, suffix string, limits domain.BudgetLimits) meteringFixture {
	t.Helper()
	runID := domain.RunID("run_000000000000000000000000000000" + suffix)
	attemptID := domain.AttemptID("attempt_000000000000000000000000000000" + suffix)
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "metering.db"), clock.NewFake(testNow))
	request := testCreateRunRequest(runID, testNow, time.Duration(limits.MaxActiveTimeMilliseconds)*time.Millisecond)
	request.BudgetLimits = limits
	request.SubmittedRequestJSON = canonicalSQLiteRunRequestJSON(limits)
	request.SubmittedRequestDigest = domain.SumBytes(request.SubmittedRequestJSON)
	mustCreateRun(t, store, request)
	mustBeginStage(t, store, runID, attemptID, 1, "prepare", domain.SumBytes([]byte("metering input "+suffix)), testNow, meteringID("begin", "metering "+suffix))
	return meteringFixture{store: store, runID: runID, attemptID: attemptID, stage: "prepare", now: testNow}
}

func openCallRequest(fixture meteringFixture, ordinal int, kind domain.CallKind) domain.OpenCallRequest {
	suffix := fmt.Sprintf("%032x", ordinal)
	return domain.OpenCallRequest{
		ID: domain.CallRecordID("callrec_" + suffix), RunID: fixture.runID, ExpectedRunVersion: 2,
		StageName: fixture.stage, AttemptID: fixture.attemptID,
		LogicalOperationID: fmt.Sprintf("logical-%d", ordinal), Kind: kind, Provider: providerForCall(kind),
		RequestDigest: domain.SumBytes([]byte(fmt.Sprintf("request-%d", ordinal))),
		PolicyDigest:  domain.SumBytes([]byte("policy")), RetryPolicy: domain.RetryPolicy{
			MaxAttempts: 4, InitialBackoff: time.Millisecond, MaxBackoff: time.Second,
			JitterSeedDigest: domain.SumBytes([]byte("jitter")),
		}, IdempotencyKey: "open_" + suffix, At: fixture.now,
	}
}

func mustOpenMeteringCall(t *testing.T, fixture meteringFixture, ordinal int, kind domain.CallKind) domain.CallRecord {
	t.Helper()
	record, err := fixture.store.OpenCall(context.Background(), openCallRequest(fixture, ordinal, kind))
	if err != nil {
		t.Fatalf("OpenCall(%d): %v", ordinal, err)
	}
	return record
}

func prepareOneRequest(fixture meteringFixture, record domain.CallRecord, ordinal int, kind domain.PhysicalCallKind, dimension domain.BudgetDimension, upper int64) domain.PrepareCallsRequest {
	return domain.PrepareCallsRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: record.ID, PlanDigest: domain.SumBytes([]byte(fmt.Sprintf("plan-%d", ordinal))),
		Calls:          []domain.PhysicalCallPlan{oneReservationPhysicalPlan(ordinal, 1, kind, dimension, upper)},
		IdempotencyKey: "prepare_" + fmt.Sprintf("%032x", ordinal), At: fixture.now,
	}
}

func oneReservationPhysicalPlan(ordinal, retryOrdinal int, kind domain.PhysicalCallKind, dimension domain.BudgetDimension, upper int64) domain.PhysicalCallPlan {
	suffix := fmt.Sprintf("%032x", ordinal)
	dimensions := requiredTestDimensions(kind)
	reservations := make([]domain.ReservationPlan, len(dimensions))
	for index, item := range dimensions {
		bound := int64(1)
		if item == dimension {
			bound = upper
		}
		reservations[index] = domain.ReservationPlan{
			ID: domain.ReservationID(fmt.Sprintf("res_%032x", ordinal*16+index)), Dimension: item,
			Subkey: testDimensionSubkey(item), UpperBound: bound,
		}
	}
	return domain.PhysicalCallPlan{
		ID: domain.AttemptCallID("call_" + suffix), Ordinal: int64(retryOrdinal), RetryGroup: "request", RetryOrdinal: int64(retryOrdinal),
		Kind: kind, Provider: providerForPhysical(kind), RequestDigest: domain.SumBytes([]byte("physical-" + suffix)),
		IdempotencyKey: "physical_" + suffix,
		Reservations:   reservations,
	}
}

func requiredTestDimensions(kind domain.PhysicalCallKind) []domain.BudgetDimension {
	switch kind {
	case domain.PhysicalLLMRequest:
		return []domain.BudgetDimension{domain.BudgetLLMCalls, domain.BudgetLLMInputTokens, domain.BudgetLLMOutputTokens, domain.BudgetExternalCostMicroUSD}
	case domain.PhysicalSimilarityRequest:
		return []domain.BudgetDimension{domain.BudgetSimilarityCalls, domain.BudgetSimilarityCostMicroUSD}
	case domain.PhysicalDockerContainerCreate:
		return []domain.BudgetDimension{domain.BudgetDockerContainerCreates}
	case domain.PhysicalLocalArtifactWrite:
		return []domain.BudgetDimension{domain.BudgetArtifactPhysicalNewBytes}
	default:
		return nil
	}
}

func testDimensionSubkey(dimension domain.BudgetDimension) string {
	switch dimension {
	case domain.BudgetLLMCalls, domain.BudgetSimilarityCalls:
		return "request"
	case domain.BudgetLLMInputTokens:
		return "input"
	case domain.BudgetLLMOutputTokens:
		return "output"
	case domain.BudgetExternalCostMicroUSD, domain.BudgetSimilarityCostMicroUSD:
		return "cost"
	case domain.BudgetDockerContainerCreates:
		return "create"
	default:
		return "bytes"
	}
}

func llmPhysicalPlan(ordinal, retryOrdinal int, upper []int64) domain.PhysicalCallPlan {
	suffix := fmt.Sprintf("%032x", ordinal)
	dimensions := []struct {
		dimension domain.BudgetDimension
		subkey    string
	}{{domain.BudgetLLMCalls, "calls"}, {domain.BudgetLLMInputTokens, "input"}, {domain.BudgetLLMOutputTokens, "output"}, {domain.BudgetExternalCostMicroUSD, "cost"}}
	reservations := make([]domain.ReservationPlan, len(dimensions))
	for index, item := range dimensions {
		reservationSuffix := fmt.Sprintf("%032x", ordinal*16+index)
		reservations[index] = domain.ReservationPlan{ID: domain.ReservationID("res_" + reservationSuffix), Dimension: item.dimension, Subkey: item.subkey, UpperBound: upper[index]}
	}
	return domain.PhysicalCallPlan{
		ID: domain.AttemptCallID("call_" + suffix), Ordinal: int64(ordinal), RetryGroup: "provider-request", RetryOrdinal: int64(retryOrdinal),
		Kind: domain.PhysicalLLMRequest, Provider: "fake-llm", RequestDigest: domain.SumBytes([]byte("llm-" + suffix)),
		IdempotencyKey: "llmphysical_" + suffix, Reservations: reservations,
	}
}

func mustBeginDispatch(t *testing.T, fixture meteringFixture, recordID domain.CallRecordID, callID domain.AttemptCallID, key string) domain.DispatchGrant {
	t.Helper()
	grant, err := fixture.store.BeginDispatch(context.Background(), domain.BeginDispatchRequest{
		RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID,
		CallRecordID: recordID, AttemptCallID: callID, IdempotencyKey: meteringID("begin", key), At: fixture.now,
	})
	if err != nil {
		t.Fatalf("BeginDispatch(%s): %v", callID, err)
	}
	return grant
}

func readBudgetAccounts(t *testing.T, store *Store, runID domain.RunID) map[domain.BudgetDimension]int64 {
	t.Helper()
	rows, err := store.db.Query(`SELECT dimension, limit_value FROM budget_accounts WHERE run_id = ? ORDER BY dimension`, runID)
	if err != nil {
		t.Fatalf("query budget accounts: %v", err)
	}
	defer rows.Close()
	result := make(map[domain.BudgetDimension]int64)
	for rows.Next() {
		var dimension domain.BudgetDimension
		var limit int64
		if err := rows.Scan(&dimension, &limit); err != nil {
			t.Fatalf("scan budget account: %v", err)
		}
		result[dimension] = limit
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate budget accounts: %v", err)
	}
	return result
}

func readBudgetAccount(t *testing.T, store *Store, runID domain.RunID, dimension domain.BudgetDimension) domain.BudgetAccount {
	t.Helper()
	var account domain.BudgetAccount
	var digest string
	if err := store.db.QueryRow(`SELECT request_snapshot_digest, limit_value, reserved_value, consumed_value, account_version
		FROM budget_accounts WHERE run_id = ? AND dimension = ?`, runID, dimension,
	).Scan(&digest, &account.Limit, &account.Reserved, &account.Consumed, &account.Version); err != nil {
		t.Fatalf("read %s account: %v", dimension, err)
	}
	account.RunID, account.RequestSnapshotDigest, account.Dimension = runID, domain.Digest(digest), dimension
	if err := account.Validate(); err != nil {
		t.Fatalf("stored %s account invalid: %v", dimension, err)
	}
	return account
}

func assertActiveAuthority(t *testing.T, store *Store, result domain.ActiveTimeResult) {
	t.Helper()
	account := readBudgetAccount(t, store, result.RunID, domain.BudgetActiveTimeNS)
	var projected int64
	if err := store.db.QueryRow(`SELECT active_elapsed_ns FROM runs WHERE run_id = ?`, result.RunID).Scan(&projected); err != nil {
		t.Fatalf("read active projection: %v", err)
	}
	if account.Consumed != projected || projected != int64(result.ActiveElapsed) || account.Remaining() != int64(result.Remaining) {
		t.Fatalf("active authority mismatch: account=%+v projection=%d result=%+v", account, projected, result)
	}
}

func assertAccountValues(t *testing.T, store *Store, runID domain.RunID, want map[domain.BudgetDimension][2]int64) {
	t.Helper()
	for dimension, values := range want {
		account := readBudgetAccount(t, store, runID, dimension)
		if account.Reserved != values[0] || account.Consumed != values[1] {
			t.Fatalf("%s account = reserved:%d consumed:%d, want reserved:%d consumed:%d", dimension, account.Reserved, account.Consumed, values[0], values[1])
		}
	}
}

func setDimensionLimit(limits *domain.BudgetLimits, dimension domain.BudgetDimension, limit int64) {
	switch dimension {
	case domain.BudgetLLMCalls:
		limits.MaxLLMCalls = limit
	case domain.BudgetLLMInputTokens:
		limits.MaxLLMInputTokens = limit
	case domain.BudgetLLMOutputTokens:
		limits.MaxLLMOutputTokens = limit
	case domain.BudgetExternalCostMicroUSD:
		limits.MaxLLMCostMicroUSD = limit
	case domain.BudgetSimilarityCalls:
		limits.MaxSimilarityCalls = limit
	case domain.BudgetSimilarityCostMicroUSD:
		limits.MaxSimilarityCostMicroUSD = limit
	case domain.BudgetDockerContainerCreates:
		limits.MaxSandboxCreates = limit
	case domain.BudgetArtifactPhysicalNewBytes:
		limits.MaxArtifactBytes = limit
	case domain.BudgetActiveTimeNS:
		limits.MaxActiveTimeMilliseconds = limit / int64(time.Millisecond)
	}
}

func setRequiredDimensionLimits(limits *domain.BudgetLimits, kind domain.PhysicalCallKind, limit int64) {
	for _, dimension := range requiredTestDimensions(kind) {
		setDimensionLimit(limits, dimension, limit)
	}
}

func providerForCall(kind domain.CallKind) string {
	switch kind {
	case domain.CallLLMGenerate:
		return "fake-llm"
	case domain.CallSimilaritySearch:
		return "fake-similarity"
	case domain.CallCacheReuse:
		return "local-cache"
	default:
		return "local-docker"
	}
}

func providerForPhysical(kind domain.PhysicalCallKind) string {
	switch kind {
	case domain.PhysicalLLMRequest:
		return "fake-llm"
	case domain.PhysicalSimilarityRequest:
		return "fake-similarity"
	case domain.PhysicalLocalArtifactWrite:
		return "local-artifact"
	default:
		return "local-docker"
	}
}

func digestPointer(value string) *domain.Digest {
	digest := domain.SumBytes([]byte(value))
	return &digest
}

func meteringID(prefix, material string) string {
	digest := string(domain.SumBytes([]byte(material)))
	return prefix + "_" + digest[7:39]
}

func reservationIDFor(t *testing.T, prepared domain.PreparedCalls, callID domain.AttemptCallID, dimension domain.BudgetDimension) domain.ReservationID {
	t.Helper()
	for _, reservation := range prepared.Reservations {
		if reservation.AttemptCallID == callID && reservation.Dimension == dimension {
			return reservation.ID
		}
	}
	t.Fatalf("reservation for call %q and dimension %q not found", callID, dimension)
	return ""
}
