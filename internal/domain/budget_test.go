package domain_test

import (
	"fmt"
	"math"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestBudgetDimensionsAndAccountsRejectInvalidValues(t *testing.T) {
	t.Parallel()
	dimensions := []domain.BudgetDimension{
		domain.BudgetLLMCalls,
		domain.BudgetLLMInputTokens,
		domain.BudgetLLMOutputTokens,
		domain.BudgetExternalCostMicroUSD,
		domain.BudgetSimilarityCalls,
		domain.BudgetSimilarityCostMicroUSD,
		domain.BudgetDockerContainerCreates,
		domain.BudgetArtifactPhysicalNewBytes,
		domain.BudgetActiveTimeNS,
	}
	if len(dimensions) != 9 {
		t.Fatalf("dimension count = %d, want 9", len(dimensions))
	}
	for _, dimension := range dimensions {
		if !dimension.Valid() {
			t.Fatalf("dimension %q is invalid", dimension)
		}
		account := domain.BudgetAccount{
			RunID:                 "run_00000000000000000000000000000001",
			RequestSnapshotDigest: domain.SumBytes([]byte("request")),
			Dimension:             dimension, Limit: 10, Reserved: 3, Consumed: 4, Version: 2,
		}
		if err := account.Validate(); err != nil {
			t.Fatalf("valid %s account: %v", dimension, err)
		}
		if got := account.Remaining(); got != 3 {
			t.Fatalf("%s remaining = %d, want 3", dimension, got)
		}
	}

	invalid := domain.BudgetAccount{
		RunID:                 "run_00000000000000000000000000000001",
		RequestSnapshotDigest: domain.SumBytes([]byte("request")),
		Dimension:             domain.BudgetLLMCalls, Limit: math.MaxInt64, Reserved: 1, Consumed: math.MaxInt64, Version: 1,
	}
	if err := invalid.Validate(); err == nil {
		t.Fatal("overflowing reserved plus consumed account accepted")
	}
	invalid.Reserved, invalid.Consumed = -1, 0
	if err := invalid.Validate(); err == nil {
		t.Fatal("negative account value accepted")
	}
}

// TestSimilarityCostHasIndependentBudgetDimension catches charging a
// similarity provider against the LLM price ceiling. The two services must be
// independently disableable and independently exhausted.
func TestSimilarityCostHasIndependentBudgetDimension(t *testing.T) {
	t.Parallel()
	if !domain.BudgetSimilarityCostMicroUSD.Valid() {
		t.Fatal("similarity cost budget dimension is invalid")
	}
	plan := domain.PhysicalCallPlan{
		ID: "call_00000000000000000000000000000010", Ordinal: 1,
		RetryGroup: "similarity-request", RetryOrdinal: 1,
		Kind: domain.PhysicalSimilarityRequest, Provider: "fake-similarity",
		RequestDigest:  domain.SumBytes([]byte("similarity bundle")),
		IdempotencyKey: "physical_00000000000000000000000000000010",
		Reservations: []domain.ReservationPlan{
			{ID: "res_00000000000000000000000000000010", Dimension: domain.BudgetSimilarityCalls, Subkey: "request", UpperBound: 1},
			{ID: "res_00000000000000000000000000000020", Dimension: domain.BudgetSimilarityCostMicroUSD, Subkey: "cost", UpperBound: 25},
		},
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("complete similarity reservation bundle: %v", err)
	}
	plan.Reservations[1].Dimension = domain.BudgetExternalCostMicroUSD
	if err := plan.Validate(); err == nil {
		t.Fatal("similarity plan borrowed the LLM cost account")
	}
}

func TestCallRecordPhysicalCallAndReservationMatrices(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 8, 0, 0, 0, time.UTC)
	digest := domain.SumBytes([]byte("binding"))
	record := domain.CallRecord{
		ID: "callrec_00000000000000000000000000000001", RunID: "run_00000000000000000000000000000001",
		StageName: "prepare", AttemptID: "attempt_00000000000000000000000000000001",
		LogicalOperationID: "generate-statement", Kind: domain.CallLLMGenerate, Provider: "fake-llm",
		RequestDigest: digest, PolicyDigest: digest, RetryPolicy: domain.RetryPolicy{
			MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Second, JitterSeedDigest: digest,
		}, IdempotencyKey: "logical_00000000000000000000000000000001",
		State: domain.CallRecordOpen, OpenedAt: now,
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("valid open call record: %v", err)
	}

	terminal := record
	terminal.State = domain.CallRecordTerminal
	terminal.DispatchKind = pointer(domain.DispatchDispatched)
	terminal.CompletedAt = pointer(now.Add(time.Second))
	if err := terminal.Validate(); err == nil {
		t.Fatal("DISPATCHED call without result physical call accepted")
	}

	physical := domain.PhysicalCall{
		ID: callA, CallRecordID: record.ID, RunID: record.RunID, StageName: record.StageName, AttemptID: record.AttemptID,
		Ordinal: 1, RetryGroup: "provider-request", RetryOrdinal: 1,
		Kind: domain.PhysicalLLMRequest, Provider: "fake-llm", RequestDigest: digest,
		IdempotencyKey: "physical_00000000000000000000000000000001", State: domain.PhysicalPrepared, PreparedAt: now,
	}
	if err := physical.Validate(); err != nil {
		t.Fatalf("valid prepared physical call: %v", err)
	}
	physical.State = domain.PhysicalCompleted
	if err := physical.Validate(); err == nil {
		t.Fatal("completed physical call without send/outcome fields accepted")
	}

	reservation := domain.BudgetReservation{
		ID: "res_00000000000000000000000000000001", RunID: record.RunID, StageName: record.StageName,
		AttemptID: record.AttemptID, CallRecordID: record.ID, AttemptCallID: physical.ID,
		Dimension: domain.BudgetLLMCalls, Subkey: "request", UpperBound: 1,
		State: domain.ReservationReserved, CreatedAt: now,
	}
	if err := reservation.Validate(); err != nil {
		t.Fatalf("valid reservation: %v", err)
	}
	reservation.Dimension = domain.BudgetArtifactPhysicalNewBytes
	if err := reservation.ValidateFor(domain.PhysicalLLMRequest); err == nil {
		t.Fatal("artifact-byte reservation accepted for LLM physical call")
	}
}

