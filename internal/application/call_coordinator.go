package application

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// CallAdapter separates deterministic call planning from the external dispatch
// boundary. Port failures are values; errors are reserved for failures that do
// not produce a usable typed outcome.
type CallAdapter[T any] interface {
	Plan(context.Context, domain.CallRecord) (domain.CallPlanDecision, error)
	Execute(context.Context, domain.DispatchGrant) (domain.PhysicalExecution[T], error)
}

// CallCoordinator executes one bounded logical call against a durable ledger.
// No ledger method spans either adapter call or a retry wait.
type CallCoordinator[T any] struct {
	ledger  port.CallLedger
	adapter CallAdapter[T]
	clock   clock.Clock
}

func NewCallCoordinator[T any](ledger port.CallLedger, adapter CallAdapter[T], source clock.Clock) (*CallCoordinator[T], error) {
	if ledger == nil || adapter == nil || source == nil {
		return nil, errors.New("call coordinator dependencies are required")
	}
	return &CallCoordinator[T]{ledger: ledger, adapter: adapter, clock: source}, nil
}

func (c *CallCoordinator[T]) Execute(ctx context.Context, request domain.OpenCallRequest) (domain.MeteredOutcome[T], error) {
	record, err := c.ledger.OpenCall(ctx, request)
	if err != nil {
		return domain.MeteredOutcome[T]{}, err
	}
	var prepared domain.PreparedCalls
	if record.State == domain.CallRecordOpen {
		decision, err := c.adapter.Plan(ctx, record)
		if err != nil {
			return domain.MeteredOutcome[T]{}, fmt.Errorf("plan logical call: %w", err)
		}
		if err := decision.Validate(record.RetryPolicy); err != nil {
			return domain.MeteredOutcome[T]{}, fmt.Errorf("validate fixed call plan: %w", err)
		}
		if decision.Failure != nil {
			return c.finish(ctx, record, request.ExpectedRunVersion, domain.DispatchNone, nil, decision.Failure)
		}
		prepared, err = c.ledger.PrepareCalls(ctx, domain.PrepareCallsRequest{
			RunID: request.RunID, ExpectedRunVersion: request.ExpectedRunVersion,
			StageName: request.StageName, AttemptID: request.AttemptID, CallRecordID: record.ID,
			PlanDigest: decision.Plan.Digest, Calls: decision.Plan.Calls,
			IdempotencyKey: coordinatorMutationID("prepare", record.ID, decision.Plan.Digest), At: c.clock.Now(),
		})
	} else {
		prepared, err = c.ledger.LoadCall(ctx, record.ID)
	}
	if err != nil {
		return domain.MeteredOutcome[T]{}, err
	}
	record = prepared.Call
	if record.State == domain.CallRecordTerminal {
		return c.replayTerminal(ctx, request.ExpectedRunVersion, prepared)
	}
	if int64(len(prepared.PhysicalCalls)) > record.RetryPolicy.MaxAttempts {
		return domain.MeteredOutcome[T]{}, errors.New("persisted physical calls exceed retry bound")
	}

	var lastDispatched *domain.AttemptCallID
	for index := range prepared.PhysicalCalls {
		physical := prepared.PhysicalCalls[index]
		if physical.State == domain.PhysicalUnknown {
			lastDispatched = callIDPointer(physical.ID)
			return c.finish(ctx, record, request.ExpectedRunVersion, domain.DispatchDispatched, lastDispatched, physical.Failure)
		}
		if physical.State == domain.PhysicalCompleted || physical.State == domain.PhysicalAbortedNoDispatch {
			if physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomeSuccess {
				value, err := c.replaySuccessfulPhysical(ctx, request.ExpectedRunVersion, physical)
				if err != nil {
					return domain.MeteredOutcome[T]{}, err
				}
				return c.finishValue(ctx, record, request.ExpectedRunVersion, physical.ID, value)
			}
			if physical.SentAt != nil {
				lastDispatched = callIDPointer(physical.ID)
			}
			if physical.Failure == nil {
				return domain.MeteredOutcome[T]{}, errors.New("persisted terminal physical call lacks an outcome")
			}
			if physical.Failure.Class != domain.FailureRetryable || index == len(prepared.PhysicalCalls)-1 {
				dispatch := domain.DispatchNone
				if lastDispatched != nil {
					dispatch = domain.DispatchDispatched
				}
				return c.finish(ctx, record, request.ExpectedRunVersion, dispatch, lastDispatched, physical.Failure)
			}
			if err := c.waitForRetry(ctx, record, physical); err != nil {
				return domain.MeteredOutcome[T]{}, err
			}
			continue
		}

		grant, err := c.dispatchGrant(ctx, record, request.ExpectedRunVersion, physical)
		if err != nil {
			return domain.MeteredOutcome[T]{}, err
		}
		execution, executeErr := c.adapter.Execute(ctx, grant)
		if executeErr != nil {
			failure := &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
			if err := c.complete(ctx, record, grant, domain.PhysicalExecution[T]{
				Boundary: domain.BoundaryUnknown, Failure: failure,
			}); err != nil {
				return domain.MeteredOutcome[T]{}, errors.Join(fmt.Errorf("execute physical call: %w", executeErr), err)
			}
			lastDispatched = callIDPointer(physical.ID)
			outcome, finishErr := c.finish(ctx, record, request.ExpectedRunVersion, domain.DispatchDispatched, lastDispatched, failure)
			if finishErr != nil {
				return domain.MeteredOutcome[T]{}, errors.Join(fmt.Errorf("execute physical call: %w", executeErr), finishErr)
			}
			return outcome, fmt.Errorf("execute physical call after unknown boundary: %w", executeErr)
		}
		if err := execution.Validate(); err != nil {
			return domain.MeteredOutcome[T]{}, fmt.Errorf("validate physical execution: %w", err)
		}
		if execution.Boundary == domain.BoundaryCompleted {
			if physical.State != domain.PhysicalSent {
				if err := c.ledger.MarkSent(ctx, grant, c.clock.Now()); err != nil {
					return domain.MeteredOutcome[T]{}, err
				}
			}
			lastDispatched = callIDPointer(physical.ID)
		} else if execution.Boundary == domain.BoundaryUnknown {
			lastDispatched = callIDPointer(physical.ID)
		} else if physical.State == domain.PhysicalSent {
			return domain.MeteredOutcome[T]{}, errors.New("provider reported no-send after a durable sent boundary")
		}
		if err := c.complete(ctx, record, grant, execution); err != nil {
			return domain.MeteredOutcome[T]{}, err
		}

		if execution.Value != nil {
			return c.finishValue(ctx, record, request.ExpectedRunVersion, physical.ID, execution.Value)
		}
		if execution.Failure.Class != domain.FailureRetryable || execution.Boundary == domain.BoundaryUnknown || index == len(prepared.PhysicalCalls)-1 {
			dispatch := domain.DispatchNone
			if lastDispatched != nil {
				dispatch = domain.DispatchDispatched
			}
			return c.finish(ctx, record, request.ExpectedRunVersion, dispatch, lastDispatched, execution.Failure)
		}
		if err := c.waitForRetry(ctx, record, physical); err != nil {
			return domain.MeteredOutcome[T]{}, err
		}
	}
	return domain.MeteredOutcome[T]{}, errors.New("fixed call plan completed without an outcome")
}

