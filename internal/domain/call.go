package domain

import (
	"fmt"
	"slices"
	"time"
)

type DispatchKind string

const (
	DispatchDispatched DispatchKind = "DISPATCHED"
	DispatchCacheHit   DispatchKind = "CACHE_HIT"
	DispatchNone       DispatchKind = "NO_DISPATCH"
)

func (v DispatchKind) Valid() bool {
	return v == DispatchDispatched || v == DispatchCacheHit || v == DispatchNone
}
func (v *DispatchKind) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "DispatchKind", func(raw string) bool { return DispatchKind(raw).Valid() }, (*string)(v))
}

type CallTrace struct {
	LogicalOperationID      string          `json:"logical_operation_id"`
	DispatchKind            DispatchKind    `json:"dispatch_kind"`
	ResultAttemptCallID     *AttemptCallID  `json:"result_attempt_call_id,omitempty"`
	PhysicalAttemptCallIDs  []AttemptCallID `json:"physical_attempt_call_ids"`
	CacheSourceCallRecordID *CallRecordID   `json:"cache_source_call_record_id,omitempty"`
	CacheHitCallRecordID    *CallRecordID   `json:"cache_hit_call_record_id,omitempty"`
	DecisionSourceCallID    *AttemptCallID  `json:"decision_source_call_id,omitempty"`
}

func (t CallTrace) Equal(other CallTrace) bool {
	return t.LogicalOperationID == other.LogicalOperationID &&
		t.DispatchKind == other.DispatchKind &&
		optionalCallIDEqual(t.ResultAttemptCallID, other.ResultAttemptCallID) &&
		slices.Equal(t.PhysicalAttemptCallIDs, other.PhysicalAttemptCallIDs) &&
		optionalCallRecordIDEqual(t.CacheSourceCallRecordID, other.CacheSourceCallRecordID) &&
		optionalCallRecordIDEqual(t.CacheHitCallRecordID, other.CacheHitCallRecordID) &&
		optionalCallIDEqual(t.DecisionSourceCallID, other.DecisionSourceCallID)
}

