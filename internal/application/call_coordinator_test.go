package application_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
)

func TestDispatchCoordinatorRetriesCompletedFailureAndReturnsDatabaseTrace(t *testing.T) {
	fixture := newCoordinatorFixture(t, "01", domain.BudgetLimits{
		MaxLLMCalls: 2, MaxLLMInputTokens: 20, MaxLLMOutputTokens: 20, MaxLLMCostMicroUSD: 20,
		MaxActiveTimeMilliseconds: 1000,
	})
	adapter := &scriptedCallAdapter[string]{
		plan: domain.CallPlanDecision{Plan: &domain.CallPlan{
			Digest: domain.SumBytes([]byte("retry plan")),
			Calls: []domain.PhysicalCallPlan{
				coordinatorPhysicalPlan(1, 1), coordinatorPhysicalPlan(2, 2),
			},
		}},
		executions: []domain.PhysicalExecution[string]{
			{
				Boundary:          domain.BoundaryCompleted,
				Failure:           &domain.PortFailure{Code: domain.FailureRateLimited, Class: domain.FailureRetryable},
				ProviderRequestID: "provider-retry", ResponseDigest: coordinatorDigestPointer("retry"),
			},
			{
				Boundary: domain.BoundaryCompleted, Value: stringPointer("ok"),
				ProviderRequestID: "provider-success", ResponseDigest: coordinatorDigestPointer("success"),
			},
		},
	}
	coordinator, err := durable.NewCallCoordinator[string](fixture.store, adapter, fixture.clock)
	if err != nil {
		t.Fatalf("NewCallCoordinator: %v", err)
	}
	outcome, err := coordinator.Execute(context.Background(), fixture.openRequest(1))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatalf("outcome validation: %v", err)
	}
	if outcome.Value == nil || *outcome.Value != "ok" || outcome.Failure != nil {
		t.Fatalf("outcome = %+v", outcome)
	}
	if got := adapter.dispatchedIDs(); len(got) != 2 || got[0] == got[1] {
		t.Fatalf("dispatched physical IDs = %v", got)
	}
	if len(fixture.clock.delays()) != 1 {
		t.Fatalf("backoff count = %d, want 1", len(fixture.clock.delays()))
	}
	stored := readCoordinatorTrace(t, fixture.path, domain.CallRecordID("callrec_00000000000000000000000000000001"))
	if !outcome.CallTrace.Equal(stored) {
		t.Fatalf("returned trace = %+v, database trace = %+v", outcome.CallTrace, stored)
	}
}

func TestMeteredOutcomeEveryTypedPortFailureHasTerminalDatabaseTrace(t *testing.T) {
	tests := []struct {
		code     domain.PortFailureCode
		class    domain.FailureClass
		boundary domain.PhysicalBoundary
	}{
		{domain.FailureTransport, domain.FailureRetryable, domain.BoundaryConfirmedNoSend},
		{domain.FailureRateLimited, domain.FailureRetryable, domain.BoundaryCompleted},
		{domain.FailureUnavailable, domain.FailureBlocked, domain.BoundaryCompleted},
		{domain.FailureProtocol, domain.FailureRejected, domain.BoundaryCompleted},
		{domain.FailureCapabilityMissing, domain.FailureIncompatible, domain.BoundaryCompleted},
		{domain.FailureVersionMismatch, domain.FailureIncompatible, domain.BoundaryCompleted},
		{domain.FailureBudgetExhausted, domain.FailureRejected, domain.BoundaryCompleted},
		{domain.FailurePolicyRejected, domain.FailureRejected, domain.BoundaryCompleted},
		{domain.FailureCircuitOpen, domain.FailureBlocked, domain.BoundaryCompleted},
		{domain.FailureBoundaryUnknown, domain.FailureUnknown, domain.BoundaryUnknown},
	}
	for index, test := range tests {
		t.Run(string(test.code), func(t *testing.T) {
			fixture := newCoordinatorFixture(t, fmt.Sprintf("%02x", index+16), domain.BudgetLimits{
				MaxLLMCalls: 1, MaxLLMInputTokens: 1, MaxLLMOutputTokens: 1, MaxLLMCostMicroUSD: 1,
				MaxActiveTimeMilliseconds: 1000,
			})
			failure := domain.PortFailure{Code: test.code, Class: test.class}
			execution := domain.PhysicalExecution[int]{Boundary: test.boundary, Failure: &failure}
			if test.boundary == domain.BoundaryCompleted {
				execution.ProviderRequestID = "provider-terminal"
				execution.ResponseDigest = coordinatorDigestPointer("terminal failure")
			}
			adapter := &scriptedCallAdapter[int]{
				plan:       domain.CallPlanDecision{Plan: &domain.CallPlan{Digest: domain.SumBytes([]byte("one call plan")), Calls: []domain.PhysicalCallPlan{coordinatorPhysicalPlan(1, 1)}}},
				executions: []domain.PhysicalExecution[int]{execution},
			}
			coordinator, err := durable.NewCallCoordinator[int](fixture.store, adapter, fixture.clock)
			if err != nil {
				t.Fatalf("NewCallCoordinator: %v", err)
			}
			outcome, err := coordinator.Execute(context.Background(), fixture.openRequest(1))
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if err := outcome.Validate(); err != nil {
				t.Fatalf("typed failure outcome invalid: %v", err)
			}
			if outcome.Failure == nil || outcome.Failure.Code != test.code || outcome.Value != nil {
				t.Fatalf("outcome = %+v", outcome)
			}
			stored := readCoordinatorTrace(t, fixture.path, domain.CallRecordID("callrec_00000000000000000000000000000001"))
			if !outcome.CallTrace.Equal(stored) {
				t.Fatalf("returned trace = %+v, database trace = %+v", outcome.CallTrace, stored)
			}
		})
	}
}

