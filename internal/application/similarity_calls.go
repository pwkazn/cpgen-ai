package application

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
)

var ErrSimilarityReplayUnavailable = errors.New("completed similarity response requires verified private artifact replay")

// SimilarityCalls owns the durable physical budget/dispatch boundary. The
// foreground caller holds the run lock throughout Search. This ledger-only
// constructor cannot reconstruct a completed response without private bytes.
type SimilarityCalls struct {
	mu             sync.Mutex
	ledger         port.CallLedger
	provider       similarity.PhysicalProvider
	clock          clock.Clock
	costUpperBound int64
	artifacts      *similarityResponseArtifacts
}

type SimilarityCallResult struct {
	Outcome  domain.MeteredOutcome[similarity.Evidence]
	Artifact *domain.PendingArtifact
}

func NewSimilarityCalls(ledger port.CallLedger, provider similarity.PhysicalProvider, source clock.Clock, costUpperBoundMicroUSD int64) (*SimilarityCalls, error) {
	if ledger == nil || provider == nil || source == nil || costUpperBoundMicroUSD <= 0 {
		return nil, errors.New("similarity calls require a ledger, provider, clock and positive cost ceiling")
	}
	return &SimilarityCalls{ledger: ledger, provider: provider, clock: source, costUpperBound: costUpperBoundMicroUSD}, nil
}

func (s *SimilarityCalls) Search(ctx context.Context, open domain.OpenCallRequest, request similarity.Request) (domain.MeteredOutcome[similarity.Evidence], error) {
	result, err := s.SearchWithArtifacts(ctx, open, request)
	return result.Outcome, err
}

// SearchWithArtifacts returns the single producing receipt for atomic stage
// attachment. Evidence remains a semantic value independent of blob location.
func (s *SimilarityCalls) SearchWithArtifacts(ctx context.Context, open domain.OpenCallRequest, request similarity.Request) (SimilarityCallResult, error) {
	return s.searchWithArtifacts(ctx, open, request, false)
}

// Reconcile restores only an existing operation and never starts a new query
// or transport retry. A missing operation remains an explicit not-found error.
func (s *SimilarityCalls) Reconcile(ctx context.Context, open domain.OpenCallRequest, request similarity.Request) (SimilarityCallResult, error) {
	return s.searchWithArtifacts(ctx, open, request, true)
}

func (s *SimilarityCalls) searchWithArtifacts(ctx context.Context, open domain.OpenCallRequest, request similarity.Request, reconcile bool) (SimilarityCallResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result SimilarityCallResult
	var err error
	result.Outcome, err = s.search(ctx, open, request, &result.Artifact, reconcile)
	return result, err
}