func (c *CallCoordinator[T]) dispatchGrant(ctx context.Context, record domain.CallRecord, expectedVersion int64, physical domain.PhysicalCall) (domain.DispatchGrant, error) {
	if physical.State != domain.PhysicalPrepared {
		return c.ledger.ResumeDispatch(ctx, expectedVersion, physical.ID)
	}
	return c.ledger.BeginDispatch(ctx, domain.BeginDispatchRequest{
		RunID: record.RunID, ExpectedRunVersion: expectedVersion,
		StageName: record.StageName, AttemptID: record.AttemptID, CallRecordID: record.ID,
		AttemptCallID:  physical.ID,
		IdempotencyKey: coordinatorMutationID("begin", record.ID, physical.ID), At: c.clock.Now(),
	})
}

func (c *CallCoordinator[T]) waitForRetry(ctx context.Context, record domain.CallRecord, physical domain.PhysicalCall) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.clock.After(record.RetryPolicy.Backoff(physical.RetryOrdinal)):
		return nil
	}
}

func (c *CallCoordinator[T]) replaySuccessfulPhysical(ctx context.Context, expectedVersion int64, physical domain.PhysicalCall) (*T, error) {
	grant, err := c.ledger.ResumeDispatch(ctx, expectedVersion, physical.ID)
	if err != nil {
		return nil, err
	}
	execution, err := c.adapter.Execute(ctx, grant)
	if err != nil {
		return nil, fmt.Errorf("reconcile completed physical call: %w", err)
	}
	if err := execution.Validate(); err != nil {
		return nil, fmt.Errorf("validate reconciled physical execution: %w", err)
	}
	if execution.Boundary != domain.BoundaryCompleted || execution.Value == nil || execution.ProviderRequestID != physical.ProviderRequestID || !digestsMatch(execution.ResponseDigest, physical.ResponseDigest) {
		return nil, errors.New("reconciled physical execution differs from durable completion")
	}
	return execution.Value, nil
}

