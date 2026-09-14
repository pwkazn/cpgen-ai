package execution

import (
	"context"
	"errors"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

// CallReconciliationLedger has no opening, planning or dispatch-start methods.
// Cleanup can settle only existing operations in their original attempt.
type CallReconciliationLedger interface {
	ReadLogicalCall(context.Context, domain.CallRecordID) (domain.CallRecord, error)
	LoadCall(context.Context, domain.CallRecordID) (domain.PreparedCalls, error)
	ResumeDispatch(context.Context, int64, domain.AttemptCallID) (domain.DispatchGrant, error)
	MarkSent(context.Context, domain.DispatchGrant, time.Time) error
	CompletePhysical(context.Context, domain.CompletePhysicalRequest) error
	FinishCall(context.Context, domain.FinishCallRequest) (domain.CallTrace, error)
}

// CallReceiptReader may recover existing private publication, but cannot ask a
// provider for a new result. Unavailable or invalid receipts remain recoverable
// errors; a confirmed absence at an uncertain send boundary is an UNKNOWN value.
type CallReceiptReader[T any] interface {
	Replay(context.Context, domain.DispatchGrant) (domain.PhysicalExecution[T], error)
}

type CallReconciler[T any] struct {
	ledger     CallReconciliationLedger
	settlement *CallCoordinator[T]
}

func NewCallReconciler[T any](ledger CallReconciliationLedger, receipts CallReceiptReader[T], source clock.Clock) (*CallReconciler[T], error) {
	if ledger == nil || receipts == nil || source == nil {
		return nil, errors.New("call reconciliation requires a settlement ledger, receipt reader and clock")
	}
	// Reuse checked completion/trace helpers behind adapters that mechanically
	// refuse the three new-effect methods. Receipt replay replaces Execute.
	coordinator, err := NewCallCoordinator[T](reconciliationOnlyLedger{ledger}, receiptOnlyAdapter[T]{receipts}, source)
	if err != nil {
		return nil, err
	}
	return &CallReconciler[T]{ledger, coordinator}, nil
}

func (r *CallReconciler[T]) Reconcile(ctx context.Context, request domain.OpenCallRequest) (domain.MeteredOutcome[T], error) {
	var empty domain.MeteredOutcome[T]
	if ctx == nil {
		return empty, errors.New("call reconciliation requires a context")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := request.Validate(); err != nil {
		return empty, err
	}
	record, err := r.ledger.ReadLogicalCall(ctx, request.ID)
	if err != nil {
		return empty, err
	}
	if err := record.Validate(); err != nil {
		return empty, err
	}
	if record.ID != request.ID || record.RunID != request.RunID || record.StageName != request.StageName || record.AttemptID != request.AttemptID || record.Kind != request.Kind || record.Provider != request.Provider || record.RequestDigest != request.RequestDigest || record.PolicyDigest != request.PolicyDigest || record.LogicalOperationID != request.LogicalOperationID || record.RetryPolicy != request.RetryPolicy || record.IdempotencyKey != request.IdempotencyKey || !record.OpenedAt.Equal(request.At) {
		return empty, errors.New("call reconciliation differs from the original logical operation")
	}
	if (record.Kind != domain.CallLLMGenerate && record.Kind != domain.CallSimilaritySearch) || record.Provider == "private-blob" || record.RetryPolicy.MaxAttempts > 8 {
		return empty, errors.New("call reconciliation requires a bounded provider operation")
	}
	c := r.settlement
	noSend := &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
	if record.State == domain.CallRecordOpen {
		// LoadCall deliberately rejects an unprepared bundle. ReadLogicalCall
		// above preserves that contract; FinishCall checks there are no rows.
		return c.finish(ctx, record, request.ExpectedRunVersion, domain.DispatchNone, nil, noSend)
	}
	prepared, err := r.ledger.LoadCall(ctx, record.ID)
	if err != nil {
		return empty, err
	}
	if err := prepared.Validate(); err != nil {
		return empty, err
	}
	if err := validateReconciliationPlan(record, prepared); err != nil {
		return empty, err
	}
	if prepared.Call.State == domain.CallRecordTerminal {
		return c.replayTerminal(ctx, request.ExpectedRunVersion, prepared)
	}
	var lastDispatched *domain.AttemptCallID
	lastFailure := noSend
	for _, physical := range prepared.PhysicalCalls {
		switch physical.State {
		case domain.PhysicalPrepared:
			// FinishCall releases every remaining unused reservation atomically.
			return r.finish(ctx, record, request.ExpectedRunVersion, lastDispatched, lastFailure)
		case domain.PhysicalUnknown:
			return r.finish(ctx, record, request.ExpectedRunVersion, callIDPointer(physical.ID), physical.Failure)
		case domain.PhysicalCompleted, domain.PhysicalAbortedNoDispatch:
			if physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomeSuccess {
				value, err := c.replaySuccessfulPhysical(ctx, request.ExpectedRunVersion, physical)
				if err != nil {
					return empty, err
				}
				return c.finishValue(ctx, record, request.ExpectedRunVersion, physical.ID, value)
			}
			if physical.Failure == nil {
				return empty, errors.New("terminal physical call lacks failure evidence")
			}
			lastFailure = physical.Failure
			if physical.SentAt != nil {
				lastDispatched = callIDPointer(physical.ID)
			}
			// A later retry might already have been dispatched before the crash.
			// Inspect it without waiting or authorizing any unstarted retry.
			continue
		case domain.PhysicalDispatching, domain.PhysicalSent:
			grant, err := r.ledger.ResumeDispatch(ctx, request.ExpectedRunVersion, physical.ID)
			if err != nil {
				return empty, err
			}
			execution, err := c.adapter.Execute(ctx, grant)
			if err != nil {
				return empty, err
			}
			if err := execution.Validate(); err != nil {
				return empty, err
			}
			if execution.Boundary == domain.BoundaryConfirmedNoSend && physical.State == domain.PhysicalSent {
				return empty, errors.New("receipt reports no-send after a durable sent boundary")
			}
			settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if execution.Boundary == domain.BoundaryCompleted && physical.State != domain.PhysicalSent {
				if err := r.ledger.MarkSent(settleCtx, grant, c.clock.Now()); err != nil {
					return empty, err
				}
			}
			if err := c.complete(settleCtx, record, grant, execution); err != nil {
				return empty, err
			}
			if execution.Value != nil {
				return c.finishValue(settleCtx, record, request.ExpectedRunVersion, physical.ID, execution.Value)
			}
			if execution.Boundary != domain.BoundaryConfirmedNoSend {
				lastDispatched = callIDPointer(physical.ID)
			}
			return r.finish(settleCtx, record, request.ExpectedRunVersion, lastDispatched, execution.Failure)
		default:
			return empty, errors.New("unsupported physical reconciliation boundary")
		}
	}
	return r.finish(ctx, record, request.ExpectedRunVersion, lastDispatched, lastFailure)
}

func (r *CallReconciler[T]) finish(ctx context.Context, record domain.CallRecord, version int64, id *domain.AttemptCallID, failure *domain.PortFailure) (domain.MeteredOutcome[T], error) {
	dispatch := domain.DispatchNone
	if id != nil {
		dispatch = domain.DispatchDispatched
	}
	return r.settlement.finish(ctx, record, version, dispatch, id, failure)
}

func validateReconciliationPlan(record domain.CallRecord, prepared domain.PreparedCalls) error {
	if prepared.Call.ID != record.ID || prepared.Call.RunID != record.RunID || prepared.Call.StageName != record.StageName || prepared.Call.AttemptID != record.AttemptID || prepared.Call.RequestDigest != record.RequestDigest || prepared.Call.PolicyDigest != record.PolicyDigest || len(prepared.PhysicalCalls) > int(record.RetryPolicy.MaxAttempts) {
		return errors.New("reconciliation bundle differs from its bounded original call")
	}
	expected := domain.PhysicalLLMRequest
	if record.Kind == domain.CallSimilaritySearch {
		expected = domain.PhysicalSimilarityRequest
	}
	stopped := false
	for i, call := range prepared.PhysicalCalls {
		if call.CallRecordID != record.ID || call.RunID != record.RunID || call.StageName != record.StageName || call.AttemptID != record.AttemptID || call.Kind != expected || call.Ordinal != int64(i+1) {
			return errors.New("reconciliation physical plan changed scope, kind or order")
		}
		if stopped && call.State != domain.PhysicalPrepared && call.State != domain.PhysicalAbortedNoDispatch {
			return errors.New("reconciliation contains work after a stopping boundary")
		}
		switch call.State {
		case domain.PhysicalPrepared, domain.PhysicalDispatching, domain.PhysicalSent, domain.PhysicalUnknown:
			stopped = true
		case domain.PhysicalCompleted:
			if call.Outcome == nil || *call.Outcome != domain.PhysicalOutcomeRetryableFailure {
				stopped = true
			}
		}
	}
	return nil
}

var errReconciliationNewWork = errors.New("call reconciliation cannot authorize new work")

type reconciliationOnlyLedger struct{ CallReconciliationLedger }

func (reconciliationOnlyLedger) OpenCall(context.Context, domain.OpenCallRequest) (domain.CallRecord, error) {
	return domain.CallRecord{}, errReconciliationNewWork
}
func (reconciliationOnlyLedger) PrepareCalls(context.Context, domain.PrepareCallsRequest) (domain.PreparedCalls, error) {
	return domain.PreparedCalls{}, errReconciliationNewWork
}
func (reconciliationOnlyLedger) BeginDispatch(context.Context, domain.BeginDispatchRequest) (domain.DispatchGrant, error) {
	return domain.DispatchGrant{}, errReconciliationNewWork
}

type receiptOnlyAdapter[T any] struct{ CallReceiptReader[T] }

func (receiptOnlyAdapter[T]) Plan(context.Context, domain.CallRecord) (domain.CallPlanDecision, error) {
	return domain.CallPlanDecision{}, errReconciliationNewWork
}
func (a receiptOnlyAdapter[T]) Execute(ctx context.Context, grant domain.DispatchGrant) (domain.PhysicalExecution[T], error) {
	return a.Replay(ctx, grant)
}