func TestDispatchCoordinatorPersistsPolicyPreRejectionWithoutPhysicalCall(t *testing.T) {
	fixture := newCoordinatorFixture(t, "30", domain.BudgetLimits{MaxActiveTimeMilliseconds: 1000})
	failure := domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
	adapter := &scriptedCallAdapter[int]{plan: domain.CallPlanDecision{Failure: &failure}}
	coordinator, err := durable.NewCallCoordinator[int](fixture.store, adapter, fixture.clock)
	if err != nil {
		t.Fatalf("NewCallCoordinator: %v", err)
	}
	outcome, err := coordinator.Execute(context.Background(), fixture.openRequest(1))
	if err != nil {
		t.Fatalf("Execute pre-rejection: %v", err)
	}
	if outcome.Failure == nil || outcome.CallTrace.DispatchKind != domain.DispatchNone || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 0 {
		t.Fatalf("pre-rejection outcome = %+v", outcome)
	}
	stored := readCoordinatorTrace(t, fixture.path, domain.CallRecordID("callrec_00000000000000000000000000000001"))
	if !outcome.CallTrace.Equal(stored) {
		t.Fatalf("returned trace = %+v, database trace = %+v", outcome.CallTrace, stored)
	}
	if adapter.executeCount() != 0 {
		t.Fatalf("pre-rejection dispatched %d physical calls", adapter.executeCount())
	}
}