func optionalCallIDEqual(left, right *AttemptCallID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func optionalCallRecordIDEqual(left, right *CallRecordID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func (t CallTrace) Validate() error {
	if t.LogicalOperationID == "" {
		return fmt.Errorf("logical operation id is required")
	}
	if !t.DispatchKind.Valid() {
		return fmt.Errorf("invalid dispatch kind %q", t.DispatchKind)
	}
	seen := make(map[AttemptCallID]struct{}, len(t.PhysicalAttemptCallIDs))
	for _, id := range t.PhysicalAttemptCallIDs {
		if err := id.Validate(); err != nil {
			return err
		}
		if _, exists := seen[id]; exists {
			return fmt.Errorf("duplicate physical attempt call id %q", id)
		}
		seen[id] = struct{}{}
	}
	validateOptionalID := func(name string, id *AttemptCallID) error {
		if id != nil {
			if err := id.Validate(); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
		return nil
	}
	for name, id := range map[string]*AttemptCallID{
		"result call": t.ResultAttemptCallID, "decision source call": t.DecisionSourceCallID,
	} {
		if err := validateOptionalID(name, id); err != nil {
			return err
		}
	}
	for name, id := range map[string]*CallRecordID{
		"cache source call record": t.CacheSourceCallRecordID, "cache hit call record": t.CacheHitCallRecordID,
	} {
		if id != nil {
			if err := id.Validate(); err != nil {
				return fmt.Errorf("%s: %w", name, err)
			}
		}
	}

	switch t.DispatchKind {
	case DispatchDispatched:
		if len(t.PhysicalAttemptCallIDs) == 0 || t.ResultAttemptCallID == nil {
			return fmt.Errorf("DISPATCHED trace requires physical and result calls")
		}
		if _, exists := seen[*t.ResultAttemptCallID]; !exists {
			return fmt.Errorf("result call is not a physical call")
		}
		if t.CacheSourceCallRecordID != nil || t.CacheHitCallRecordID != nil || t.DecisionSourceCallID != nil {
			return fmt.Errorf("DISPATCHED trace cannot contain cache or decision source calls")
		}
	case DispatchCacheHit:
		if len(t.PhysicalAttemptCallIDs) != 0 || t.ResultAttemptCallID != nil || t.DecisionSourceCallID != nil {
			return fmt.Errorf("CACHE_HIT trace cannot contain physical, result, or decision source calls")
		}
		if t.CacheSourceCallRecordID == nil || t.CacheHitCallRecordID == nil {
			return fmt.Errorf("CACHE_HIT trace requires source and current logical call records")
		}
		if *t.CacheSourceCallRecordID == *t.CacheHitCallRecordID {
			return fmt.Errorf("CACHE_HIT source and current logical call records must differ")
		}
	case DispatchNone:
		if len(t.PhysicalAttemptCallIDs) != 0 || t.ResultAttemptCallID != nil || t.CacheSourceCallRecordID != nil || t.CacheHitCallRecordID != nil {
			return fmt.Errorf("NO_DISPATCH trace cannot contain physical, result, or cache calls")
		}
	}
	return nil
}

type FailureClass string

const (
	FailureRetryable    FailureClass = "RETRYABLE"
	FailureBlocked      FailureClass = "BLOCKED"
	FailureIncompatible FailureClass = "INCOMPATIBLE"
	FailureRejected     FailureClass = "REJECTED"
	FailureUnknown      FailureClass = "UNKNOWN"
)

func (v FailureClass) Valid() bool {
	switch v {
	case FailureRetryable, FailureBlocked, FailureIncompatible, FailureRejected, FailureUnknown:
		return true
	default:
		return false
	}
}
func (v *FailureClass) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "FailureClass", func(raw string) bool { return FailureClass(raw).Valid() }, (*string)(v))
}

type PortFailureCode string

const (
	FailureTransport         PortFailureCode = "transport"
	FailureRateLimited       PortFailureCode = "rate_limited"
	FailureUnavailable       PortFailureCode = "unavailable"
	FailureProtocol          PortFailureCode = "protocol"
	FailureCapabilityMissing PortFailureCode = "capability_missing"
	FailureVersionMismatch   PortFailureCode = "version_mismatch"
	FailureBudgetExhausted   PortFailureCode = "budget_exhausted"
	FailurePolicyRejected    PortFailureCode = "policy_rejected"
	FailureCircuitOpen       PortFailureCode = "circuit_open"
	FailureBoundaryUnknown   PortFailureCode = "boundary_unknown"
)

func (v PortFailureCode) Valid() bool {
	switch v {
	case FailureTransport, FailureRateLimited, FailureUnavailable, FailureProtocol, FailureCapabilityMissing,
		FailureVersionMismatch, FailureBudgetExhausted, FailurePolicyRejected, FailureCircuitOpen, FailureBoundaryUnknown:
		return true
	default:
		return false
	}
}
func (v *PortFailureCode) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "PortFailureCode", func(raw string) bool { return PortFailureCode(raw).Valid() }, (*string)(v))
}

type PortFailure struct {
	Code       PortFailureCode   `json:"code"`
	Class      FailureClass      `json:"class"`
	Evidence   []PendingArtifact `json:"evidence"`
	RetryAfter *time.Time        `json:"retry_after,omitempty"`
}

func (f PortFailure) Validate() error {
	if !f.Code.Valid() {
		return fmt.Errorf("invalid port failure code %q", f.Code)
	}
	if !f.Class.Valid() {
		return fmt.Errorf("invalid port failure class %q", f.Class)
	}
	for index, evidence := range f.Evidence {
		if err := evidence.Validate(); err != nil {
			return fmt.Errorf("evidence %d: %w", index, err)
		}
	}
	return nil
}

type MeteredOutcome[T any] struct {
	Value     *T           `json:"value,omitempty"`
	Failure   *PortFailure `json:"failure,omitempty"`
	CallTrace CallTrace    `json:"call_trace"`
}

func (o MeteredOutcome[T]) Validate() error {
	if (o.Value == nil) == (o.Failure == nil) {
		return fmt.Errorf("metered outcome requires exactly one of value or failure")
	}
	if o.Failure != nil {
		if err := o.Failure.Validate(); err != nil {
			return err
		}
	}
	return o.CallTrace.Validate()
}
