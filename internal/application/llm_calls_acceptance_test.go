package application_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
)

const llmAcceptanceResponse = `{"id":"acceptance-provider","choices":[{"message":{"content":"{\"schema_version\":\"cpgen.idea/v1\",\"title\":\"ok\"}"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`

func llmAcceptanceLimits() domain.BudgetLimits {
	return domain.BudgetLimits{MaxLLMCalls: 8, MaxLLMInputTokens: 100000, MaxLLMOutputTokens: 10000, MaxLLMCostMicroUSD: 10000}
}

func llmAcceptanceBind(t *testing.T, fixture coordinatorFixture, provider port.PhysicalLLM, request port.GenerateRequest, attempts int64) (domain.OpenCallRequest, port.GenerateRequest, port.LLMRequestPlan) {
	t.Helper()
	open := fixture.openRequest(701)
	open.RetryPolicy.MaxAttempts = attempts
	request.LogicalIdempotencyKey = open.LogicalOperationID
	plan, err := provider.PlanGenerate(request)
	if err != nil {
		t.Fatalf("acceptance plan: %v", err)
	}
	open.Provider, open.RequestDigest, open.PolicyDigest = plan.Provider, plan.RequestDigest, request.ProviderPolicyDigest
	return open, request, plan
}

func llmAcceptanceService(t *testing.T, ledger port.CallLedger, provider port.PhysicalLLM, source clock.Clock) *durable.LLMCalls {
	t.Helper()
	service, err := durable.NewLLMCalls(ledger, provider, source, 100)
	if err != nil {
		t.Fatalf("construct LLM ledger bridge: %v", err)
	}
	return service
}

func llmAcceptanceLoad(t *testing.T, fixture coordinatorFixture, id domain.CallRecordID) domain.PreparedCalls {
	t.Helper()
	prepared, err := fixture.store.LoadCall(context.Background(), id)
	if err != nil {
		t.Fatalf("load acceptance ledger: %v", err)
	}
	return prepared
}

// Assert each reservation, including calls and money, rather than merely checking
// that a terminal state was written. Released retries must contribute zero.
func llmAcceptanceSettlements(t *testing.T, prepared domain.PreparedCalls, want []map[domain.BudgetDimension]int64) port.Usage {
	t.Helper()
	if prepared.Call.State != domain.CallRecordTerminal {
		t.Errorf("logical call state=%s; want TERMINAL", prepared.Call.State)
	}
	if len(prepared.PhysicalCalls) != len(want) || len(prepared.Reservations) != 4*len(want) {
		t.Errorf("physical/reservation rows=%d/%d; want %d/%d", len(prepared.PhysicalCalls), len(prepared.Reservations), len(want), 4*len(want))
	}
	var total port.Usage
	for i, physical := range prepared.PhysicalCalls {
		if physical.State == domain.PhysicalDispatching || physical.State == domain.PhysicalSent || physical.State == domain.PhysicalPrepared {
			t.Errorf("physical ordinal %d remains %s", physical.Ordinal, physical.State)
		}
		if i >= len(want) {
			continue
		}
		seen := map[domain.BudgetDimension]bool{}
		for _, reservation := range prepared.Reservations {
			if reservation.AttemptCallID != physical.ID {
				continue
			}
			seen[reservation.Dimension] = true
			expected, present := want[i][reservation.Dimension]
			if !present {
				t.Errorf("unexpected reservation dimension %s", reservation.Dimension)
			}
			if reservation.SettledValue == nil {
				t.Errorf("ordinal %d dimension %s is not settled (state=%s)", physical.Ordinal, reservation.Dimension, reservation.State)
				continue
			}
			if *reservation.SettledValue != expected {
				t.Errorf("ordinal %d dimension %s settled=%d; want %d", physical.Ordinal, reservation.Dimension, *reservation.SettledValue, expected)
			}
			state := domain.ReservationSettled
			if physical.State == domain.PhysicalAbortedNoDispatch {
				state = domain.ReservationReleased
			}
			if reservation.State != state {
				t.Errorf("ordinal %d dimension %s state=%s; want %s", physical.Ordinal, reservation.Dimension, reservation.State, state)
			}
			switch reservation.Dimension {
			case domain.BudgetLLMInputTokens:
				total.InputTokens += *reservation.SettledValue
			case domain.BudgetLLMOutputTokens:
				total.OutputTokens += *reservation.SettledValue
			}
		}
		if len(seen) != 4 {
			t.Errorf("ordinal %d has %d reservation dimensions; want four", physical.Ordinal, len(seen))
		}
	}
	return total
}