func TestCallCoordinatorResumesEveryDurableBoundaryWithoutReplanning(t *testing.T) {
	tests := []struct {
		name          string
		suffix        string
		boundary      string
		wantPlanCalls int
		wantExecCalls int
		wantFailure   bool
	}{
		{name: "open", suffix: "40", boundary: "OPEN", wantPlanCalls: 1, wantExecCalls: 1},
		{name: "prepared", suffix: "41", boundary: "PREPARED", wantExecCalls: 1},
		{name: "dispatching", suffix: "42", boundary: "DISPATCHING", wantExecCalls: 1},
		{name: "sent", suffix: "43", boundary: "SENT", wantExecCalls: 1},
		{name: "completed", suffix: "44", boundary: "COMPLETED", wantExecCalls: 1},
		{name: "unknown", suffix: "45", boundary: "UNKNOWN", wantFailure: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limits := domain.BudgetLimits{
				MaxLLMCalls: 1, MaxLLMInputTokens: 1, MaxLLMOutputTokens: 1, MaxLLMCostMicroUSD: 1,
				MaxActiveTimeMilliseconds: 1000,
			}
			fixture := newCoordinatorFixture(t, test.suffix, limits)
			request := fixture.openRequest(1)
			plan := domain.CallPlan{
				Digest: domain.SumBytes([]byte("durable boundary plan")),
				Calls:  []domain.PhysicalCallPlan{coordinatorPhysicalPlan(1, 1)},
			}
			stageCoordinatorBoundary(t, fixture, request, plan, test.boundary)

			execution := domain.PhysicalExecution[string]{
				Boundary: domain.BoundaryCompleted, Value: stringPointer("ok"),
				ProviderRequestID: "provider-durable", ResponseDigest: coordinatorDigestPointer("durable response"),
			}
			adapter := &scriptedCallAdapter[string]{
				plan: domain.CallPlanDecision{Plan: &plan}, executions: []domain.PhysicalExecution[string]{execution},
			}
			coordinator, err := durable.NewCallCoordinator[string](fixture.store, adapter, fixture.clock)
			if err != nil {
				t.Fatalf("NewCallCoordinator: %v", err)
			}
			outcome, err := coordinator.Execute(context.Background(), request)
			if err != nil {
				t.Fatalf("resume %s: %v", test.boundary, err)
			}
			if got := adapter.planCount(); got != test.wantPlanCalls {
				t.Fatalf("Plan calls = %d, want %d", got, test.wantPlanCalls)
			}
			if got := adapter.executeCount(); got != test.wantExecCalls {
				t.Fatalf("Execute calls = %d, want %d", got, test.wantExecCalls)
			}
			if test.wantFailure {
				if outcome.Failure == nil || outcome.Failure.Code != domain.FailureBoundaryUnknown || outcome.Value != nil {
					t.Fatalf("UNKNOWN outcome = %+v", outcome)
				}
			} else if outcome.Value == nil || *outcome.Value != "ok" || outcome.Failure != nil {
				t.Fatalf("resumed outcome = %+v", outcome)
			}
			stored := readCoordinatorTrace(t, fixture.path, request.ID)
			if !outcome.CallTrace.Equal(stored) {
				t.Fatalf("resumed trace = %+v, stored = %+v", outcome.CallTrace, stored)
			}
		})
	}
}

func TestCallCoordinatorTerminalReplayReturnsDurableFailureWithoutPlanning(t *testing.T) {
	fixture := newCoordinatorFixture(t, "46", domain.BudgetLimits{
		MaxLLMCalls: 1, MaxLLMInputTokens: 1, MaxLLMOutputTokens: 1, MaxLLMCostMicroUSD: 1,
		MaxActiveTimeMilliseconds: 1000,
	})
	request := fixture.openRequest(1)
	failure := domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
	firstAdapter := &scriptedCallAdapter[int]{
		plan: domain.CallPlanDecision{Plan: &domain.CallPlan{
			Digest: domain.SumBytes([]byte("terminal replay plan")), Calls: []domain.PhysicalCallPlan{coordinatorPhysicalPlan(1, 1)},
		}},
		executions: []domain.PhysicalExecution[int]{{Boundary: domain.BoundaryUnknown, Failure: &failure}},
	}
	coordinator, err := durable.NewCallCoordinator[int](fixture.store, firstAdapter, fixture.clock)
	if err != nil {
		t.Fatalf("NewCallCoordinator: %v", err)
	}
	first, err := coordinator.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("initial Execute: %v", err)
	}

	replayAdapter := &scriptedCallAdapter[int]{plan: firstAdapter.plan}
	replayCoordinator, err := durable.NewCallCoordinator[int](fixture.store, replayAdapter, fixture.clock)
	if err != nil {
		t.Fatalf("NewCallCoordinator replay: %v", err)
	}
	replayed, err := replayCoordinator.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("terminal replay: %v", err)
	}
	if replayAdapter.planCount() != 0 || replayAdapter.executeCount() != 0 {
		t.Fatalf("terminal replay called adapter: plan=%d execute=%d", replayAdapter.planCount(), replayAdapter.executeCount())
	}
	if replayed.Failure == nil || replayed.Failure.Code != domain.FailureBoundaryUnknown || !replayed.CallTrace.Equal(first.CallTrace) {
		t.Fatalf("terminal replay = %+v, first = %+v", replayed, first)
	}
}