func (s *SimilarityCalls) search(ctx context.Context, open domain.OpenCallRequest, request similarity.Request, artifact **domain.PendingArtifact, reconcile bool) (domain.MeteredOutcome[similarity.Evidence], error) {
	var empty domain.MeteredOutcome[similarity.Evidence]
	if ctx == nil {
		return empty, errors.New("similarity context is required")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := open.Validate(); err != nil {
		return empty, err
	}
	request.NormalizedTags = slices.Clone(request.NormalizedTags)
	plan, err := s.provider.PlanSearch(request)
	if err != nil {
		return empty, err
	}
	if open.Kind != domain.CallSimilaritySearch || open.Provider != plan.Provider || open.RequestDigest != plan.RequestDigest || open.PolicyDigest != plan.PolicyDigest || open.LogicalOperationID != request.LogicalIdempotencyKey || open.RetryPolicy.MaxAttempts > 8 || plan.MaxResponseBytes <= 0 || plan.MaxResponseBytes > 64<<20 {
		return empty, errors.New("similarity logical call differs from the admitted request or retry bound")
	}
	ledger := &freshDispatchLedger{CallLedger: s.ledger, grants: map[domain.AttemptCallID]domain.DispatchGrant{}}
	adapter := &similarityCallAdapter{ledger: ledger, provider: s.provider, request: request, plan: plan, costUpperBound: s.costUpperBound}
	if s.artifacts != nil {
		adapter.responses, err = s.artifacts.session(open, request, plan)
		if err != nil {
			return empty, err
		}
	}
	var outcome domain.MeteredOutcome[similarity.Evidence]
	if reconcile {
		settlement, ok := s.ledger.(CallReconciliationLedger)
		if !ok {
			return empty, errors.New("similarity reconciliation requires original logical call reads")
		}
		coordinator, createErr := NewCallReconciler[similarity.Evidence](settlement, adapter, s.clock)
		if createErr != nil {
			return empty, createErr
		}
		outcome, err = coordinator.Reconcile(ctx, open)
	} else {
		coordinator, createErr := NewCallCoordinator[similarity.Evidence](ledger, adapter, s.clock)
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
	if err != nil || outcome.Value == nil {
		return outcome, err
	}
	// Physical evidence identifies its producing exchange. The public logical
	// result also includes prior transport attempts from the authoritative trace.
	// Usage remains the producing response's provider report; total spend is
	// available from settled reservations, including failed attempts.
	value := outcome.Value
	evidence, err := similarity.NewEvidenceWithServiceIdentity(request, value.ProviderIdentity, value.ServiceIdentity, value.Hits, value.ObservedAt, value.Usage, value.UsageSource, value.ModelVersion, value.IndexVersion, value.Cache, outcome.CallTrace)
	if err != nil {
		return empty, err
	}
	outcome.Value = &evidence
	*artifact = adapter.artifact
	return outcome, nil
}

type similarityCallAdapter struct {
	ledger         *freshDispatchLedger
	provider       similarity.PhysicalProvider
	request        similarity.Request
	plan           similarity.PhysicalSearchPlan
	costUpperBound int64
	responses      *similarityResponseSession
	artifact       *domain.PendingArtifact
}

func (a *similarityCallAdapter) Replay(ctx context.Context, grant domain.DispatchGrant) (domain.PhysicalExecution[similarity.Evidence], error) {
	if len(a.ledger.grants) != 0 {
		return domain.PhysicalExecution[similarity.Evidence]{}, errReconciliationNewWork
	}
	return a.Execute(ctx, grant)
}

func (a *similarityCallAdapter) Plan(ctx context.Context, record domain.CallRecord) (domain.CallPlanDecision, error) {
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
	for i := range calls {
		id := domain.AttemptCallID(coordinatorMutationID("call", record.ID, i+1))
		call := domain.PhysicalCallPlan{ID: id, Ordinal: int64(i + 1), RetryGroup: "similarity-transport", RetryOrdinal: int64(i + 1), Kind: domain.PhysicalSimilarityRequest, Provider: a.plan.Provider, RequestDigest: a.plan.RequestDigest, IdempotencyKey: coordinatorMutationID("physical", record.ID, i+1)}
		for _, dimension := range []domain.BudgetDimension{domain.BudgetSimilarityCalls, domain.BudgetSimilarityCostMicroUSD} {
			upper := a.costUpperBound
			if dimension == domain.BudgetSimilarityCalls {
				upper = 1
			}
			call.Reservations = append(call.Reservations, domain.ReservationPlan{ID: domain.ReservationID(coordinatorMutationID("res", id, dimension)), Dimension: dimension, Subkey: "request", UpperBound: upper})
		}
		calls[i] = call
	}
	raw, err := json.Marshal(calls)
	if err != nil {
		return domain.CallPlanDecision{}, err
	}
	return domain.CallPlanDecision{Plan: &domain.CallPlan{Digest: domain.SumBytes(raw), Calls: calls}}, nil
}

func (a *similarityCallAdapter) Execute(ctx context.Context, grant domain.DispatchGrant) (domain.PhysicalExecution[similarity.Evidence], error) {
	var empty domain.PhysicalExecution[similarity.Evidence]
	prepared, err := a.ledger.LoadCall(ctx, grant.CallRecordID)
	if err != nil {
		return empty, err
	}
	if grant.Kind != domain.PhysicalSimilarityRequest || grant.RequestDigest != a.plan.RequestDigest || grant.Provider != a.plan.Provider {
		return empty, errors.New("similarity grant differs from the admitted provider request")
	}
	for _, physical := range prepared.PhysicalCalls {
		if physical.ID != grant.AttemptCallID {
			continue
		}
		_, fresh := a.ledger.grants[grant.AttemptCallID]
		if !fresh || physical.State == domain.PhysicalCompleted {
			if a.responses != nil {
				replayed, pending, found, err := a.responses.replay(ctx, grant, a.provider)
				if err != nil {
					return empty, err
				}
				if found {
					a.artifact = pending
					return replayed, nil
				}
			}
			if physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomeSuccess {
				return empty, ErrSimilarityReplayUnavailable
			}
		}
	}
	authorized, fresh := a.ledger.grants[grant.AttemptCallID]
	delete(a.ledger.grants, grant.AttemptCallID)
	if !fresh || authorized != grant {
		return domain.PhysicalExecution[similarity.Evidence]{Boundary: domain.BoundaryUnknown, Failure: &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}}, nil
	}
	var writer port.ArtifactWriter
	if a.responses != nil {
		writer, err = a.responses.writer(ctx, grant)
		if err != nil {
			return domain.PhysicalExecution[similarity.Evidence]{Boundary: domain.BoundaryConfirmedNoSend, Failure: &domain.PortFailure{Code: domain.FailureUnavailable, Class: domain.FailureBlocked}}, nil
		}
	}
	result, err := a.provider.SearchPhysical(ctx, a.request, grant.AttemptCallID)
	if err != nil {
		if writer != nil {
			abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = a.responses.abort(abortCtx, grant, writer)
			cancel()
		}
		return empty, err
	}
	execution := result.Execution
	if err := execution.Validate(); err != nil {
		return empty, err
	}
	if execution.Value != nil {
		if err := a.provider.ValidatePhysicalResponse(a.request, *execution.Value); err != nil {
			return empty, err
		}
		trace := execution.Value.CallTrace
		if len(trace.PhysicalAttemptCallIDs) != 1 || trace.PhysicalAttemptCallIDs[0] != grant.AttemptCallID {
			return empty, errors.New("similarity response substituted its physical identity")
		}
	}
	// Provider implementations report usage, but never choose ledger accounts.
	execution.Usage = nil
	if execution.Boundary != domain.BoundaryConfirmedNoSend {
		for _, reservation := range prepared.Reservations {
			if reservation.AttemptCallID != grant.AttemptCallID {
				continue
			}
			value, verified := reservation.UpperBound, false
			if reservation.Dimension == domain.BudgetSimilarityCalls {
				value, verified = 1, true
			}
			if reservation.Dimension == domain.BudgetSimilarityCostMicroUSD && result.CostVerified && execution.Value != nil {
				value, verified = execution.Value.Usage.CostMicroUSD, true
			}
			execution.Usage = append(execution.Usage, domain.ReservationUsage{ReservationID: reservation.ID, Dimension: reservation.Dimension, Subkey: reservation.Subkey, Value: value, Verified: verified})
		}
	}
	if writer != nil {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if execution.Value == nil {
			if err := a.responses.abort(persistCtx, grant, writer); err != nil {
				return empty, err
			}
		} else {
			a.artifact, err = a.responses.publish(persistCtx, grant, writer, execution, a.provider)
			if err != nil {
				return empty, err
			}
		}
	}
	return execution, nil
}
