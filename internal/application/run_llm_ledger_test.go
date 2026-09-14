package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
)

func TestRunLLMLedgerSettlesAcrossActiveTimeHeartbeat(t *testing.T) {
	t.Setenv("CPGEN_DURABLE_TEST_KEY", "fixture-key")
	var httpCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "heartbeat-provider", "choices": []any{map[string]any{"message": map[string]string{"content": `{"schema_version":"cpgen.idea/v1","title":"heartbeat"}`}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	}))
	defer server.Close()
	model, request := durableTestProvider(t, server.URL)
	f := newCoordinatorFixtureWithStages(t, "a3", domain.BudgetLimits{MaxLLMCalls: 3, MaxLLMInputTokens: 30000, MaxLLMOutputTokens: 2000, MaxLLMCostMicroUSD: 2000, MaxArtifactBytes: 500000, MaxActiveTimeMilliseconds: 60000}, []domain.StageName{"prepare", "exercise"})
	active, err := f.store.AccountActiveTime(context.Background(), domain.ActiveTimeCommand{RunID: f.runID, ExpectedRunVersion: 2, Action: domain.ActiveTimeStart, IdempotencyKey: "active_00000000000000000000000000000031", At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	open := f.openRequest(1101)
	open.ExpectedRunVersion, open.RetryPolicy.MaxAttempts = active.RunVersion, 1
	request.LogicalIdempotencyKey = open.LogicalOperationID
	plan, err := model.PlanGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	open.Provider, open.RequestDigest, open.PolicyDigest = plan.Provider, plan.RequestDigest, request.ProviderPolicyDigest
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	provider := &heartbeatPhysicalLLM{PhysicalLLM: model, fixture: f}
	ledger, err := durable.NewRunLedger(f.store, f.runID, "prepare", f.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	calls, err := durable.NewReplayableLLMCalls(ledger, provider, blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	result, err := calls.Generate(context.Background(), open, request)
	if err != nil || result.Value == nil {
		t.Fatalf("heartbeat prevented settlement: result=%+v error=%v", result, err)
	}
	if httpCalls.Load() != 1 || result.Value.Usage != (port.Usage{InputTokens: 3, OutputTokens: 4}) {
		t.Fatalf("HTTP=%d result=%+v", httpCalls.Load(), result)
	}
}

type heartbeatPhysicalLLM struct {
	port.PhysicalLLM
	fixture coordinatorFixture
}

func (p *heartbeatPhysicalLLM) GeneratePhysical(ctx context.Context, request port.GenerateRequest, id domain.AttemptCallID) (port.PhysicalLLMResult, error) {
	result, err := p.PhysicalLLM.GeneratePhysical(ctx, request, id)
	if err != nil {
		return result, err
	}
	<-p.fixture.clock.After(time.Second)
	run, err := p.fixture.store.GetRun(ctx, p.fixture.runID)
	if err != nil {
		return result, err
	}
	_, err = p.fixture.store.AccountActiveTime(ctx, domain.ActiveTimeCommand{RunID: run.RunID, ExpectedRunVersion: run.Version, Action: domain.ActiveTimeHeartbeat, IdempotencyKey: coordinatorID("heartbeat", string(id)), At: p.fixture.clock.Now()})
	return result, err
}

func TestRunLLMLedgerRepairsAndReplaysAfterHeartbeatsAndDatabaseReopen(t *testing.T) {
	f := newStructuredLLMOptionsFixture(t, structuredLLMOptions{activeTimeMS: 60000})
	startRunLLMActiveTime(t, f.coordinatorFixture)
	provider := &heartbeatPhysicalLLM{PhysicalLLM: f.model, fixture: f.coordinatorFixture}
	service := boundStructuredLLM(t, f, f.store, provider)
	first, err := service.Generate(context.Background(), f.open, f.request)
	if err != nil || first.Outcome.Value == nil || len(first.CallTraces) != 2 || len(first.Artifacts) != 2 || f.httpCalls.Load() != 2 {
		t.Fatalf("first=%+v err=%v HTTP=%d", first, err, f.httpCalls.Load())
	}
	original, err := f.store.ReadOpenCall(context.Background(), f.open.ID)
	if err != nil || original.ExpectedRunVersion != 3 {
		t.Fatalf("original opening command=%+v err=%v", original, err)
	}
	runLLMHeartbeat(t, f.coordinatorFixture, "restart")
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.OpenWithClock(context.Background(), sqlite.Config{Path: f.path, BusyTimeout: time.Second, MaxReaders: 4}, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service = boundStructuredLLM(t, f, store, f.model)
	current, err := store.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	open := f.open
	open.ExpectedRunVersion = current.Version
	replayed, err := service.Generate(context.Background(), open, f.request)
	if err != nil || replayed.Outcome.Value == nil || !reflect.DeepEqual(first.CallTraces, replayed.CallTraces) || !reflect.DeepEqual(first.Artifacts, replayed.Artifacts) || replayed.Usage != first.Usage || f.httpCalls.Load() != 2 {
		t.Fatalf("replay=%+v err=%v HTTP=%d", replayed, err, f.httpCalls.Load())
	}
	stored, err := store.ReadOpenCall(context.Background(), original.ID)
	if err != nil || !reflect.DeepEqual(original, stored) {
		t.Fatalf("original command changed on replay: %+v %v", stored, err)
	}
}

func TestRunLLMLedgerTransportRetryAndUnknownBoundarySurviveHeartbeats(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport-retry", true: "unknown-repair"}[unknown], func(t *testing.T) {
			options := structuredLLMOptions{activeTimeMS: 60000, disconnectRepair: unknown}
			if !unknown {
				options.firstStatus = http.StatusTooManyRequests
			}
			f := newStructuredLLMOptionsFixture(t, options)
			if !unknown {
				f.open.RetryPolicy.MaxAttempts = 2
			}
			startRunLLMActiveTime(t, f.coordinatorFixture)
			service := boundStructuredLLM(t, f, f.store, &heartbeatPhysicalLLM{PhysicalLLM: f.model, fixture: f.coordinatorFixture})
			first, err := service.Generate(context.Background(), f.open, f.request)
			if err != nil || f.httpCalls.Load() != 2 {
				t.Fatalf("first=%+v err=%v HTTP=%d", first, err, f.httpCalls.Load())
			}
			if unknown {
				if first.Outcome.Failure == nil || first.Outcome.Failure.Code != domain.FailureBoundaryUnknown {
					t.Fatalf("unknown send outcome=%+v", first.Outcome)
				}
			} else if first.Outcome.Value == nil || len(first.CallTraces) != 1 || len(first.CallTraces[0].PhysicalAttemptCallIDs) != 2 {
				t.Fatalf("retry outcome=%+v", first)
			}
			runLLMHeartbeat(t, f.coordinatorFixture, "terminal-replay")
			service = boundStructuredLLM(t, f, f.store, f.model)
			replay, err := service.Generate(context.Background(), f.open, f.request)
			if err != nil || f.httpCalls.Load() != 2 || !reflect.DeepEqual(first.CallTraces, replay.CallTraces) || replay.Usage != first.Usage {
				t.Fatalf("replay=%+v err=%v HTTP=%d", replay, err, f.httpCalls.Load())
			}
		})
	}
}

func TestRunLLMLedgerSettlesAfterHeartbeatAndPersistedCancellation(t *testing.T) {
	f := newStructuredLLMOptionsFixture(t, structuredLLMOptions{activeTimeMS: 60000, firstContent: `{"schema_version":"cpgen.idea/v1","title":"cancelled"}`})
	startRunLLMActiveTime(t, f.coordinatorFixture)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := llmAcceptanceProviderHook{PhysicalLLM: f.model, after: func(context.Context, port.PhysicalLLMResult, error) {
		runLLMHeartbeat(t, f.coordinatorFixture, "before-cancel")
		run, err := f.store.GetRun(context.Background(), f.runID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.store.RequestCancel(context.Background(), domain.CancelRequest{ID: "control_00000000000000000000000000001101", RunID: f.runID, ExpectedRunVersion: run.Version, Reason: "fixture cancel", IdempotencyKey: "cancel_00000000000000000000000000001101", At: f.clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
	}}
	service := boundStructuredLLM(t, f, f.store, provider)
	result, err := service.Generate(ctx, f.open, f.request)
	if err != nil || result.Outcome.Value == nil || len(result.Artifacts) != 1 || result.Usage != (port.Usage{InputTokens: 3, OutputTokens: 4}) || f.httpCalls.Load() != 1 {
		t.Fatalf("cancel settlement=%+v err=%v", result, err)
	}
	service = boundStructuredLLM(t, f, f.store, f.model)
	replay, err := service.Generate(context.Background(), f.open, f.request)
	if err != nil || replay.Outcome.Value == nil || !reflect.DeepEqual(result.CallTraces, replay.CallTraces) || f.httpCalls.Load() != 1 {
		t.Fatalf("cancel replay=%+v err=%v", replay, err)
	}
	ledger, err := durable.NewRunLedger(f.store, f.runID, "prepare", f.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.OpenCall(context.Background(), f.openRequest(1102)); !errors.Is(err, sqlite.ErrCancelPending) {
		t.Fatalf("pending cancel authorized fresh logical work: %v", err)
	}
}

func startRunLLMActiveTime(t *testing.T, f coordinatorFixture) {
	t.Helper()
	if _, err := f.store.AccountActiveTime(context.Background(), domain.ActiveTimeCommand{RunID: f.runID, ExpectedRunVersion: 2, Action: domain.ActiveTimeStart, IdempotencyKey: coordinatorID("active", "run-bound-llm"), At: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
}

func TestRunLLMLedgerCacheCompletionReplaysAcrossHeartbeats(t *testing.T) {
	f := newStructuredLLMOptionsFixture(t, structuredLLMOptions{activeTimeMS: 60000})
	source := structuredCacheService(t, f)
	generated, err := f.structured.Generate(context.Background(), f.open, f.request)
	if err != nil {
		t.Fatal(err)
	}
	open, request := commitStructuredSource(t, f, generated)
	if _, err := source.Put(context.Background(), f.open, f.request); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AccountActiveTime(context.Background(), domain.ActiveTimeCommand{RunID: f.runID, ExpectedRunVersion: open.ExpectedRunVersion, Action: domain.ActiveTimeStart, IdempotencyKey: coordinatorID("active", "cache-llm"), At: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	ledger, err := durable.NewRunLedger(f.store, f.runID, open.StageName, open.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	calls, err := durable.NewReplayableLLMCalls(ledger, f.model, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	structured, err := durable.NewStructuredLLMCalls(calls, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := runlock.NewManager(t.TempDir(), runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locks.Close() })
	cache, err := durable.NewStructuredLLMCache(structured, f.store, locks)
	if err != nil {
		t.Fatal(err)
	}
	first, err := cache.Reuse(context.Background(), open, request)
	if err != nil || !first.Hit {
		t.Fatalf("first cache=%+v err=%v", first, err)
	}
	runLLMHeartbeat(t, f.coordinatorFixture, "cache-replay")
	replay, err := cache.Reuse(context.Background(), open, request)
	if err != nil || !replay.Hit || !replay.Outcome.CallTrace.Equal(first.Outcome.CallTrace) || !reflect.DeepEqual(replay.Reuses, first.Reuses) || f.httpCalls.Load() != 2 {
		t.Fatalf("cache replay=%+v err=%v HTTP=%d", replay, err, f.httpCalls.Load())
	}
}

func runLLMHeartbeat(t *testing.T, f coordinatorFixture, key string) {
	t.Helper()
	<-f.clock.After(time.Second)
	run, err := f.store.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.AccountActiveTime(context.Background(), domain.ActiveTimeCommand{RunID: f.runID, ExpectedRunVersion: run.Version, Action: domain.ActiveTimeHeartbeat, IdempotencyKey: coordinatorID("heartbeat", key), At: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
}

func boundStructuredLLM(t *testing.T, f structuredLLMFixture, store durable.Store, provider port.PhysicalLLM) *durable.StructuredLLMCalls {
	t.Helper()
	ledger, err := durable.NewRunLedger(store, f.runID, "prepare", f.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	calls, err := durable.NewReplayableLLMCalls(ledger, provider, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	service, err := durable.NewStructuredLLMCalls(calls, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestRunLLMLedgerRetriesOnlyConflictingDatabaseTransitions(t *testing.T) {
	f := newStructuredLLMOptionsFixture(t, structuredLLMOptions{activeTimeMS: 60000, firstContent: `{"schema_version":"cpgen.idea/v1","title":"racing"}`})
	startRunLLMActiveTime(t, f.coordinatorFixture)
	store := &racingRunLLMStore{Store: f.store, fixture: f.coordinatorFixture, test: t, counts: map[string]int{}}
	service := boundStructuredLLM(t, f, store, f.model)
	result, err := service.Generate(context.Background(), f.open, f.request)
	if err != nil || result.Outcome.Value == nil || f.httpCalls.Load() != 1 {
		t.Fatalf("racing result=%+v err=%v HTTP=%d", result, err, f.httpCalls.Load())
	}
	for _, transition := range []string{"open", "prepare", "begin", "sent", "complete", "finish", "release"} {
		if store.counts[transition] < 2 {
			t.Errorf("%s did not retry its forced real version conflict: %d", transition, store.counts[transition])
		}
	}
	original, err := f.store.ReadOpenCall(context.Background(), f.open.ID)
	if err != nil || original.ExpectedRunVersion != 4 {
		t.Fatalf("conflicted opening version was retained: %+v %v", original, err)
	}
}

func TestRunLLMLedgerRejectsChangedBindingsAndObsoleteAttempts(t *testing.T) {
	f := newCoordinatorFixture(t, "a4", domain.BudgetLimits{})
	ledger, err := durable.NewRunLedger(f.store, f.runID, "prepare", f.attemptID)
	if err != nil {
		t.Fatal(err)
	}
	open := f.openRequest(1201)
	if _, err := ledger.OpenCall(context.Background(), open); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*domain.OpenCallRequest){
		func(v *domain.OpenCallRequest) { v.StageName = "exercise" },
		func(v *domain.OpenCallRequest) { v.AttemptID = "attempt_00000000000000000000000000000099" },
		func(v *domain.OpenCallRequest) { v.RunID = "run_00000000000000000000000000000099" },
		func(v *domain.OpenCallRequest) { v.At = v.At.Add(time.Second) },
		func(v *domain.OpenCallRequest) { v.PolicyDigest = domain.SumBytes([]byte("changed policy")) },
		func(v *domain.OpenCallRequest) { v.RequestDigest = domain.SumBytes([]byte("changed input")) },
	} {
		changed := open
		mutate(&changed)
		if _, err := ledger.OpenCall(context.Background(), changed); err == nil {
			t.Fatalf("changed binding accepted: %+v", changed)
		}
	}
	interrupted, err := f.store.InterruptStage(context.Background(), domain.InterruptStageCommand{RunID: f.runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: f.attemptID, Cause: domain.CauseStepDeadline, IdempotencyKey: coordinatorID("interrupt", "old-llm-attempt"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.OpenCall(context.Background(), open); err == nil {
		t.Fatal("interrupted attempt reopened a logical call")
	}
	_, err = f.store.BeginStage(context.Background(), domain.BeginStageCommand{RunID: f.runID, ExpectedRunVersion: interrupted.Version, StageName: "prepare", AttemptID: "attempt_00000000000000000000000000001202", InputDigest: domain.SumBytes([]byte("input")), IdempotencyKey: coordinatorID("begin", "new-llm-attempt"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.OpenCall(context.Background(), open); err == nil {
		t.Fatal("old ledger acquired authority from a new current attempt")
	}
}

type racingRunLLMStore struct {
	durable.Store
	fixture coordinatorFixture
	test    *testing.T
	counts  map[string]int
}

func TestRunLLMLedgerBoundsConflictRetriesAndHonorsCancellation(t *testing.T) {
	for _, scenario := range []string{"conflicts", "storage-error", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newCoordinatorFixture(t, "a5", domain.BudgetLimits{})
			failure := error(sqlite.ErrVersionConflict)
			want := 8
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if scenario == "storage-error" {
				failure, want = errors.New("fixture storage failure"), 1
			} else if scenario == "cancelled" {
				failure, want = context.Canceled, 0
				cancel()
			}
			store := &failingRunLLMStore{Store: f.store, failure: failure}
			ledger, err := durable.NewRunLedger(store, f.runID, "prepare", f.attemptID)
			if err != nil {
				t.Fatal(err)
			}
			open := f.openRequest(1301)
			if _, err := ledger.OpenCall(ctx, open); !errors.Is(err, failure) || store.calls != want {
				t.Fatalf("calls=%d want=%d err=%v", store.calls, want, err)
			}
			if _, err := f.store.ReadLogicalCall(context.Background(), open.ID); !errors.Is(err, sqlite.ErrNotFound) {
				t.Fatalf("failed open persisted a logical call: %v", err)
			}
		})
	}
}

type failingRunLLMStore struct {
	durable.Store
	failure error
	calls   int
}

func (s *failingRunLLMStore) OpenReplayableCall(context.Context, domain.OpenCallRequest) (domain.CallRecord, error) {
	s.calls++
	return domain.CallRecord{}, s.failure
}

func (s *racingRunLLMStore) race(transition string) {
	s.counts[transition]++
	if s.counts[transition] == 1 {
		runLLMHeartbeat(s.test, s.fixture, "race-"+transition)
	}
}

func (s *racingRunLLMStore) OpenReplayableCall(ctx context.Context, request domain.OpenCallRequest) (domain.CallRecord, error) {
	s.race("open")
	return s.Store.OpenReplayableCall(ctx, request)
}
func (s *racingRunLLMStore) PrepareCalls(ctx context.Context, request domain.PrepareCallsRequest) (domain.PreparedCalls, error) {
	s.race("prepare")
	return s.Store.PrepareCalls(ctx, request)
}
func (s *racingRunLLMStore) BeginDispatch(ctx context.Context, request domain.BeginDispatchRequest) (domain.DispatchGrant, error) {
	s.race("begin")
	return s.Store.BeginDispatch(ctx, request)
}
func (s *racingRunLLMStore) MarkSent(ctx context.Context, grant domain.DispatchGrant, at time.Time) error {
	s.race("sent")
	return s.Store.MarkSent(ctx, grant, at)
}
func (s *racingRunLLMStore) CompletePhysical(ctx context.Context, request domain.CompletePhysicalRequest) error {
	s.race("complete")
	return s.Store.CompletePhysical(ctx, request)
}
func (s *racingRunLLMStore) FinishReplayableCall(ctx context.Context, request domain.FinishCallRequest) (domain.CallTrace, error) {
	s.race("finish")
	return s.Store.FinishReplayableCall(ctx, request)
}
func (s *racingRunLLMStore) ReleaseUnwrittenArtifactReservations(ctx context.Context, request domain.OpenCallRequest) error {
	s.race("release")
	return s.Store.ReleaseUnwrittenArtifactReservations(ctx, request)
}