func TestCallCoordinatorTerminalSuccessReplayUsesOriginalPhysicalIdentity(t *testing.T) {
	fixture := newCoordinatorFixture(t, "47", domain.BudgetLimits{
		MaxLLMCalls: 1, MaxLLMInputTokens: 1, MaxLLMOutputTokens: 1, MaxLLMCostMicroUSD: 1,
		MaxActiveTimeMilliseconds: 1000,
	})
	request := fixture.openRequest(1)
	plan := domain.CallPlan{
		Digest: domain.SumBytes([]byte("terminal success replay plan")), Calls: []domain.PhysicalCallPlan{coordinatorPhysicalPlan(1, 1)},
	}
	execution := domain.PhysicalExecution[string]{
		Boundary: domain.BoundaryCompleted, Value: stringPointer("ok"),
		ProviderRequestID: "provider-terminal-success", ResponseDigest: coordinatorDigestPointer("terminal success"),
	}
	firstAdapter := &scriptedCallAdapter[string]{
		plan: domain.CallPlanDecision{Plan: &plan}, executions: []domain.PhysicalExecution[string]{execution},
	}
	coordinator, err := durable.NewCallCoordinator[string](fixture.store, firstAdapter, fixture.clock)
	if err != nil {
		t.Fatalf("NewCallCoordinator: %v", err)
	}
	first, err := coordinator.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("initial Execute: %v", err)
	}

	replayAdapter := &scriptedCallAdapter[string]{executions: []domain.PhysicalExecution[string]{execution}}
	replayCoordinator, err := durable.NewCallCoordinator[string](fixture.store, replayAdapter, fixture.clock)
	if err != nil {
		t.Fatalf("NewCallCoordinator replay: %v", err)
	}
	replayed, err := replayCoordinator.Execute(context.Background(), request)
	if err != nil {
		t.Fatalf("terminal success replay: %v", err)
	}
	if replayAdapter.planCount() != 0 || replayAdapter.executeCount() != 1 {
		t.Fatalf("terminal success replay adapter calls: plan=%d execute=%d", replayAdapter.planCount(), replayAdapter.executeCount())
	}
	firstIDs, replayIDs := firstAdapter.dispatchedIDs(), replayAdapter.dispatchedIDs()
	if len(firstIDs) != 1 || len(replayIDs) != 1 || firstIDs[0] != replayIDs[0] {
		t.Fatalf("terminal replay physical IDs: first=%v replay=%v", firstIDs, replayIDs)
	}
	if replayed.Value == nil || *replayed.Value != "ok" || !replayed.CallTrace.Equal(first.CallTrace) {
		t.Fatalf("terminal replay = %+v, first = %+v", replayed, first)
	}
}

func stageCoordinatorBoundary(t *testing.T, fixture coordinatorFixture, request domain.OpenCallRequest, plan domain.CallPlan, boundary string) {
	t.Helper()
	record, err := fixture.store.OpenCall(context.Background(), request)
	if err != nil {
		t.Fatalf("OpenCall boundary setup: %v", err)
	}
	if boundary == "OPEN" {
		return
	}
	prepared, err := fixture.store.PrepareCalls(context.Background(), domain.PrepareCallsRequest{
		RunID: request.RunID, ExpectedRunVersion: request.ExpectedRunVersion, StageName: request.StageName,
		AttemptID: request.AttemptID, CallRecordID: record.ID, PlanDigest: plan.Digest, Calls: plan.Calls,
		IdempotencyKey: coordinatorID("prepare", string(record.ID)), At: fixture.clock.Now(),
	})
	if err != nil {
		t.Fatalf("PrepareCalls boundary setup: %v", err)
	}
	if boundary == "PREPARED" {
		return
	}
	physical := prepared.PhysicalCalls[0]
	grant, err := fixture.store.BeginDispatch(context.Background(), domain.BeginDispatchRequest{
		RunID: request.RunID, ExpectedRunVersion: request.ExpectedRunVersion, StageName: request.StageName,
		AttemptID: request.AttemptID, CallRecordID: record.ID, AttemptCallID: physical.ID,
		IdempotencyKey: coordinatorID("begin", string(physical.ID)), At: fixture.clock.Now(),
	})
	if err != nil {
		t.Fatalf("BeginDispatch boundary setup: %v", err)
	}
	if boundary == "DISPATCHING" {
		return
	}
	if boundary == "UNKNOWN" {
		failure := domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
		if err := fixture.store.CompletePhysical(context.Background(), domain.CompletePhysicalRequest{
			RunID: request.RunID, ExpectedRunVersion: request.ExpectedRunVersion, StageName: request.StageName,
			AttemptID: request.AttemptID, CallRecordID: record.ID, AttemptCallID: physical.ID,
			State: domain.PhysicalUnknown, Outcome: domain.PhysicalOutcomeUnknown, Failure: &failure,
			IdempotencyKey: coordinatorID("complete", string(physical.ID)), At: fixture.clock.Now(),
		}); err != nil {
			t.Fatalf("CompletePhysical UNKNOWN setup: %v", err)
		}
		return
	}
	if err := fixture.store.MarkSent(context.Background(), grant, fixture.clock.Now()); err != nil {
		t.Fatalf("MarkSent boundary setup: %v", err)
	}
	if boundary == "SENT" {
		return
	}
	if boundary != "COMPLETED" {
		t.Fatalf("unknown boundary setup %q", boundary)
	}
	if err := fixture.store.CompletePhysical(context.Background(), domain.CompletePhysicalRequest{
		RunID: request.RunID, ExpectedRunVersion: request.ExpectedRunVersion, StageName: request.StageName,
		AttemptID: request.AttemptID, CallRecordID: record.ID, AttemptCallID: physical.ID,
		State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess,
		ProviderRequestID: "provider-durable", ResponseDigest: coordinatorDigestPointer("durable response"),
		IdempotencyKey: coordinatorID("complete", string(physical.ID)), At: fixture.clock.Now(),
	}); err != nil {
		t.Fatalf("CompletePhysical COMPLETED setup: %v", err)
	}
}