func llmAcceptanceCharge(input, output, calls, cost int64) map[domain.BudgetDimension]int64 {
	return map[domain.BudgetDimension]int64{
		domain.BudgetLLMInputTokens: input, domain.BudgetLLMOutputTokens: output,
		domain.BudgetLLMCalls: calls, domain.BudgetExternalCostMicroUSD: cost,
	}
}

func TestLLMCallsAcceptanceRetryUsageMatchesDatabase(t *testing.T) {
	t.Setenv("CPGEN_DURABLE_TEST_KEY", "acceptance-fixture-key")
	for _, status := range []int{429, 500, 502, 503, 504} {
		for _, exhausted := range []bool{false, true} {
			t.Run(fmt.Sprintf("HTTP-%d/exhausted-%t", status, exhausted), func(t *testing.T) {
				var calls atomic.Int32
				var mu sync.Mutex
				var keys []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					n := calls.Add(1)
					mu.Lock()
					keys = append(keys, r.Header.Get("Idempotency-Key"))
					mu.Unlock()
					if exhausted || n < 3 {
						w.WriteHeader(status)
					}
					// Even a complete low usage object on HTTP failure is unverified.
					_, _ = io.WriteString(w, llmAcceptanceResponse)
				}))
				defer server.Close()
				model, request := durableTestProvider(t, server.URL)
				fixture := newCoordinatorFixture(t, "b1", llmAcceptanceLimits())
				open, request, plan := llmAcceptanceBind(t, fixture, model, request, 3)
				service := llmAcceptanceService(t, fixture.store, model, fixture.clock)
				outcome, err := service.Generate(context.Background(), open, request)
				if err != nil {
					t.Fatalf("generate: %v", err)
				}
				if err := outcome.Validate(); err != nil {
					t.Errorf("invalid metered outcome: %v", err)
				}
				prepared := llmAcceptanceLoad(t, fixture, open.ID)
				bound := llmAcceptanceCharge(plan.InputTokenUpperBound, plan.OutputTokenUpperBound, 1, 100)
				last := llmAcceptanceCharge(3, 4, 1, 100)
				if exhausted {
					last = bound
				}
				total := llmAcceptanceSettlements(t, prepared, []map[domain.BudgetDimension]int64{bound, bound, last})
				if prepared.CallTrace == nil || !outcome.CallTrace.Equal(*prepared.CallTrace) || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 3 {
					t.Error("returned trace differs from the three durable physical attempts")
				}
				if exhausted {
					if outcome.Value != nil || outcome.Failure == nil || outcome.Failure.Class != domain.FailureRetryable {
						t.Error("exhausted retries did not return their terminal failure")
					}
				} else if outcome.Value == nil {
					t.Error("third response did not succeed")
				} else {
					value := outcome.Value
					if value.Usage != total || !value.CallTrace.Equal(outcome.CallTrace) {
						t.Errorf("response usage/trace differs from DB: usage=%+v DB=%+v", value.Usage, total)
					}
					if value.ProviderMeta["attempt_count"] != "3" || value.ProviderMeta["usage_source"] != "durable_ledger" || value.ProviderMeta["usage_settlement"] != "durable_ledger" {
						t.Error("successful response does not identify durable ledger totals")
					}
				}
				before := llmAcceptanceLoad(t, fixture, open.ID)
				_, replayErr := service.Generate(context.Background(), open, request)
				if exhausted && replayErr != nil {
					t.Errorf("terminal failure replay: %v", replayErr)
				}
				if !exhausted && !errors.Is(replayErr, durable.ErrLLMReplayUnavailable) {
					t.Error("success replay must explicitly return ErrLLMReplayUnavailable at this checkpoint")
				}
				if after := llmAcceptanceLoad(t, fixture, open.ID); !reflect.DeepEqual(before, after) {
					t.Error("terminal replay changed settled ledger rows")
				}
				mu.Lock()
				defer mu.Unlock()
				if calls.Load() != 3 || len(keys) != 3 {
					t.Fatalf("retry/replay made %d HTTP calls; want three", calls.Load())
				}
				if keys[0] == "" || keys[0] != keys[1] || keys[1] != keys[2] {
					t.Error("durable physical retries changed provider idempotency key")
				}
				if len(fixture.clock.delays()) != 2 {
					t.Error("application must own exactly two retry waits")
				}
			})
		}
	}
}