// TestPhysicalCallPlanRequiresExactReservationBundle catches dispatch plans
// that omit a required budget account or split one account across duplicate
// subkeys. Such plans would authorize external work without reserving its full
// worst-case cost.
func TestPhysicalCallPlanRequiresExactReservationBundle(t *testing.T) {
	t.Parallel()
	base := domain.PhysicalCallPlan{
		ID: "call_00000000000000000000000000000011", Ordinal: 1,
		RetryGroup: "provider-request", RetryOrdinal: 1,
		Kind: domain.PhysicalLLMRequest, Provider: "fake-llm",
		RequestDigest:  domain.SumBytes([]byte("complete bundle")),
		IdempotencyKey: "physical_00000000000000000000000000000011",
		Reservations: []domain.ReservationPlan{
			{ID: "res_00000000000000000000000000000011", Dimension: domain.BudgetLLMCalls, Subkey: "request", UpperBound: 1},
			{ID: "res_00000000000000000000000000000012", Dimension: domain.BudgetLLMInputTokens, Subkey: "input", UpperBound: 10},
			{ID: "res_00000000000000000000000000000013", Dimension: domain.BudgetLLMOutputTokens, Subkey: "output", UpperBound: 20},
			{ID: "res_00000000000000000000000000000014", Dimension: domain.BudgetExternalCostMicroUSD, Subkey: "cost", UpperBound: 30},
		},
	}
	if err := base.Validate(); err != nil {
		t.Fatalf("complete LLM reservation bundle: %v", err)
	}

	missing := base
	missing.Reservations = append([]domain.ReservationPlan(nil), base.Reservations[1:]...)
	if err := missing.Validate(); err == nil {
		t.Fatal("LLM plan without the fixed call reservation was accepted")
	}

	duplicate := base
	duplicate.Reservations = append(append([]domain.ReservationPlan(nil), base.Reservations...), domain.ReservationPlan{
		ID: "res_00000000000000000000000000000015", Dimension: domain.BudgetLLMInputTokens,
		Subkey: "second-input-counter", UpperBound: 1,
	})
	if err := duplicate.Validate(); err == nil {
		t.Fatal("LLM plan with a duplicate budget dimension was accepted")
	}

	create := base
	create.Kind, create.Provider = domain.PhysicalDockerContainerCreate, "local-docker"
	create.Reservations = []domain.ReservationPlan{{
		ID: "res_00000000000000000000000000000016", Dimension: domain.BudgetDockerContainerCreates,
		Subkey: "create", UpperBound: 2,
	}}
	if err := create.Validate(); err == nil {
		t.Fatal("Docker create plan with a non-unit fixed-count reservation was accepted")
	}

	ping := create
	ping.Kind = domain.PhysicalDockerEnginePing
	ping.Reservations = nil
	if err := ping.Validate(); err != nil {
		t.Fatalf("Docker ping with no reservation: %v", err)
	}
}