type coordinatorFixture struct {
	store     *sqlite.Store
	path      string
	runID     domain.RunID
	attemptID domain.AttemptID
	clock     *recordingClock
}

func newCoordinatorFixture(t *testing.T, suffix string, limits domain.BudgetLimits) coordinatorFixture {
	return newCoordinatorFixtureWithStages(t, suffix, limits, []domain.StageName{"prepare"})
}

func newCoordinatorFixtureWithStages(t *testing.T, suffix string, limits domain.BudgetLimits, stages []domain.StageName) coordinatorFixture {
	t.Helper()
	return newCoordinatorFixtureWithStageInput(t, suffix, limits, stages, domain.SumBytes([]byte("input")))
}

func newCoordinatorFixtureWithStageInput(t *testing.T, suffix string, limits domain.BudgetLimits, stages []domain.StageName, inputDigest domain.Digest) coordinatorFixture {
	return newCoordinatorFixtureAt(t, suffix, limits, stages, inputDigest, time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC))
}

func newCoordinatorFixtureAt(t *testing.T, suffix string, limits domain.BudgetLimits, stages []domain.StageName, inputDigest domain.Digest, at time.Time) coordinatorFixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "coordinator.db")
	clock := newRecordingClock(at)
	store, err := openFreshApplicationSQLite(t, sqlite.Config{Path: path, BusyTimeout: time.Second, MaxReaders: 4}, clock)
	if err != nil {
		t.Fatalf("open coordinator store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runID := domain.RunID("run_000000000000000000000000000000" + suffix)
	attemptID := domain.AttemptID("attempt_000000000000000000000000000000" + suffix)
	configJSON := []byte(`{"schema":"cpgen.config/v1"}`)
	requestJSON := canonicalCoordinatorRunRequestJSON(limits)
	created, err := store.CreateRun(context.Background(), domain.CreateRunRequest{
		RunID: runID, SubmittedRequestJSON: requestJSON, SubmittedRequestDigest: domain.SumBytes(requestJSON),
		EffectiveSeed: 7, RedactedEffectiveConfigJSON: configJSON, RedactedEffectiveConfigDigest: domain.SumBytes(configJSON),
		WorkflowRevision: "slice1/v1", SchemaVersion: "cpgen.request/v1", WorkflowDigest: domain.SumBytes([]byte("workflow")),
		BudgetLimits: limits, StageSequence: stages, CreatedAt: clock.Now(),
		IdempotencyKey: coordinatorID("create", "coordinator "+suffix),
	})
	if err != nil || created.Version != 1 {
		t.Fatalf("CreateRun = %+v, %v", created, err)
	}
	_, err = store.BeginStage(context.Background(), domain.BeginStageCommand{
		RunID: runID, ExpectedRunVersion: 1, StageName: "prepare", AttemptID: attemptID,
		InputDigest: inputDigest, IdempotencyKey: coordinatorID("begin", "coordinator "+suffix), At: clock.Now(),
	})
	if err != nil {
		t.Fatalf("BeginStage: %v", err)
	}
	return coordinatorFixture{store: store, path: path, runID: runID, attemptID: attemptID, clock: clock}
}

func canonicalCoordinatorRunRequestJSON(limits domain.BudgetLimits) []byte {
	encoded, err := json.Marshal(domain.RunRequest{
		SchemaVersion: "cpgen.request/v1", Mode: "generate", Brief: "coordinator",
		Tags: []string{"graphs"}, NormalizedTags: []string{"graphs"}, Language: "en",
		Difficulty: "hard", TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512,
		SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"},
		BudgetLimits: limits,
	})
	if err != nil {
		panic(err)
	}
	var decoded any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		panic(err)
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		panic(err)
	}
	return canonical
}

