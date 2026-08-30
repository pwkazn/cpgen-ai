package domain_test

import (
	"testing"

	"cpgen/internal/domain"
)

const (
	callA domain.AttemptCallID = "call_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	callB domain.AttemptCallID = "call_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestCallTraceValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		trace   domain.CallTrace
		wantErr bool
	}{
		{
			name: "dispatched",
			trace: domain.CallTrace{LogicalOperationID: "op-1", DispatchKind: domain.DispatchDispatched,
				PhysicalAttemptCallIDs: []domain.AttemptCallID{callA, callB}, ResultAttemptCallID: pointer(callB)},
		},
		{
			name: "cache hit",
			trace: domain.CallTrace{LogicalOperationID: "op-2", DispatchKind: domain.DispatchCacheHit,
				CacheSourceAttemptCallID: pointer(callA), CachePinCallID: pointer(callB)},
		},
		{
			name:  "no dispatch",
			trace: domain.CallTrace{LogicalOperationID: "op-3", DispatchKind: domain.DispatchNone},
		},
		{
			name: "result outside physical calls",
			trace: domain.CallTrace{LogicalOperationID: "op-4", DispatchKind: domain.DispatchDispatched,
				PhysicalAttemptCallIDs: []domain.AttemptCallID{callA}, ResultAttemptCallID: pointer(callB)},
			wantErr: true,
		},
		{
			name: "cache hit pretending to dispatch",
			trace: domain.CallTrace{LogicalOperationID: "op-5", DispatchKind: domain.DispatchCacheHit,
				PhysicalAttemptCallIDs: []domain.AttemptCallID{callA}, CacheSourceAttemptCallID: pointer(callA), CachePinCallID: pointer(callB)},
			wantErr: true,
		},
		{
			name: "duplicate physical call",
			trace: domain.CallTrace{LogicalOperationID: "op-6", DispatchKind: domain.DispatchDispatched,
				PhysicalAttemptCallIDs: []domain.AttemptCallID{callA, callA}, ResultAttemptCallID: pointer(callA)},
			wantErr: true,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := test.trace.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestMeteredOutcomeRequiresExactlyOneTerminalValue(t *testing.T) {
	t.Parallel()
	trace := domain.CallTrace{LogicalOperationID: "op", DispatchKind: domain.DispatchNone}
	value := 1
	failure := domain.PortFailure{Code: domain.FailureBudgetExhausted, Class: domain.FailureRejected}
	for _, outcome := range []domain.MeteredOutcome[int]{
		{CallTrace: trace},
		{Value: &value, Failure: &failure, CallTrace: trace},
	} {
		if err := outcome.Validate(); err == nil {
			t.Fatal("invalid value/failure cardinality was accepted")
		}
	}
	if err := (domain.MeteredOutcome[int]{Failure: &failure, CallTrace: trace}).Validate(); err != nil {
		t.Fatalf("valid failure outcome rejected: %v", err)
	}
}

func pointer[T any](value T) *T { return &value }