func TestLLMCallsAcceptanceBudgetAndMissingKeySendNothing(t *testing.T) {
	for _, name := range []string{"call-budget", "input-budget", "output-budget", "cost-budget", "missing-key"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CPGEN_DURABLE_TEST_KEY", "acceptance-fixture-key")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = io.WriteString(w, llmAcceptanceResponse)
			}))
			defer server.Close()
			limits := llmAcceptanceLimits()
			switch name {
			case "call-budget":
				limits.MaxLLMCalls = 1
			case "input-budget":
				limits.MaxLLMInputTokens = 1
			case "output-budget":
				limits.MaxLLMOutputTokens = 1
			case "cost-budget":
				limits.MaxLLMCostMicroUSD = 1
			case "missing-key":
				t.Setenv("CPGEN_DURABLE_TEST_KEY", "")
			}
			model, request := durableTestProvider(t, server.URL)
			fixture := newCoordinatorFixture(t, "b2", limits)
			open, request, _ := llmAcceptanceBind(t, fixture, model, request, 2)
			service := llmAcceptanceService(t, fixture.store, model, fixture.clock)
			outcome, err := service.Generate(context.Background(), open, request)
			if err != nil {
				t.Fatalf("local rejection: %v", err)
			}
			code := domain.FailureBudgetExhausted
			if name == "missing-key" {
				code = domain.FailurePolicyRejected
			}
			if outcome.Value != nil || outcome.Failure == nil || outcome.Failure.Code != code || outcome.CallTrace.DispatchKind != domain.DispatchNone || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 0 {
				t.Error("local rejection did not preserve a no-dispatch failure trace")
			}
			prepared := llmAcceptanceLoad(t, fixture, open.ID)
			if name == "missing-key" {
				zero := llmAcceptanceCharge(0, 0, 0, 0)
				llmAcceptanceSettlements(t, prepared, []map[domain.BudgetDimension]int64{zero, zero})
			} else if len(prepared.Reservations) != 0 {
				t.Error("budget rejection retained reservations")
			}
			if calls.Load() != 0 {
				t.Errorf("local rejection sent %d HTTP requests", calls.Load())
			}
		})
	}
}

type llmAcceptanceProviderHook struct {
	port.PhysicalLLM
	after func(context.Context, port.PhysicalLLMResult, error)
}

func (p llmAcceptanceProviderHook) GeneratePhysical(ctx context.Context, r port.GenerateRequest, id domain.AttemptCallID) (port.PhysicalLLMResult, error) {
	result, err := p.PhysicalLLM.GeneratePhysical(ctx, r, id)
	if p.after != nil {
		p.after(ctx, result, err)
	}
	return result, err
}

type llmAcceptanceContextKey struct{}

type llmAcceptanceSettlementLedger struct {
	port.CallLedger
	t                           *testing.T
	marked, completed, finished int
}

func (l *llmAcceptanceSettlementLedger) check(ctx context.Context) {
	l.t.Helper()
	deadline, ok := ctx.Deadline()
	if ctx.Err() != nil || !ok || time.Until(deadline) <= 0 || time.Until(deadline) > 5*time.Second+time.Millisecond*100 {
		l.t.Error("post-dispatch settlement lacks a live bounded cleanup context")
	}
	if ctx.Value(llmAcceptanceContextKey{}) != "preserved" {
		l.t.Error("cleanup context lost caller values")
	}
}

func (l *llmAcceptanceSettlementLedger) MarkSent(ctx context.Context, grant domain.DispatchGrant, at time.Time) error {
	l.check(ctx)
	l.marked++
	return l.CallLedger.MarkSent(ctx, grant, at)
}