func (f coordinatorFixture) openRequest(ordinal int) domain.OpenCallRequest {
	suffix := fmt.Sprintf("%032x", ordinal)
	return domain.OpenCallRequest{
		ID: domain.CallRecordID("callrec_" + suffix), RunID: f.runID, ExpectedRunVersion: 2,
		StageName: "prepare", AttemptID: f.attemptID, LogicalOperationID: "coordinator-operation-" + suffix,
		Kind: domain.CallLLMGenerate, Provider: "fake-llm", RequestDigest: domain.SumBytes([]byte("coordinator request")),
		PolicyDigest: domain.SumBytes([]byte("coordinator policy")), RetryPolicy: domain.RetryPolicy{
			MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond,
			JitterSeedDigest: domain.SumBytes([]byte("coordinator jitter")),
		},
		IdempotencyKey: "open_" + suffix, At: f.clock.Now(),
	}
}

type scriptedCallAdapter[T any] struct {
	mu         sync.Mutex
	plan       domain.CallPlanDecision
	planned    int
	executions []domain.PhysicalExecution[T]
	dispatched []domain.AttemptCallID
}

func (a *scriptedCallAdapter[T]) Plan(context.Context, domain.CallRecord) (domain.CallPlanDecision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.planned++
	return a.plan, nil
}

func (a *scriptedCallAdapter[T]) Execute(_ context.Context, grant domain.DispatchGrant) (domain.PhysicalExecution[T], error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dispatched = append(a.dispatched, grant.AttemptCallID)
	if len(a.executions) == 0 {
		return domain.PhysicalExecution[T]{}, fmt.Errorf("unexpected physical dispatch")
	}
	result := a.executions[0]
	a.executions = a.executions[1:]
	return result, nil
}

func (a *scriptedCallAdapter[T]) dispatchedIDs() []domain.AttemptCallID {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]domain.AttemptCallID(nil), a.dispatched...)
}

func (a *scriptedCallAdapter[T]) executeCount() int { return len(a.dispatchedIDs()) }

func (a *scriptedCallAdapter[T]) planCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.planned
}

type recordingClock struct {
	mu       sync.Mutex
	now      time.Time
	backoffs []time.Duration
}

func newRecordingClock(now time.Time) *recordingClock { return &recordingClock{now: now} }

func (c *recordingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := c.now
	c.now = c.now.Add(time.Millisecond)
	return result
}

func (c *recordingClock) After(delay time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.backoffs = append(c.backoffs, delay)
	result := make(chan time.Time, 1)
	c.now = c.now.Add(delay)
	result <- c.now
	return result
}

func (c *recordingClock) delays() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.backoffs...)
}

