package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// ErrLLMReplayUnavailable prevents a completed model response from being
// recreated by another paid request when private artifacts are unavailable.
var ErrLLMReplayUnavailable = errors.New("completed LLM response requires verified private artifact replay")

// LLMCalls connects physical provider calls to the existing durable ledger.
// The foreground executor must hold its run lock throughout Generate. This
// NewReplayableLLMCalls also publishes and restores private response artifacts.
type LLMCalls struct {
	mu             sync.Mutex
	ledger         port.CallLedger
	provider       port.PhysicalLLM
	clock          clock.Clock
	costUpperBound int64
	artifacts      *llmResponseArtifacts
}

func NewLLMCalls(ledger port.CallLedger, provider port.PhysicalLLM, source clock.Clock, costUpperBoundMicroUSD int64) (*LLMCalls, error) {
	if ledger == nil || provider == nil || source == nil || costUpperBoundMicroUSD <= 0 {
		return nil, errors.New("LLM calls require a ledger, provider, clock and positive per-request cost ceiling")
	}
	return &LLMCalls{ledger: ledger, provider: provider, clock: source, costUpperBound: costUpperBoundMicroUSD}, nil
}

func (s *LLMCalls) Generate(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (domain.MeteredOutcome[port.GenerateResponse], error) {
	return s.generate(ctx, open, request, false)
}

// Reconcile can only restore and settle an existing provider operation. It
// cannot create a missing call, reserve work or send another physical request.
func (s *LLMCalls) Reconcile(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (domain.MeteredOutcome[port.GenerateResponse], error) {
	return s.generate(ctx, open, request, true)
}

func (s *LLMCalls) generate(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest, reconcile bool) (domain.MeteredOutcome[port.GenerateResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var empty domain.MeteredOutcome[port.GenerateResponse]
	if ctx == nil {
		return empty, errors.New("LLM call context is required")
	}
	if err := open.Validate(); err != nil {
		return empty, err
	}
	// A coordinator instance owns an immutable request and a set of freshly
	// issued grants. A resumed DISPATCHING grant never regains send authority.
	request.Variables = append(json.RawMessage(nil), request.Variables...)
	plan, err := s.provider.PlanGenerate(request)
	if err != nil {
		return empty, err
	}
	if open.Kind != domain.CallLLMGenerate || open.RequestDigest != plan.RequestDigest || open.Provider != plan.Provider || open.PolicyDigest != request.ProviderPolicyDigest || open.LogicalOperationID != request.LogicalIdempotencyKey || open.RetryPolicy.MaxAttempts > 8 || plan.InputTokenUpperBound <= 0 || plan.OutputTokenUpperBound <= 0 {
		return empty, errors.New("LLM logical call differs from admitted provider request or retry bound")
	}
	ledger := &freshDispatchLedger{CallLedger: s.ledger, grants: map[domain.AttemptCallID]domain.DispatchGrant{}}
	adapter := &llmCallAdapter{ledger: ledger, provider: s.provider, request: request, plan: plan, costUpperBound: s.costUpperBound}
	if s.artifacts != nil {
		adapter.responses, err = s.artifacts.session(open, request)
		if err != nil {
			return empty, err
		}
	}
	var outcome domain.MeteredOutcome[port.GenerateResponse]
	if reconcile {
		settlement, ok := s.ledger.(CallReconciliationLedger)
		if !ok {
			return empty, errors.New("LLM reconciliation requires original logical call reads")
		}
		coordinator, createErr := NewCallReconciler[port.GenerateResponse](settlement, adapter, s.clock)
		if createErr != nil {
			return empty, createErr
		}
		outcome, err = coordinator.Reconcile(ctx, open)
	} else {
		coordinator, createErr := NewCallCoordinator[port.GenerateResponse](ledger, adapter, s.clock)
		if createErr != nil {
			return empty, createErr
		}
		outcome, err = coordinator.Execute(ctx, open)
	}
	if adapter.responses != nil && outcome.CallTrace.Validate() == nil {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		releaseErr := releasePrivateResponseSlots(releaseCtx, adapter.responses.privateResponseSession, reconcile)
		cancel()
		if releaseErr != nil {
			return empty, errors.Join(err, releaseErr)
		}
	}
	if err != nil {
		return outcome, err
	}
	if outcome.Value != nil {
		outcome.Value.CallTrace = outcome.CallTrace
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		settled, loadErr := s.ledger.LoadCall(readCtx, open.ID)
		if loadErr != nil {
			return empty, loadErr
		}
		var usage port.Usage
		for _, reservation := range settled.Reservations {
			if reservation.SettledValue == nil {
				continue
			}
			switch reservation.Dimension {
			case domain.BudgetLLMInputTokens:
				usage.InputTokens += *reservation.SettledValue
			case domain.BudgetLLMOutputTokens:
				usage.OutputTokens += *reservation.SettledValue
			}
		}
		outcome.Value.Usage = usage
		if outcome.Value.ProviderMeta == nil {
			outcome.Value.ProviderMeta = map[string]string{}
		}
		outcome.Value.ProviderMeta["usage_settlement"] = "durable_ledger"
		outcome.Value.ProviderMeta["usage_source"] = "durable_ledger"
		outcome.Value.ProviderMeta["attempt_count"] = strconv.Itoa(len(outcome.CallTrace.PhysicalAttemptCallIDs))
	}
	return outcome, nil
}

type llmCallAdapter struct {
	ledger         *freshDispatchLedger
	provider       port.PhysicalLLM
	request        port.GenerateRequest
	plan           port.LLMRequestPlan
	costUpperBound int64
	responses      *llmResponseSession
}

func (a *llmCallAdapter) Replay(ctx context.Context, grant domain.DispatchGrant) (domain.PhysicalExecution[port.GenerateResponse], error) {
	if len(a.ledger.grants) != 0 {
		return domain.PhysicalExecution[port.GenerateResponse]{}, errReconciliationNewWork
	}
	return a.Execute(ctx, grant)
}

func (a *llmCallAdapter) Plan(ctx context.Context, record domain.CallRecord) (domain.CallPlanDecision, error) {
	if a.responses != nil {
		failure, err := a.responses.prepare(ctx)
		if err != nil {
			return domain.CallPlanDecision{}, err
		}
		if failure != nil {
			return domain.CallPlanDecision{Failure: failure}, nil
		}
	}
	calls := make([]domain.PhysicalCallPlan, record.RetryPolicy.MaxAttempts)
	dimensions := []domain.BudgetDimension{domain.BudgetLLMCalls, domain.BudgetLLMInputTokens, domain.BudgetLLMOutputTokens, domain.BudgetExternalCostMicroUSD}
	bounds := []int64{1, a.plan.InputTokenUpperBound, a.plan.OutputTokenUpperBound, a.costUpperBound}
	for i := range calls {
		id := domain.AttemptCallID(MutationID("call", record.ID, i+1))
		call := domain.PhysicalCallPlan{ID: id, Ordinal: int64(i + 1), RetryGroup: "llm-transport", RetryOrdinal: int64(i + 1), Kind: domain.PhysicalLLMRequest, Provider: a.plan.Provider, RequestDigest: a.plan.RequestDigest, IdempotencyKey: MutationID("physical", record.ID, i+1)}
		for j, dimension := range dimensions {
			call.Reservations = append(call.Reservations, domain.ReservationPlan{ID: domain.ReservationID(MutationID("res", id, dimension)), Dimension: dimension, Subkey: "request", UpperBound: bounds[j]})
		}
		calls[i] = call
	}
	raw, err := json.Marshal(calls)
	if err != nil {
		return domain.CallPlanDecision{}, err
	}
	return domain.CallPlanDecision{Plan: &domain.CallPlan{Digest: domain.SumBytes(raw), Calls: calls}}, nil
}

func (a *llmCallAdapter) Execute(ctx context.Context, grant domain.DispatchGrant) (domain.PhysicalExecution[port.GenerateResponse], error) {
	var empty domain.PhysicalExecution[port.GenerateResponse]
	prepared, err := a.ledger.LoadCall(ctx, grant.CallRecordID)
	if err != nil {
		return empty, err
	}
	if grant.Kind != domain.PhysicalLLMRequest || grant.RequestDigest != a.plan.RequestDigest || grant.Provider != a.plan.Provider {
		return empty, errors.New("LLM dispatch grant differs from admitted request")
	}
	for _, physical := range prepared.PhysicalCalls {
		if physical.ID != grant.AttemptCallID {
			continue
		}
		_, fresh := a.ledger.grants[grant.AttemptCallID]
		if !fresh || physical.State == domain.PhysicalCompleted {
			if a.responses != nil {
				replayed, found, err := a.responses.replay(ctx, grant, a.provider)
				if err != nil {
					return empty, err
				}
				if found {
					return replayed, nil
				}
			}
			if physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomeSuccess {
				return empty, ErrLLMReplayUnavailable
			}
		}
	}
	authorized, fresh := a.ledger.grants[grant.AttemptCallID]
	delete(a.ledger.grants, grant.AttemptCallID)
	if !fresh || authorized != grant {
		return domain.PhysicalExecution[port.GenerateResponse]{Boundary: domain.BoundaryUnknown, Failure: &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}}, nil
	}
	var writer port.ArtifactWriter
	if a.responses != nil {
		writer, err = a.responses.writer(ctx, grant)
		if err != nil {
			return domain.PhysicalExecution[port.GenerateResponse]{Boundary: domain.BoundaryConfirmedNoSend, Failure: &domain.PortFailure{Code: domain.FailureUnavailable, Class: domain.FailureBlocked}}, nil
		}
	}
	result, err := a.provider.GeneratePhysical(ctx, a.request, grant.AttemptCallID)
	if err != nil {
		if writer != nil {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = a.responses.abort(abortCtx, grant, writer)
			cancel()
		}
		return empty, fmt.Errorf("LLM physical execution: %w", err)
	}
	execution := result.Execution
	if execution.Boundary != domain.BoundaryConfirmedNoSend {
		for _, reservation := range prepared.Reservations {
			if reservation.AttemptCallID != grant.AttemptCallID {
				continue
			}
			value, verified := reservation.UpperBound, false
			switch reservation.Dimension {
			case domain.BudgetLLMCalls:
				value, verified = 1, true
			case domain.BudgetLLMInputTokens:
				value, verified = result.Usage.InputTokens, result.UsageVerified
			case domain.BudgetLLMOutputTokens:
				value, verified = result.Usage.OutputTokens, result.UsageVerified
			}
			execution.Usage = append(execution.Usage, domain.ReservationUsage{ReservationID: reservation.ID, Dimension: reservation.Dimension, Subkey: reservation.Subkey, Value: value, Verified: verified})
		}
	}
	if writer != nil {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if execution.Value == nil && result.FormatRepair == nil {
			if err := a.responses.abort(persistCtx, grant, writer); err != nil {
				return empty, err
			}
		} else {
			if err := a.responses.publish(persistCtx, grant, writer, &execution, a.provider, result.FormatRepair); err != nil {
				return empty, err
			}
		}
	}
	return execution, nil
}