func (l *llmAcceptanceSettlementLedger) CompletePhysical(ctx context.Context, request domain.CompletePhysicalRequest) error {
	l.check(ctx)
	l.completed++
	return l.CallLedger.CompletePhysical(ctx, request)
}

func (l *llmAcceptanceSettlementLedger) FinishCall(ctx context.Context, request domain.FinishCallRequest) (domain.CallTrace, error) {
	l.check(ctx)
	l.finished++
	return l.CallLedger.FinishCall(ctx, request)
}

func TestLLMCallsAcceptanceCancellationSettlesDispatchedWork(t *testing.T) {
	for _, name := range []string{"completed-response", "unknown-EOF", "retryable-response"} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("CPGEN_DURABLE_TEST_KEY", "acceptance-fixture-key")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if name == "unknown-EOF" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error("could not simulate EOF")
						return
					}
					_ = conn.Close()
					return
				}
				if name == "retryable-response" {
					w.WriteHeader(429)
				}
				_, _ = io.WriteString(w, llmAcceptanceResponse)
			}))
			defer server.Close()
			model, request := durableTestProvider(t, server.URL)
			fixture := newCoordinatorFixture(t, "b3", llmAcceptanceLimits())
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), llmAcceptanceContextKey{}, "preserved"))
			defer cancel()
			provider := llmAcceptanceProviderHook{PhysicalLLM: model, after: func(got context.Context, _ port.PhysicalLLMResult, _ error) {
				if got != ctx {
					t.Error("new physical dispatch did not use the original caller context")
				}
				cancel() // Exact boundary: provider result exists, ledger cleanup has not begun.
			}}
			open, request, plan := llmAcceptanceBind(t, fixture, provider, request, 2)
			ledger := &llmAcceptanceSettlementLedger{CallLedger: fixture.store, t: t}
			service := llmAcceptanceService(t, ledger, provider, fixture.clock)
			outcome, err := service.Generate(ctx, open, request)
			if name == "retryable-response" {
				if !errors.Is(err, context.Canceled) {
					t.Errorf("canceled retry should stop with context.Canceled: %v", err)
				}
			} else if err != nil {
				t.Errorf("cancellation prevented dispatched settlement: %v", err)
			}
			prepared := llmAcceptanceLoad(t, fixture, open.ID)
			if calls.Load() != 1 || ledger.completed != 1 {
				t.Errorf("HTTP/complete counts=%d/%d; want one/one", calls.Load(), ledger.completed)
			}
			for _, physical := range prepared.PhysicalCalls {
				if physical.State == domain.PhysicalDispatching || physical.State == domain.PhysicalSent {
					t.Errorf("cancellation left ordinal %d in %s", physical.Ordinal, physical.State)
				}
			}
			if name == "retryable-response" {
				// A canceled wait may leave the logical call resumable, but the
				// attempted work must already be settled and no next send may occur.
				if prepared.PhysicalCalls[0].State != domain.PhysicalCompleted {
					t.Error("retry cancellation lost completed HTTP evidence")
				}
				for _, reservation := range prepared.Reservations {
					if reservation.AttemptCallID == prepared.PhysicalCalls[0].ID && (reservation.State != domain.ReservationSettled || reservation.SettledValue == nil || *reservation.SettledValue != reservation.UpperBound) {
						t.Error("retry cancellation left attempted usage unsettled")
					}
				}
				return
			}
			first := llmAcceptanceCharge(3, 4, 1, 100)
			if name == "unknown-EOF" {
				first = llmAcceptanceCharge(plan.InputTokenUpperBound, plan.OutputTokenUpperBound, 1, 100)
				if outcome.Failure == nil || outcome.Failure.Code != domain.FailureBoundaryUnknown || prepared.PhysicalCalls[0].State != domain.PhysicalUnknown {
					t.Error("EOF cancellation lost UNKNOWN evidence")
				}
			}
			total := llmAcceptanceSettlements(t, prepared, []map[domain.BudgetDimension]int64{first, llmAcceptanceCharge(0, 0, 0, 0)})
			if name == "completed-response" && (outcome.Value == nil || outcome.Value.Usage != total || ledger.marked != 1) {
				t.Error("completed response canceled before cleanup lost success accounting")
			}
			if ledger.finished != 1 {
				t.Error("canceled execution did not finish its logical ledger record")
			}
			if name == "unknown-EOF" {
				_, err := service.Generate(context.Background(), open, request)
				if err != nil || calls.Load() != 1 {
					t.Error("UNKNOWN replay resent or failed to return its stored failure")
				}
			}
		})
	}
}

