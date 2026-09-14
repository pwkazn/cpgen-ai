package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/similarity"
)

func TestDurableSimilarityCountsOnlyLedgerRetriesAndSettlesExactCost(t *testing.T) {
	t.Setenv("CPGEN_DURABLE_SIMILARITY_KEY", "fixture-key")
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if sends.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`{"provider_identity":"fixture","hits":[],"usage":{"input_tokens":2,"output_tokens":3,"cost_micro_usd":11}}`))
	}))
	t.Cleanup(server.Close)
	provider, request := durableSimilarityProvider(t, server.URL)
	f := newCoordinatorFixture(t, "b1", domain.BudgetLimits{MaxSimilarityCalls: 2, MaxSimilarityCostMicroUSD: 200})
	open := f.openRequest(1701)
	request.LogicalIdempotencyKey = open.LogicalOperationID
	plan, err := provider.PlanSearch(request)
	if err != nil {
		t.Fatal(err)
	}
	open.Kind, open.Provider, open.RequestDigest, open.PolicyDigest = domain.CallSimilaritySearch, plan.Provider, plan.RequestDigest, plan.PolicyDigest
	calls, err := durable.NewSimilarityCalls(f.store, provider, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	result, err := calls.Search(context.Background(), open, request)
	if err != nil || result.Value == nil || result.Value.Validate() != nil || !result.Value.CallTrace.Equal(result.CallTrace) || sends.Load() != 2 || len(result.CallTrace.PhysicalAttemptCallIDs) != 2 {
		t.Fatalf("result=%+v err=%v HTTP=%d", result, err, sends.Load())
	}
	prepared, err := f.store.LoadCall(context.Background(), open.ID)
	if err != nil {
		t.Fatal(err)
	}
	var cost, count int64
	for _, reservation := range prepared.Reservations {
		if reservation.State != domain.ReservationSettled || reservation.SettledValue == nil {
			t.Fatalf("unsettled reservation: %+v", reservation)
		}
		if reservation.Dimension == domain.BudgetSimilarityCalls {
			count += *reservation.SettledValue
		}
		if reservation.Dimension == domain.BudgetSimilarityCostMicroUSD {
			cost += *reservation.SettledValue
		}
	}
	if count != 2 || cost != 111 {
		t.Fatalf("settled count=%d cost=%d", count, cost)
	}
	_, err = calls.Search(context.Background(), open, request)
	if !errors.Is(err, durable.ErrSimilarityReplayUnavailable) || sends.Load() != 2 {
		t.Fatalf("unavailable private replay sent again: %v HTTP=%d", err, sends.Load())
	}
}

func TestDurableSimilarityRejectsOverBudgetBeforeHTTP(t *testing.T) {
	t.Setenv("CPGEN_DURABLE_SIMILARITY_KEY", "fixture-key")
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sends.Add(1); w.WriteHeader(500) }))
	t.Cleanup(server.Close)
	provider, request := durableSimilarityProvider(t, server.URL)
	f := newCoordinatorFixture(t, "b2", domain.BudgetLimits{MaxSimilarityCalls: 1, MaxSimilarityCostMicroUSD: 200})
	open := f.openRequest(1702)
	request.LogicalIdempotencyKey = open.LogicalOperationID
	plan, err := provider.PlanSearch(request)
	if err != nil {
		t.Fatal(err)
	}
	open.Kind, open.Provider, open.RequestDigest, open.PolicyDigest = domain.CallSimilaritySearch, plan.Provider, plan.RequestDigest, plan.PolicyDigest
	calls, err := durable.NewSimilarityCalls(f.store, provider, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	result, err := calls.Search(context.Background(), open, request)
	if err != nil || result.Failure == nil || result.Failure.Code != domain.FailureBudgetExhausted || sends.Load() != 0 {
		t.Fatalf("result=%+v err=%v HTTP=%d", result, err, sends.Load())
	}
}