func coordinatorPhysicalPlan(ordinal, retryOrdinal int) domain.PhysicalCallPlan {
	suffix := fmt.Sprintf("%032x", ordinal)
	return domain.PhysicalCallPlan{
		ID: domain.AttemptCallID("call_" + suffix), Ordinal: int64(ordinal), RetryGroup: "provider-request", RetryOrdinal: int64(retryOrdinal),
		Kind: domain.PhysicalLLMRequest, Provider: "fake-llm", RequestDigest: domain.SumBytes([]byte("physical " + suffix)),
		IdempotencyKey: "physical_" + suffix,
		Reservations: []domain.ReservationPlan{
			{ID: domain.ReservationID(fmt.Sprintf("res_%032x", ordinal*16)), Dimension: domain.BudgetLLMCalls, Subkey: "request", UpperBound: 1},
			{ID: domain.ReservationID(fmt.Sprintf("res_%032x", ordinal*16+1)), Dimension: domain.BudgetLLMInputTokens, Subkey: "input", UpperBound: 1},
			{ID: domain.ReservationID(fmt.Sprintf("res_%032x", ordinal*16+2)), Dimension: domain.BudgetLLMOutputTokens, Subkey: "output", UpperBound: 1},
			{ID: domain.ReservationID(fmt.Sprintf("res_%032x", ordinal*16+3)), Dimension: domain.BudgetExternalCostMicroUSD, Subkey: "cost", UpperBound: 1},
		},
	}
}

func readCoordinatorTrace(t *testing.T, path string, recordID domain.CallRecordID) domain.CallTrace {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("open trace reader: %v", err)
	}
	defer db.Close()
	var logical, dispatch string
	var result, source, hit sql.NullString
	if err := db.QueryRow(`SELECT logical_operation_id, dispatch_kind, result_attempt_call_id,
		cache_source_call_record_id, cache_hit_call_record_id FROM call_records WHERE call_record_id = ?`, recordID,
	).Scan(&logical, &dispatch, &result, &source, &hit); err != nil {
		t.Fatalf("read terminal call record: %v", err)
	}
	trace := domain.CallTrace{LogicalOperationID: logical, DispatchKind: domain.DispatchKind(dispatch)}
	if result.Valid {
		value := domain.AttemptCallID(result.String)
		trace.ResultAttemptCallID = &value
	}
	if source.Valid {
		value := domain.CallRecordID(source.String)
		trace.CacheSourceCallRecordID = &value
	}
	if hit.Valid {
		value := domain.CallRecordID(hit.String)
		trace.CacheHitCallRecordID = &value
	}
	rows, err := db.Query(`SELECT attempt_call_id FROM physical_calls
		WHERE call_record_id = ? AND (sent_at IS NOT NULL OR state = 'UNKNOWN') ORDER BY ordinal`, recordID)
	if err != nil {
		t.Fatalf("read physical trace: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id domain.AttemptCallID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan physical trace: %v", err)
		}
		trace.PhysicalAttemptCallIDs = append(trace.PhysicalAttemptCallIDs, id)
	}
	if err := trace.Validate(); err != nil {
		t.Fatalf("stored trace invalid: %+v: %v", trace, err)
	}
	return trace
}

func coordinatorDigestPointer(value string) *domain.Digest {
	digest := domain.SumBytes([]byte(value))
	return &digest
}

func stringPointer(value string) *string { return &value }

func coordinatorID(prefix, material string) string {
	digest := string(domain.SumBytes([]byte(material)))
	return prefix + "_" + digest[7:39]
}

type prepareFailureLedger struct {
	port.CallLedger
	failure error
}

func (l prepareFailureLedger) PrepareCalls(context.Context, domain.PrepareCallsRequest) (domain.PreparedCalls, error) {
	return domain.PreparedCalls{}, l.failure
}

func TestDispatchCoordinatorPreservesPreparationFailureWithoutDispatch(t *testing.T) {
	for _, failure := range []error{sqlite.ErrVersionConflict, errors.New("preparation storage failure")} {
		t.Run(failure.Error(), func(t *testing.T) {
			fixture := newCoordinatorFixture(t, "e1", domain.BudgetLimits{MaxLLMCalls: 2})
			adapter := &scriptedCallAdapter[string]{plan: domain.CallPlanDecision{Plan: &domain.CallPlan{
				Digest: domain.SumBytes([]byte("prepare failure")), Calls: []domain.PhysicalCallPlan{coordinatorPhysicalPlan(1, 1)},
			}}}
			coordinator, err := durable.NewCallCoordinator[string](prepareFailureLedger{fixture.store, failure}, adapter, fixture.clock)
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := coordinator.Execute(context.Background(), fixture.openRequest(1))
			if !errors.Is(err, failure) || outcome.Value != nil || adapter.executeCount() != 0 {
				t.Fatalf("preparation failure was lost or dispatched: outcome=%+v err=%v sends=%d", outcome, err, adapter.executeCount())
			}
		})
	}
}