func TestLLMCallsAcceptancePersistedCancelAfterResponseStillSettles(t *testing.T) {
	t.Setenv("CPGEN_DURABLE_TEST_KEY", "acceptance-fixture-key")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, llmAcceptanceResponse)
	}))
	defer server.Close()
	model, request := durableTestProvider(t, server.URL)
	fixture := newCoordinatorFixture(t, "b4", llmAcceptanceLimits())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := llmAcceptanceProviderHook{PhysicalLLM: model, after: func(_ context.Context, result port.PhysicalLLMResult, err error) {
		if err != nil || result.Execution.Boundary != domain.BoundaryCompleted || result.Execution.Value == nil {
			t.Fatal("persistent-cancel fixture needs a completed successful HTTP response")
		}
		id := domain.ControlRequestID("control_00000000000000000000000000000b40")
		_, err = fixture.store.RequestCancel(context.Background(), domain.CancelRequest{
			ID: id, RunID: fixture.runID, ExpectedRunVersion: 2,
			Reason: "acceptance cancellation after provider response", IdempotencyKey: string(id), At: fixture.clock.Now(),
		})
		if err != nil {
			t.Fatalf("persist cancel request: %v", err)
		}
		cancel()
	}}
	open, request, _ := llmAcceptanceBind(t, fixture, provider, request, 2)
	service := llmAcceptanceService(t, fixture.store, provider, fixture.clock)
	_, err := service.Generate(ctx, open, request)
	if err != nil {
		t.Errorf("completed provider response could not settle after persisted cancel: %v", err)
	}
	prepared := llmAcceptanceLoad(t, fixture, open.ID)
	llmAcceptanceSettlements(t, prepared, []map[domain.BudgetDimension]int64{llmAcceptanceCharge(3, 4, 1, 100), llmAcceptanceCharge(0, 0, 0, 0)})
	if prepared.PhysicalCalls[0].State != domain.PhysicalCompleted {
		t.Errorf("known completed response remained %s after persistent cancel", prepared.PhysicalCalls[0].State)
	}
	if calls.Load() != 1 {
		t.Errorf("persistent cancellation sent %d HTTP calls; want one", calls.Load())
	}
}

type llmAcceptanceCrashLedger struct{ port.CallLedger }

var errLLMAcceptanceCrash = errors.New("acceptance simulated process loss after durable BeginDispatch")

func (l llmAcceptanceCrashLedger) BeginDispatch(ctx context.Context, request domain.BeginDispatchRequest) (domain.DispatchGrant, error) {
	grant, err := l.CallLedger.BeginDispatch(ctx, request)
	if err != nil {
		return grant, err
	}
	return domain.DispatchGrant{}, errLLMAcceptanceCrash
}

func TestLLMCallsAcceptanceResumeDispatchingIsUnknownWithoutHTTP(t *testing.T) {
	t.Setenv("CPGEN_DURABLE_TEST_KEY", "acceptance-fixture-key")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, llmAcceptanceResponse)
	}))
	defer server.Close()
	model, request := durableTestProvider(t, server.URL)
	fixture := newCoordinatorFixture(t, "b5", llmAcceptanceLimits())
	open, request, plan := llmAcceptanceBind(t, fixture, model, request, 2)
	crashing := llmAcceptanceService(t, llmAcceptanceCrashLedger{fixture.store}, model, fixture.clock)
	if _, err := crashing.Generate(context.Background(), open, request); !errors.Is(err, errLLMAcceptanceCrash) {
		t.Fatalf("could not stage durable DISPATCHING boundary: %v", err)
	}
	staged := llmAcceptanceLoad(t, fixture, open.ID)
	if staged.PhysicalCalls[0].State != domain.PhysicalDispatching || calls.Load() != 0 {
		t.Fatal("crash fixture did not stop between authorization and HTTP")
	}
	restarted := llmAcceptanceService(t, fixture.store, model, fixture.clock)
	outcome, err := restarted.Generate(context.Background(), open, request)
	if err != nil {
		t.Fatalf("resume dispatching: %v", err)
	}
	if outcome.Value != nil || outcome.Failure == nil || outcome.Failure.Code != domain.FailureBoundaryUnknown || len(outcome.CallTrace.PhysicalAttemptCallIDs) != 1 || outcome.CallTrace.PhysicalAttemptCallIDs[0] != staged.PhysicalCalls[0].ID {
		t.Error("resumed dispatch authority was not converted to the original UNKNOWN attempt")
	}
	prepared := llmAcceptanceLoad(t, fixture, open.ID)
	llmAcceptanceSettlements(t, prepared, []map[domain.BudgetDimension]int64{
		llmAcceptanceCharge(plan.InputTokenUpperBound, plan.OutputTokenUpperBound, 1, 100), llmAcceptanceCharge(0, 0, 0, 0),
	})
	if _, err := restarted.Generate(context.Background(), open, request); err != nil {
		t.Errorf("UNKNOWN terminal replay: %v", err)
	}
	if calls.Load() != 0 {
		t.Errorf("resumed DISPATCHING/UNKNOWN made %d HTTP sends; want zero", calls.Load())
	}
}