func TestDispatchRequestsAndConservativeUsageValidation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 5, 8, 0, 0, 0, time.UTC)
	digest := domain.SumBytes([]byte("dispatch"))
	request := domain.CompletePhysicalRequest{
		RunID: "run_00000000000000000000000000000001", ExpectedRunVersion: 2,
		StageName: "prepare", AttemptID: "attempt_00000000000000000000000000000001",
		CallRecordID: "callrec_00000000000000000000000000000001", AttemptCallID: callA,
		State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess,
		ProviderRequestID: "provider-request-1", ResponseDigest: pointer(digest),
		Usage:          []domain.ReservationUsage{{ReservationID: "res_00000000000000000000000000000001", Dimension: domain.BudgetLLMCalls, Subkey: "request", Value: 1, Verified: true}},
		IdempotencyKey: "complete_00000000000000000000000000000001", At: now,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("valid completion: %v", err)
	}
	request.Usage[0].Value = -1
	if err := request.Validate(); err != nil {
		t.Fatalf("contradictory provider usage must remain representable for conservative settlement: %v", err)
	}

	unknown := request
	unknown.State = domain.PhysicalUnknown
	unknown.Outcome = domain.PhysicalOutcomeUnknown
	unknown.ProviderRequestID = ""
	unknown.ResponseDigest = nil
	unknown.Usage = nil
	unknown.Failure = &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
	if err := unknown.Validate(); err != nil {
		t.Fatalf("valid unknown-boundary completion: %v", err)
	}
}

func TestRetryPolicyIsBoundedAndPersistable(t *testing.T) {
	t.Parallel()
	policy := domain.RetryPolicy{
		MaxAttempts: 3, InitialBackoff: 100 * time.Millisecond, MaxBackoff: time.Second,
		JitterSeedDigest: domain.SumBytes([]byte("jitter-seed")),
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("valid retry policy: %v", err)
	}
	for _, mutate := range []func(*domain.RetryPolicy){
		func(value *domain.RetryPolicy) { value.MaxAttempts = 0 },
		func(value *domain.RetryPolicy) { value.InitialBackoff = 0 },
		func(value *domain.RetryPolicy) { value.MaxBackoff = value.InitialBackoff - time.Nanosecond },
		func(value *domain.RetryPolicy) { value.JitterSeedDigest = "" },
	} {
		invalid := policy
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid retry policy accepted: %+v", invalid)
		}
	}
}

// TestCallPlanRetryBoundIsGlobalAcrossGroups catches resetting retry ordinals
// under a new group name to exceed MaxAttempts for one logical call.
func TestCallPlanRetryBoundIsGlobalAcrossGroups(t *testing.T) {
	t.Parallel()
	first := completeLLMPlanForDomainTest(1, "primary")
	second := completeLLMPlanForDomainTest(2, "secondary")
	first.RetryOrdinal, second.RetryOrdinal = 1, 1
	plan := domain.CallPlan{
		Digest: domain.SumBytes([]byte("two groups exceed one attempt")),
		Calls:  []domain.PhysicalCallPlan{first, second},
	}
	policy := domain.RetryPolicy{
		MaxAttempts: 1, InitialBackoff: time.Millisecond, MaxBackoff: time.Second,
		JitterSeedDigest: domain.SumBytes([]byte("one attempt")),
	}
	if err := plan.Validate(policy); err == nil {
		t.Fatal("two retry groups bypassed the one-attempt logical call bound")
	}
}

func TestCallPlanRejectsRetryOrdinalSwappedAcrossGlobalSequence(t *testing.T) {
	t.Parallel()
	first := completeLLMPlanForDomainTest(1, "primary")
	second := completeLLMPlanForDomainTest(2, "secondary")
	first.RetryOrdinal, second.RetryOrdinal = 2, 1
	plan := domain.CallPlan{
		Digest: domain.SumBytes([]byte("swapped global retry sequence")),
		Calls:  []domain.PhysicalCallPlan{first, second},
	}
	policy := domain.RetryPolicy{
		MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Second,
		JitterSeedDigest: domain.SumBytes([]byte("two attempts")),
	}
	if err := plan.Validate(policy); err == nil {
		t.Fatal("call plan accepted retry ordinals detached from physical order")
	}
}

func completeLLMPlanForDomainTest(ordinal int, group string) domain.PhysicalCallPlan {
	suffix := fmt.Sprintf("%032x", ordinal)
	return domain.PhysicalCallPlan{
		ID: domain.AttemptCallID("call_" + suffix), Ordinal: int64(ordinal), RetryGroup: group,
		RetryOrdinal: int64(ordinal), Kind: domain.PhysicalLLMRequest, Provider: "fake-llm",
		RequestDigest: domain.SumBytes([]byte("plan " + suffix)), IdempotencyKey: "physical_" + suffix,
		Reservations: []domain.ReservationPlan{
			{ID: domain.ReservationID(fmt.Sprintf("res_%032x", ordinal*16)), Dimension: domain.BudgetLLMCalls, Subkey: "request", UpperBound: 1},
			{ID: domain.ReservationID(fmt.Sprintf("res_%032x", ordinal*16+1)), Dimension: domain.BudgetLLMInputTokens, Subkey: "input", UpperBound: 1},
			{ID: domain.ReservationID(fmt.Sprintf("res_%032x", ordinal*16+2)), Dimension: domain.BudgetLLMOutputTokens, Subkey: "output", UpperBound: 1},
			{ID: domain.ReservationID(fmt.Sprintf("res_%032x", ordinal*16+3)), Dimension: domain.BudgetExternalCostMicroUSD, Subkey: "cost", UpperBound: 1},
		},
	}
}