func TestDurableSimilarityRecoveredDispatchNeverRegainsSendAuthority(t *testing.T) {
	t.Setenv("CPGEN_DURABLE_SIMILARITY_KEY", "fixture-key")
	var sends atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { sends.Add(1); w.WriteHeader(500) }))
	t.Cleanup(server.Close)
	provider, request := durableSimilarityProvider(t, server.URL)
	f := newCoordinatorFixture(t, "b3", domain.BudgetLimits{MaxSimilarityCalls: 1, MaxSimilarityCostMicroUSD: 100})
	open := f.openRequest(1703)
	open.RetryPolicy.MaxAttempts = 1
	request.LogicalIdempotencyKey = open.LogicalOperationID
	plan, err := provider.PlanSearch(request)
	if err != nil {
		t.Fatal(err)
	}
	open.Kind, open.Provider, open.RequestDigest, open.PolicyDigest = domain.CallSimilaritySearch, plan.Provider, plan.RequestDigest, plan.PolicyDigest
	ctx := context.Background()
	if _, err := f.store.OpenCall(ctx, open); err != nil {
		t.Fatal(err)
	}
	physicalID := domain.AttemptCallID("call_00000000000000000000000000001703")
	physical := domain.PhysicalCallPlan{ID: physicalID, Ordinal: 1, RetryGroup: "similarity-transport", RetryOrdinal: 1, Kind: domain.PhysicalSimilarityRequest, Provider: plan.Provider, RequestDigest: plan.RequestDigest, IdempotencyKey: coordinatorID("physical", "unknown-similarity")}
	for _, dimension := range []domain.BudgetDimension{domain.BudgetSimilarityCalls, domain.BudgetSimilarityCostMicroUSD} {
		upper := int64(100)
		if dimension == domain.BudgetSimilarityCalls {
			upper = 1
		}
		physical.Reservations = append(physical.Reservations, domain.ReservationPlan{ID: domain.ReservationID(coordinatorID("res", string(dimension))), Dimension: dimension, Subkey: "request", UpperBound: upper})
	}
	raw, err := json.Marshal([]domain.PhysicalCallPlan{physical})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.PrepareCalls(ctx, domain.PrepareCallsRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: f.attemptID, CallRecordID: open.ID, PlanDigest: domain.SumBytes(raw), Calls: []domain.PhysicalCallPlan{physical}, IdempotencyKey: coordinatorID("prepare", "unknown-similarity"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.store.BeginDispatch(ctx, domain.BeginDispatchRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: f.attemptID, CallRecordID: open.ID, AttemptCallID: physicalID, IdempotencyKey: coordinatorID("begin", "unknown-similarity"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	calls, err := durable.NewSimilarityCalls(f.store, provider, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	for replay := 0; replay < 2; replay++ {
		result, err := calls.Search(ctx, open, request)
		if err != nil || result.Failure == nil || result.Failure.Code != domain.FailureBoundaryUnknown || sends.Load() != 0 {
			t.Fatalf("result=%+v err=%v HTTP=%d", result, err, sends.Load())
		}
	}
	prepared, err := f.store.LoadCall(ctx, open.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, reservation := range prepared.Reservations {
		if reservation.State != domain.ReservationSettled || reservation.SettledValue == nil || *reservation.SettledValue != reservation.UpperBound {
			t.Fatalf("unknown boundary did not settle conservatively: %+v", reservation)
		}
	}
}

func durableSimilarityProvider(t *testing.T, endpoint string) (*similarity.HTTPAdapter, similarity.Request) {
	t.Helper()
	provider, err := similarity.New(similarity.Config{Endpoint: endpoint, APIKeyEnv: "CPGEN_DURABLE_SIMILARITY_KEY", ProviderIdentity: "fixture", ServiceIdentity: "fixture-service", AllowInsecureHTTP: true, Timeout: time.Second, MaxResponseBytes: 16384, MaxAttempts: 4})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := similarity.NewPolicy("durable/v1", .5, .8, .5, .8, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := similarity.NewPackageSafeProjection("title", "statement", []string{"graphs"}, "en")
	if err != nil {
		t.Fatal(err)
	}
	request, err := similarity.NewRequest(projection, policy.PolicyRef, policy.PolicyDigest, "durable-similarity")
	if err != nil {
		t.Fatal(err)
	}
	return provider, request
}