// A controlled clock observes the chosen wait without spending real Retry-After
// time. The physical adapter's timestamp is normalized to this same time source.
type llmAcceptanceClock struct {
	now    time.Time
	delays []time.Duration
}

func (c *llmAcceptanceClock) Now() time.Time { return c.now }
func (c *llmAcceptanceClock) After(d time.Duration) <-chan time.Time {
	c.delays = append(c.delays, d)
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return ch
}

type llmAcceptanceRetryProvider struct {
	port.PhysicalLLM
	source     *llmAcceptanceClock
	retryAfter time.Duration
}

func (p llmAcceptanceRetryProvider) GeneratePhysical(ctx context.Context, request port.GenerateRequest, id domain.AttemptCallID) (port.PhysicalLLMResult, error) {
	result, err := p.PhysicalLLM.GeneratePhysical(ctx, request, id)
	if result.Execution.Failure != nil {
		at := p.source.Now().Add(p.retryAfter)
		result.Execution.Failure.RetryAfter = &at
	}
	return result, err
}

func TestLLMCallsAcceptanceRetryWaitUsesMaximumBackoffAndRetryAfter(t *testing.T) {
	for _, delay := range []time.Duration{-time.Second, time.Nanosecond, 7 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			t.Setenv("CPGEN_DURABLE_TEST_KEY", "acceptance-fixture-key")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if calls.Add(1) == 1 {
					w.Header().Set("Retry-After", strconv.Itoa(int(delay/time.Second)))
					w.WriteHeader(429)
				}
				_, _ = io.WriteString(w, llmAcceptanceResponse)
			}))
			defer server.Close()
			model, request := durableTestProvider(t, server.URL)
			fixture := newCoordinatorFixture(t, "b6", llmAcceptanceLimits())
			source := &llmAcceptanceClock{now: fixture.clock.Now().Add(time.Second)}
			provider := llmAcceptanceRetryProvider{PhysicalLLM: model, source: source, retryAfter: delay}
			open, request, _ := llmAcceptanceBind(t, fixture, provider, request, 2)
			service := llmAcceptanceService(t, fixture.store, provider, source)
			outcome, err := service.Generate(context.Background(), open, request)
			if err != nil || outcome.Value == nil {
				t.Fatalf("retry wait execution failed: %v", err)
			}
			want := open.RetryPolicy.Backoff(1)
			if delay > want {
				want = delay
			}
			if len(source.delays) != 1 || source.delays[0] != want {
				t.Errorf("retry waits=%v; want [%v]", source.delays, want)
			}
			if calls.Load() != 2 {
				t.Errorf("retry wait caused %d HTTP calls; want two", calls.Load())
			}
		})
	}
}