func (c *CallCoordinator[T]) replayTerminal(ctx context.Context, expectedVersion int64, prepared domain.PreparedCalls) (domain.MeteredOutcome[T], error) {
	if prepared.CallTrace == nil {
		return domain.MeteredOutcome[T]{}, errors.New("terminal logical call lacks durable trace")
	}
	if prepared.Failure != nil {
		return terminalOutcome[T](nil, prepared.Failure, *prepared.CallTrace)
	}
	if prepared.Call.ResultAttemptCallID == nil {
		return domain.MeteredOutcome[T]{}, errors.New("terminal successful call lacks physical result")
	}
	for _, physical := range prepared.PhysicalCalls {
		if physical.ID == *prepared.Call.ResultAttemptCallID {
			value, err := c.replaySuccessfulPhysical(ctx, expectedVersion, physical)
			if err != nil {
				return domain.MeteredOutcome[T]{}, err
			}
			return terminalOutcome(value, (*domain.PortFailure)(nil), *prepared.CallTrace)
		}
	}
	return domain.MeteredOutcome[T]{}, errors.New("terminal call result does not reference a loaded physical call")
}

func digestsMatch(left, right *domain.Digest) bool {
	return left != nil && right != nil && *left == *right
}

func (c *CallCoordinator[T]) complete(ctx context.Context, record domain.CallRecord, grant domain.DispatchGrant, execution domain.PhysicalExecution[T]) error {
	request := domain.CompletePhysicalRequest{
		RunID: record.RunID, ExpectedRunVersion: grant.ExpectedRunVersion,
		StageName: record.StageName, AttemptID: record.AttemptID, CallRecordID: record.ID,
		AttemptCallID: grant.AttemptCallID, Failure: execution.Failure,
		ProviderRequestID: execution.ProviderRequestID, ResponseDigest: execution.ResponseDigest, Usage: execution.Usage,
		IdempotencyKey: coordinatorMutationID("complete", record.ID, grant.AttemptCallID), At: c.clock.Now(),
	}
	switch execution.Boundary {
	case domain.BoundaryCompleted:
		request.State = domain.PhysicalCompleted
		if execution.Value != nil {
			request.Outcome = domain.PhysicalOutcomeSuccess
		} else if execution.Failure.Class == domain.FailureRetryable {
			request.Outcome = domain.PhysicalOutcomeRetryableFailure
		} else {
			request.Outcome = domain.PhysicalOutcomePermanentFailure
		}
	case domain.BoundaryConfirmedNoSend:
		request.State, request.Outcome = domain.PhysicalAbortedNoDispatch, domain.PhysicalOutcomeNoSend
	case domain.BoundaryUnknown:
		request.State, request.Outcome = domain.PhysicalUnknown, domain.PhysicalOutcomeUnknown
	}
	return c.ledger.CompletePhysical(ctx, request)
}

func (c *CallCoordinator[T]) finishValue(ctx context.Context, record domain.CallRecord, expectedVersion int64, resultID domain.AttemptCallID, value *T) (domain.MeteredOutcome[T], error) {
	trace, err := c.ledger.FinishCall(ctx, finishRequest(record, expectedVersion, domain.DispatchDispatched, &resultID, nil, c.clock.Now()))
	if err != nil {
		return domain.MeteredOutcome[T]{}, err
	}
	return terminalOutcome(value, (*domain.PortFailure)(nil), trace)
}

func (c *CallCoordinator[T]) finish(ctx context.Context, record domain.CallRecord, expectedVersion int64, dispatch domain.DispatchKind, resultID *domain.AttemptCallID, failure *domain.PortFailure) (domain.MeteredOutcome[T], error) {
	trace, err := c.ledger.FinishCall(ctx, finishRequest(record, expectedVersion, dispatch, resultID, failure, c.clock.Now()))
	if err != nil {
		return domain.MeteredOutcome[T]{}, err
	}
	return terminalOutcome[T](nil, failure, trace)
}

func finishRequest(record domain.CallRecord, expectedVersion int64, dispatch domain.DispatchKind, resultID *domain.AttemptCallID, failure *domain.PortFailure, at time.Time) domain.FinishCallRequest {
	return domain.FinishCallRequest{
		RunID: record.RunID, ExpectedRunVersion: expectedVersion, StageName: record.StageName,
		AttemptID: record.AttemptID, CallRecordID: record.ID, DispatchKind: dispatch,
		ResultAttemptCallID: resultID, Failure: failure,
		IdempotencyKey: coordinatorMutationID("finish", record.ID, dispatch), At: at,
	}
}

func terminalOutcome[T any](value *T, failure *domain.PortFailure, trace domain.CallTrace) (domain.MeteredOutcome[T], error) {
	result := domain.MeteredOutcome[T]{Value: value, Failure: failure, CallTrace: trace}
	if err := result.Validate(); err != nil {
		return domain.MeteredOutcome[T]{}, err
	}
	return result, nil
}

func callIDPointer(id domain.AttemptCallID) *domain.AttemptCallID {
	result := id
	return &result
}

func coordinatorMutationID(prefix string, values ...any) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = fmt.Fprintln(hash, value)
	}
	return fmt.Sprintf("%s_%x", prefix, hash.Sum(nil)[:16])
}