func TestLLMCallsAcceptanceCancelSettlementAuthorityStaysNarrow(t *testing.T) {
	for _, name := range []string{"ordinary-stale-version", "cancel-after-attempt-change", "new-dispatch-during-cancel"} {
		t.Run(name, func(t *testing.T) {
			limits := llmAcceptanceLimits()
			limits.MaxActiveTimeMilliseconds = 1000
			fixture := newCoordinatorFixture(t, "b7", limits)
			ctx := context.Background()
			open := fixture.openRequest(702)
			plan := domain.CallPlan{
				Digest: domain.SumBytes([]byte("acceptance narrow settlement authority")),
				Calls:  []domain.PhysicalCallPlan{coordinatorPhysicalPlan(1, 1), coordinatorPhysicalPlan(2, 2)},
			}
			stageCoordinatorBoundary(t, fixture, open, plan, "DISPATCHING")
			grant, err := fixture.store.ResumeDispatch(ctx, 2, plan.Calls[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if name == "ordinary-stale-version" {
				// A legitimate non-cancel event also advances the version by one.
				_, err = fixture.store.AccountActiveTime(ctx, domain.ActiveTimeCommand{
					RunID: fixture.runID, ExpectedRunVersion: 2, Action: domain.ActiveTimeStart,
					IdempotencyKey: coordinatorID("active", name), At: fixture.clock.Now(),
				})
			} else {
				id := domain.ControlRequestID("control_00000000000000000000000000000b70")
				_, err = fixture.store.RequestCancel(ctx, domain.CancelRequest{
					ID: id, RunID: fixture.runID, ExpectedRunVersion: 2,
					Reason: "acceptance narrow authority", IdempotencyKey: string(id), At: fixture.clock.Now(),
				})
			}
			if err != nil {
				t.Fatalf("stage version advance: %v", err)
			}
			if name == "cancel-after-attempt-change" {
				// Seed a replacement current attempt in this isolated fixture.
				// Keep the +1 pending-cancel condition true to isolate the attempt guard.
				db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(fixture.path)+"?_pragma=foreign_keys(1)")
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				replacement := domain.AttemptID("attempt_00000000000000000000000000000b71")
				if _, err := db.Exec(`INSERT INTO stage_attempts
					(attempt_id, run_id, stage_name, ordinal, state, input_digest, started_at)
					SELECT ?, run_id, stage_name, 2, 'RUNNING', input_digest, started_at
					FROM stage_attempts WHERE attempt_id = ?`, replacement, fixture.attemptID); err != nil {
					t.Fatalf("seed replacement attempt: %v", err)
				}
				if _, err := db.Exec(`UPDATE stage_records SET current_attempt_id = ?, attempt_count = 2
					WHERE run_id = ? AND stage_name = ?`, replacement, fixture.runID, open.StageName); err != nil {
					t.Fatalf("select replacement attempt: %v", err)
				}
			}
			before := llmAcceptanceLoad(t, fixture, open.ID)
			if name != "new-dispatch-during-cancel" {
				if err := fixture.store.MarkSent(ctx, grant, fixture.clock.Now()); err == nil {
					t.Error("stale or replaced-attempt authority recorded a new SENT receipt")
				}
				if err := fixture.store.CompletePhysical(ctx, domain.CompletePhysicalRequest{
					RunID: open.RunID, ExpectedRunVersion: 2, StageName: open.StageName,
					AttemptID: open.AttemptID, CallRecordID: open.ID, AttemptCallID: grant.AttemptCallID,
					State: domain.PhysicalUnknown, Outcome: domain.PhysicalOutcomeUnknown,
					Failure:        &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown},
					IdempotencyKey: coordinatorID("complete", name), At: fixture.clock.Now(),
				}); err == nil {
					t.Error("stale or replaced-attempt authority settled physical usage")
				}
			} else {
				for _, version := range []int64{2, 3} {
					if _, err := fixture.store.BeginDispatch(ctx, domain.BeginDispatchRequest{
						RunID: open.RunID, ExpectedRunVersion: version, StageName: open.StageName,
						AttemptID: open.AttemptID, CallRecordID: open.ID, AttemptCallID: plan.Calls[1].ID,
						IdempotencyKey: coordinatorID("begin", fmt.Sprint(version)), At: fixture.clock.Now(),
					}); err == nil {
						t.Errorf("pending cancel allowed new dispatch with expected version %d", version)
					}
				}
			}
			if after := llmAcceptanceLoad(t, fixture, open.ID); !reflect.DeepEqual(before, after) {
				t.Error("rejected authority changed physical calls or reservations")
			}
		})
	}
}
