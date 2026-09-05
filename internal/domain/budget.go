package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

type BudgetDimension string

const (
	BudgetLLMCalls                 BudgetDimension = "LLM_CALLS"
	BudgetLLMInputTokens           BudgetDimension = "LLM_INPUT_TOKENS"
	BudgetLLMOutputTokens          BudgetDimension = "LLM_OUTPUT_TOKENS"
	BudgetExternalCostMicroUSD     BudgetDimension = "EXTERNAL_COST_MICRO_USD"
	BudgetSimilarityCalls          BudgetDimension = "SIMILARITY_CALLS"
	BudgetSimilarityCostMicroUSD   BudgetDimension = "SIMILARITY_COST_MICRO_USD"
	BudgetDockerContainerCreates   BudgetDimension = "DOCKER_CONTAINER_CREATES"
	BudgetArtifactPhysicalNewBytes BudgetDimension = "ARTIFACT_PHYSICAL_NEW_BYTES"
	BudgetActiveTimeNS             BudgetDimension = "ACTIVE_TIME_NS"
)

func (v BudgetDimension) Valid() bool {
	switch v {
	case BudgetLLMCalls, BudgetLLMInputTokens, BudgetLLMOutputTokens, BudgetExternalCostMicroUSD,
		BudgetSimilarityCalls, BudgetSimilarityCostMicroUSD, BudgetDockerContainerCreates,
		BudgetArtifactPhysicalNewBytes, BudgetActiveTimeNS:
		return true
	default:
		return false
	}
}

func (v *BudgetDimension) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "BudgetDimension", func(raw string) bool { return BudgetDimension(raw).Valid() }, (*string)(v))
}

type BudgetAccount struct {
	RunID                 RunID           `json:"run_id"`
	RequestSnapshotDigest Digest          `json:"request_snapshot_digest"`
	Dimension             BudgetDimension `json:"dimension"`
	Limit                 int64           `json:"limit"`
	Reserved              int64           `json:"reserved"`
	Consumed              int64           `json:"consumed"`
	Version               int64           `json:"version"`
}

func (v BudgetAccount) Validate() error {
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if err := v.RequestSnapshotDigest.Validate(); err != nil {
		return err
	}
	if !v.Dimension.Valid() {
		return fmt.Errorf("invalid budget dimension %q", v.Dimension)
	}
	if v.Limit < 0 || v.Reserved < 0 || v.Consumed < 0 || v.Version <= 0 {
		return errors.New("budget account values must be non-negative with a positive version")
	}
	if v.Consumed > v.Limit || v.Reserved > v.Limit-v.Consumed {
		return errors.New("budget account exceeds its immutable limit")
	}
	return nil
}

func (v BudgetAccount) Remaining() int64 {
	if v.Limit < 0 || v.Reserved < 0 || v.Consumed < 0 || v.Consumed > v.Limit || v.Reserved > v.Limit-v.Consumed {
		return 0
	}
	return v.Limit - v.Consumed - v.Reserved
}

type RetryPolicy struct {
	MaxAttempts      int64         `json:"max_attempts"`
	InitialBackoff   time.Duration `json:"initial_backoff"`
	MaxBackoff       time.Duration `json:"max_backoff"`
	JitterSeedDigest Digest        `json:"jitter_seed_digest"`
}

func (v RetryPolicy) Validate() error {
	if v.MaxAttempts <= 0 || v.InitialBackoff <= 0 || v.MaxBackoff < v.InitialBackoff {
		return errors.New("retry policy must have positive bounded attempts and backoff")
	}
	return v.JitterSeedDigest.Validate()
}

func (v RetryPolicy) Backoff(completedAttempt int64) time.Duration {
	if err := v.Validate(); err != nil || completedAttempt <= 0 {
		return 0
	}
	base := v.InitialBackoff
	for ordinal := int64(1); ordinal < completedAttempt && base < v.MaxBackoff; ordinal++ {
		if base > v.MaxBackoff/2 {
			base = v.MaxBackoff
			break
		}
		base *= 2
	}
	if base >= v.MaxBackoff {
		return v.MaxBackoff
	}
	jitterRange := base / 4
	if jitterRange <= 0 {
		return base
	}
	seed := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", v.JitterSeedDigest, completedAttempt)))
	jitter := time.Duration(binary.BigEndian.Uint64(seed[:8]) % (uint64(jitterRange) + 1))
	if jitter > v.MaxBackoff-base {
		return v.MaxBackoff
	}
	return base + jitter
}

type CallKind string

const (
	CallLLMGenerate      CallKind = "LLM_GENERATE"
	CallSimilaritySearch CallKind = "SIMILARITY_SEARCH"
	CallSandboxCompile   CallKind = "SANDBOX_COMPILE"
	CallSandboxRun       CallKind = "SANDBOX_RUN"
	CallSandboxProbe     CallKind = "SANDBOX_PROBE"
	CallCacheReuse       CallKind = "CACHE_REUSE"
)

func (v CallKind) Valid() bool {
	switch v {
	case CallLLMGenerate, CallSimilaritySearch, CallSandboxCompile, CallSandboxRun, CallSandboxProbe, CallCacheReuse:
		return true
	default:
		return false
	}
}

type CallRecordState string

const (
	CallRecordOpen     CallRecordState = "OPEN"
	CallRecordPrepared CallRecordState = "PREPARED"
	CallRecordTerminal CallRecordState = "TERMINAL"
)

func (v CallRecordState) Valid() bool {
	return v == CallRecordOpen || v == CallRecordPrepared || v == CallRecordTerminal
}

type CallRecord struct {
	ID                      CallRecordID    `json:"id"`
	RunID                   RunID           `json:"run_id"`
	StageName               StageName       `json:"stage_name"`
	AttemptID               AttemptID       `json:"attempt_id"`
	LogicalOperationID      string          `json:"logical_operation_id"`
	Kind                    CallKind        `json:"kind"`
	Provider                string          `json:"provider"`
	RequestDigest           Digest          `json:"request_digest"`
	PolicyDigest            Digest          `json:"policy_digest"`
	RetryPolicy             RetryPolicy     `json:"retry_policy"`
	IdempotencyKey          string          `json:"idempotency_key"`
	State                   CallRecordState `json:"state"`
	DispatchKind            *DispatchKind   `json:"dispatch_kind,omitempty"`
	ResultAttemptCallID     *AttemptCallID  `json:"result_attempt_call_id,omitempty"`
	CacheSourceCallRecordID *CallRecordID   `json:"cache_source_call_record_id,omitempty"`
	CacheHitCallRecordID    *CallRecordID   `json:"cache_hit_call_record_id,omitempty"`
	Failure                 *PortFailure    `json:"failure,omitempty"`
	OpenedAt                time.Time       `json:"opened_at"`
	PreparedAt              *time.Time      `json:"prepared_at,omitempty"`
	CompletedAt             *time.Time      `json:"completed_at,omitempty"`
}

func (v CallRecord) Validate() error {
	for _, check := range []error{v.ID.Validate(), v.RunID.Validate(), v.StageName.Validate(), v.AttemptID.Validate(), v.RequestDigest.Validate(), v.PolicyDigest.Validate(), v.RetryPolicy.Validate(), validateIdempotencyKey(v.IdempotencyKey), validateLogicalOperationID(v.LogicalOperationID)} {
		if check != nil {
			return check
		}
	}
	if !v.Kind.Valid() || !v.State.Valid() || strings.TrimSpace(v.Provider) == "" || len(v.Provider) > 256 || !utf8.ValidString(v.Provider) {
		return errors.New("call record kind, provider, or state is invalid")
	}
	if err := validateUTCTime("opened at", v.OpenedAt); err != nil {
		return err
	}
	if v.PreparedAt != nil {
		if err := validateUTCTime("prepared at", *v.PreparedAt); err != nil || v.PreparedAt.Before(v.OpenedAt) {
			return errors.New("call record prepared time is invalid")
		}
	}
	if v.CompletedAt != nil {
		if err := validateUTCTime("completed at", *v.CompletedAt); err != nil || v.CompletedAt.Before(v.OpenedAt) {
			return errors.New("call record completion time is invalid")
		}
	}
	switch v.State {
	case CallRecordOpen:
		if v.PreparedAt != nil || v.DispatchKind != nil || v.ResultAttemptCallID != nil || v.CacheSourceCallRecordID != nil || v.CacheHitCallRecordID != nil || v.Failure != nil || v.CompletedAt != nil {
			return errors.New("open call record has prepared or terminal fields")
		}
	case CallRecordPrepared:
		if v.PreparedAt == nil || v.DispatchKind != nil || v.ResultAttemptCallID != nil || v.CacheSourceCallRecordID != nil || v.CacheHitCallRecordID != nil || v.Failure != nil || v.CompletedAt != nil {
			return errors.New("prepared call record has invalid fields")
		}
	case CallRecordTerminal:
		if v.DispatchKind == nil || v.CompletedAt == nil {
			return errors.New("terminal call record lacks terminal fields")
		}
		if v.Failure != nil {
			if err := v.Failure.Validate(); err != nil {
				return err
			}
		}
		switch *v.DispatchKind {
		case DispatchDispatched:
			if v.ResultAttemptCallID == nil || v.CacheSourceCallRecordID != nil || v.CacheHitCallRecordID != nil {
				return errors.New("terminal DISPATCHED call has invalid projection")
			}
			if err := v.ResultAttemptCallID.Validate(); err != nil {
				return err
			}
		case DispatchCacheHit:
			if v.ResultAttemptCallID != nil || v.CacheSourceCallRecordID == nil || v.CacheHitCallRecordID == nil || *v.CacheHitCallRecordID != v.ID || *v.CacheSourceCallRecordID == *v.CacheHitCallRecordID || v.Failure != nil {
				return errors.New("terminal CACHE_HIT call has invalid logical provenance")
			}
			if err := v.CacheSourceCallRecordID.Validate(); err != nil {
				return err
			}
		case DispatchNone:
			if v.ResultAttemptCallID != nil || v.CacheSourceCallRecordID != nil || v.CacheHitCallRecordID != nil || v.Failure == nil {
				return errors.New("terminal NO_DISPATCH call has invalid projection")
			}
		default:
			return errors.New("terminal call has invalid dispatch kind")
		}
	}
	return nil
}

type OpenCallRequest struct {
	ID                 CallRecordID `json:"id"`
	RunID              RunID        `json:"run_id"`
	ExpectedRunVersion int64        `json:"expected_run_version"`
	StageName          StageName    `json:"stage_name"`
	AttemptID          AttemptID    `json:"attempt_id"`
	LogicalOperationID string       `json:"logical_operation_id"`
	Kind               CallKind     `json:"kind"`
	Provider           string       `json:"provider"`
	RequestDigest      Digest       `json:"request_digest"`
	PolicyDigest       Digest       `json:"policy_digest"`
	RetryPolicy        RetryPolicy  `json:"retry_policy"`
	IdempotencyKey     string       `json:"idempotency_key"`
	At                 time.Time    `json:"at"`
}

func (v OpenCallRequest) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	for _, check := range []error{v.ID.Validate(), v.StageName.Validate(), v.AttemptID.Validate(), v.RequestDigest.Validate(), v.PolicyDigest.Validate(), v.RetryPolicy.Validate(), validateLogicalOperationID(v.LogicalOperationID)} {
		if check != nil {
			return check
		}
	}
	if !v.Kind.Valid() || strings.TrimSpace(v.Provider) == "" || len(v.Provider) > 256 || !utf8.ValidString(v.Provider) {
		return errors.New("call kind or provider is invalid")
	}
	return nil
}

type PhysicalCallKind string

const (
	PhysicalLLMRequest            PhysicalCallKind = "LLM_REQUEST"
	PhysicalSimilarityRequest     PhysicalCallKind = "SIMILARITY_REQUEST"
	PhysicalDockerEnginePing      PhysicalCallKind = "DOCKER_ENGINE_PING"
	PhysicalDockerContainerCreate PhysicalCallKind = "DOCKER_CONTAINER_CREATE"
	PhysicalLocalArtifactWrite    PhysicalCallKind = "LOCAL_ARTIFACT_WRITE"
)

func (v PhysicalCallKind) Valid() bool {
	switch v {
	case PhysicalLLMRequest, PhysicalSimilarityRequest, PhysicalDockerEnginePing, PhysicalDockerContainerCreate, PhysicalLocalArtifactWrite:
		return true
	default:
		return false
	}
}

type PhysicalCallState string

const (
	PhysicalPrepared          PhysicalCallState = "PREPARED"
	PhysicalDispatching       PhysicalCallState = "DISPATCHING"
	PhysicalSent              PhysicalCallState = "SENT"
	PhysicalCompleted         PhysicalCallState = "COMPLETED"
	PhysicalAbortedNoDispatch PhysicalCallState = "ABORTED_NO_DISPATCH"
	PhysicalUnknown           PhysicalCallState = "UNKNOWN"
)

func (v PhysicalCallState) Valid() bool {
	switch v {
	case PhysicalPrepared, PhysicalDispatching, PhysicalSent, PhysicalCompleted, PhysicalAbortedNoDispatch, PhysicalUnknown:
		return true
	default:
		return false
	}
}

type PhysicalOutcomeKind string

const (
	PhysicalOutcomeSuccess          PhysicalOutcomeKind = "SUCCESS"
	PhysicalOutcomeRetryableFailure PhysicalOutcomeKind = "RETRYABLE_FAILURE"
	PhysicalOutcomePermanentFailure PhysicalOutcomeKind = "PERMANENT_FAILURE"
	PhysicalOutcomeNoSend           PhysicalOutcomeKind = "NO_SEND"
	PhysicalOutcomeUnknown          PhysicalOutcomeKind = "UNKNOWN_BOUNDARY"
)

func (v PhysicalOutcomeKind) Valid() bool {
	switch v {
	case PhysicalOutcomeSuccess, PhysicalOutcomeRetryableFailure, PhysicalOutcomePermanentFailure, PhysicalOutcomeNoSend, PhysicalOutcomeUnknown:
		return true
	default:
		return false
	}
}

type PhysicalCall struct {
	ID                AttemptCallID        `json:"id"`
	CallRecordID      CallRecordID         `json:"call_record_id"`
	RunID             RunID                `json:"run_id"`
	StageName         StageName            `json:"stage_name"`
	AttemptID         AttemptID            `json:"attempt_id"`
	Ordinal           int64                `json:"ordinal"`
	RetryGroup        string               `json:"retry_group"`
	RetryOrdinal      int64                `json:"retry_ordinal"`
	Kind              PhysicalCallKind     `json:"kind"`
	Provider          string               `json:"provider"`
	RequestDigest     Digest               `json:"request_digest"`
	IdempotencyKey    string               `json:"idempotency_key"`
	State             PhysicalCallState    `json:"state"`
	Outcome           *PhysicalOutcomeKind `json:"outcome,omitempty"`
	Failure           *PortFailure         `json:"failure,omitempty"`
	ProviderRequestID string               `json:"provider_request_id,omitempty"`
	ResponseDigest    *Digest              `json:"response_digest,omitempty"`
	PreparedAt        time.Time            `json:"prepared_at"`
	DispatchStartedAt *time.Time           `json:"dispatch_started_at,omitempty"`
	SentAt            *time.Time           `json:"sent_at,omitempty"`
	CompletedAt       *time.Time           `json:"completed_at,omitempty"`
}

func (v PhysicalCall) Validate() error {
	for _, check := range []error{v.ID.Validate(), v.CallRecordID.Validate(), v.RunID.Validate(), v.StageName.Validate(), v.AttemptID.Validate(), v.RequestDigest.Validate(), validateIdempotencyKey(v.IdempotencyKey)} {
		if check != nil {
			return check
		}
	}
	if v.Ordinal <= 0 || v.RetryOrdinal <= 0 || strings.TrimSpace(v.RetryGroup) == "" || len(v.RetryGroup) > 128 || !v.Kind.Valid() || !v.State.Valid() || strings.TrimSpace(v.Provider) == "" {
		return errors.New("physical call identity, ordinal, kind, provider, or state is invalid")
	}
	if err := validateUTCTime("prepared at", v.PreparedAt); err != nil {
		return err
	}
	for name, value := range map[string]*time.Time{"dispatch started at": v.DispatchStartedAt, "sent at": v.SentAt, "completed at": v.CompletedAt} {
		if value != nil {
			if err := validateUTCTime(name, *value); err != nil || value.Before(v.PreparedAt) {
				return fmt.Errorf("%s is invalid", name)
			}
		}
	}
	if v.SentAt != nil && (v.DispatchStartedAt == nil || v.SentAt.Before(*v.DispatchStartedAt)) {
		return errors.New("physical send time precedes dispatch")
	}
	if v.CompletedAt != nil && v.SentAt != nil && v.CompletedAt.Before(*v.SentAt) {
		return errors.New("physical completion precedes send")
	}
	if v.ResponseDigest != nil {
		if err := v.ResponseDigest.Validate(); err != nil {
			return err
		}
	}
	if v.Failure != nil {
		if err := v.Failure.Validate(); err != nil {
			return err
		}
	}
	switch v.State {
	case PhysicalPrepared:
		if v.Outcome != nil || v.Failure != nil || v.ProviderRequestID != "" || v.ResponseDigest != nil || v.DispatchStartedAt != nil || v.SentAt != nil || v.CompletedAt != nil {
			return errors.New("prepared physical call has lifecycle fields")
		}
	case PhysicalDispatching:
		if v.DispatchStartedAt == nil || v.Outcome != nil || v.Failure != nil || v.ProviderRequestID != "" || v.ResponseDigest != nil || v.SentAt != nil || v.CompletedAt != nil {
			return errors.New("dispatching physical call has invalid fields")
		}
	case PhysicalSent:
		if v.DispatchStartedAt == nil || v.SentAt == nil || v.Outcome != nil || v.Failure != nil || v.CompletedAt != nil {
			return errors.New("sent physical call has invalid fields")
		}
	case PhysicalCompleted:
		if v.DispatchStartedAt == nil || v.SentAt == nil || v.CompletedAt == nil || v.Outcome == nil || v.ProviderRequestID == "" || v.ResponseDigest == nil {
			return errors.New("completed physical call lacks terminal fields")
		}
		if *v.Outcome == PhysicalOutcomeSuccess {
			if v.Failure != nil {
				return errors.New("successful physical call carries a failure")
			}
		} else if *v.Outcome == PhysicalOutcomeRetryableFailure {
			if v.Failure == nil || v.Failure.Class != FailureRetryable {
				return errors.New("retryable physical outcome lacks retryable failure")
			}
		} else if *v.Outcome != PhysicalOutcomePermanentFailure || v.Failure == nil || v.Failure.Class == FailureRetryable || v.Failure.Class == FailureUnknown {
			return errors.New("completed physical call has invalid outcome")
		}
	case PhysicalAbortedNoDispatch:
		if v.SentAt != nil || v.CompletedAt == nil || v.Outcome == nil || *v.Outcome != PhysicalOutcomeNoSend || v.Failure == nil || v.ProviderRequestID != "" || v.ResponseDigest != nil {
			return errors.New("aborted physical call has invalid no-dispatch fields")
		}
	case PhysicalUnknown:
		if v.DispatchStartedAt == nil || v.CompletedAt == nil || v.Outcome == nil || *v.Outcome != PhysicalOutcomeUnknown || v.Failure == nil || v.Failure.Code != FailureBoundaryUnknown || v.Failure.Class != FailureUnknown || v.ResponseDigest != nil {
			return errors.New("unknown physical call has invalid boundary fields")
		}
	}
	return nil
}

type ReservationState string

const (
	ReservationReserved ReservationState = "RESERVED"
	ReservationSettled  ReservationState = "SETTLED"
	ReservationReleased ReservationState = "RELEASED"
)

func (v ReservationState) Valid() bool {
	return v == ReservationReserved || v == ReservationSettled || v == ReservationReleased
}

type BudgetReservation struct {
	ID            ReservationID    `json:"id"`
	RunID         RunID            `json:"run_id"`
	StageName     StageName        `json:"stage_name"`
	AttemptID     AttemptID        `json:"attempt_id"`
	CallRecordID  CallRecordID     `json:"call_record_id"`
	AttemptCallID AttemptCallID    `json:"attempt_call_id"`
	Dimension     BudgetDimension  `json:"dimension"`
	Subkey        string           `json:"subkey"`
	UpperBound    int64            `json:"upper_bound"`
	SettledValue  *int64           `json:"settled_value,omitempty"`
	State         ReservationState `json:"state"`
	CreatedAt     time.Time        `json:"created_at"`
	SettledAt     *time.Time       `json:"settled_at,omitempty"`
}

func (v BudgetReservation) Validate() error {
	for _, check := range []error{v.ID.Validate(), v.RunID.Validate(), v.StageName.Validate(), v.AttemptID.Validate(), v.CallRecordID.Validate(), v.AttemptCallID.Validate()} {
		if check != nil {
			return check
		}
	}
	if !v.Dimension.Valid() || strings.TrimSpace(v.Subkey) == "" || len(v.Subkey) > 128 || v.UpperBound <= 0 || !v.State.Valid() {
		return errors.New("budget reservation identity, dimension, bound, or state is invalid")
	}
	if err := validateUTCTime("reservation created at", v.CreatedAt); err != nil {
		return err
	}
	if v.State == ReservationReserved {
		if v.SettledValue != nil || v.SettledAt != nil {
			return errors.New("reserved budget reservation has settlement fields")
		}
	} else {
		if v.SettledValue == nil || v.SettledAt == nil || *v.SettledValue < 0 || *v.SettledValue > v.UpperBound {
			return errors.New("terminal budget reservation has invalid settlement")
		}
		if v.State == ReservationReleased && *v.SettledValue != 0 {
			return errors.New("released reservation must settle zero")
		}
		if err := validateUTCTime("reservation settled at", *v.SettledAt); err != nil || v.SettledAt.Before(v.CreatedAt) {
			return errors.New("reservation settlement time is invalid")
		}
	}
	return nil
}

func (v BudgetReservation) ValidateFor(kind PhysicalCallKind) error {
	if err := v.Validate(); err != nil {
		return err
	}
	if !dimensionAllowed(kind, v.Dimension) {
		return fmt.Errorf("budget dimension %q is not allowed for physical kind %q", v.Dimension, kind)
	}
	return nil
}

type ReservationPlan struct {
	ID         ReservationID   `json:"id"`
	Dimension  BudgetDimension `json:"dimension"`
	Subkey     string          `json:"subkey"`
	UpperBound int64           `json:"upper_bound"`
}

func (v ReservationPlan) ValidateFor(kind PhysicalCallKind) error {
	if err := v.ID.Validate(); err != nil {
		return err
	}
	if !v.Dimension.Valid() || strings.TrimSpace(v.Subkey) == "" || len(v.Subkey) > 128 || v.UpperBound <= 0 {
		return errors.New("reservation plan dimension, subkey, or bound is invalid")
	}
	if !dimensionAllowed(kind, v.Dimension) {
		return fmt.Errorf("budget dimension %q is not allowed for physical kind %q", v.Dimension, kind)
	}
	return nil
}

func dimensionAllowed(kind PhysicalCallKind, dimension BudgetDimension) bool {
	switch kind {
	case PhysicalLLMRequest:
		return dimension == BudgetLLMCalls || dimension == BudgetLLMInputTokens || dimension == BudgetLLMOutputTokens || dimension == BudgetExternalCostMicroUSD
	case PhysicalSimilarityRequest:
		return dimension == BudgetSimilarityCalls || dimension == BudgetSimilarityCostMicroUSD
	case PhysicalDockerContainerCreate:
		return dimension == BudgetDockerContainerCreates
	case PhysicalLocalArtifactWrite:
		return dimension == BudgetArtifactPhysicalNewBytes
	case PhysicalDockerEnginePing:
		return false
	default:
		return false
	}
}

type PhysicalCallPlan struct {
	ID             AttemptCallID     `json:"id"`
	Ordinal        int64             `json:"ordinal"`
	RetryGroup     string            `json:"retry_group"`
	RetryOrdinal   int64             `json:"retry_ordinal"`
	Kind           PhysicalCallKind  `json:"kind"`
	Provider       string            `json:"provider"`
	RequestDigest  Digest            `json:"request_digest"`
	IdempotencyKey string            `json:"idempotency_key"`
	Reservations   []ReservationPlan `json:"reservations"`
}

func (v PhysicalCallPlan) Validate() error {
	for _, check := range []error{v.ID.Validate(), v.RequestDigest.Validate(), validateIdempotencyKey(v.IdempotencyKey)} {
		if check != nil {
			return check
		}
	}
	if v.Ordinal <= 0 || v.RetryOrdinal <= 0 || strings.TrimSpace(v.RetryGroup) == "" || len(v.RetryGroup) > 128 || !v.Kind.Valid() || strings.TrimSpace(v.Provider) == "" {
		return errors.New("physical plan identity, ordinal, retry group, kind, or provider is invalid")
	}
	seenIDs := make(map[ReservationID]struct{}, len(v.Reservations))
	seenDimensions := make(map[BudgetDimension]struct{}, len(v.Reservations))
	for _, reservation := range v.Reservations {
		if err := reservation.ValidateFor(v.Kind); err != nil {
			return err
		}
		if _, exists := seenIDs[reservation.ID]; exists {
			return errors.New("physical plan contains duplicate reservation ID")
		}
		seenIDs[reservation.ID] = struct{}{}
		if _, exists := seenDimensions[reservation.Dimension]; exists {
			return errors.New("physical plan contains duplicate reservation dimension")
		}
		seenDimensions[reservation.Dimension] = struct{}{}
		if fixedCountDimension(reservation.Dimension) && reservation.UpperBound != 1 {
			return fmt.Errorf("fixed-count budget dimension %q must reserve exactly one", reservation.Dimension)
		}
	}
	required := requiredBudgetDimensions(v.Kind)
	if len(seenDimensions) != len(required) {
		return fmt.Errorf("physical kind %q requires exactly %d budget dimensions", v.Kind, len(required))
	}
	for _, dimension := range required {
		if _, exists := seenDimensions[dimension]; !exists {
			return fmt.Errorf("physical kind %q lacks required budget dimension %q", v.Kind, dimension)
		}
	}
	return nil
}

func requiredBudgetDimensions(kind PhysicalCallKind) []BudgetDimension {
	switch kind {
	case PhysicalLLMRequest:
		return []BudgetDimension{BudgetLLMCalls, BudgetLLMInputTokens, BudgetLLMOutputTokens, BudgetExternalCostMicroUSD}
	case PhysicalSimilarityRequest:
		return []BudgetDimension{BudgetSimilarityCalls, BudgetSimilarityCostMicroUSD}
	case PhysicalDockerContainerCreate:
		return []BudgetDimension{BudgetDockerContainerCreates}
	case PhysicalLocalArtifactWrite:
		return []BudgetDimension{BudgetArtifactPhysicalNewBytes}
	case PhysicalDockerEnginePing:
		return nil
	default:
		return nil
	}
}

func fixedCountDimension(dimension BudgetDimension) bool {
	return dimension == BudgetLLMCalls || dimension == BudgetSimilarityCalls || dimension == BudgetDockerContainerCreates
}

type PrepareCallsRequest struct {
	RunID              RunID              `json:"run_id"`
	ExpectedRunVersion int64              `json:"expected_run_version"`
	StageName          StageName          `json:"stage_name"`
	AttemptID          AttemptID          `json:"attempt_id"`
	CallRecordID       CallRecordID       `json:"call_record_id"`
	PlanDigest         Digest             `json:"plan_digest"`
	Calls              []PhysicalCallPlan `json:"calls"`
	IdempotencyKey     string             `json:"idempotency_key"`
	At                 time.Time          `json:"at"`
}

func (v PrepareCallsRequest) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	for _, check := range []error{v.StageName.Validate(), v.AttemptID.Validate(), v.CallRecordID.Validate(), v.PlanDigest.Validate()} {
		if check != nil {
			return check
		}
	}
	if len(v.Calls) == 0 {
		return errors.New("prepared call bundle must not be empty")
	}
	seenIDs := make(map[AttemptCallID]struct{}, len(v.Calls))
	seenOrdinals := make(map[int64]struct{}, len(v.Calls))
	retryOrdinals := make([]int64, 0, len(v.Calls))
	for _, call := range v.Calls {
		if err := call.Validate(); err != nil {
			return err
		}
		if call.Ordinal != call.RetryOrdinal {
			return errors.New("physical and retry ordinals must identify the same global attempt")
		}
		if _, exists := seenIDs[call.ID]; exists {
			return errors.New("call bundle contains duplicate physical call ID")
		}
		if _, exists := seenOrdinals[call.Ordinal]; exists {
			return errors.New("call bundle contains duplicate physical ordinal")
		}
		seenIDs[call.ID], seenOrdinals[call.Ordinal] = struct{}{}, struct{}{}
		retryOrdinals = append(retryOrdinals, call.RetryOrdinal)
	}
	ordinals := make([]int64, 0, len(seenOrdinals))
	for ordinal := range seenOrdinals {
		ordinals = append(ordinals, ordinal)
	}
	slices.Sort(ordinals)
	slices.Sort(retryOrdinals)
	for index := range ordinals {
		want := int64(index + 1)
		if ordinals[index] != want || retryOrdinals[index] != want {
			return errors.New("physical and retry ordinals must form one global contiguous sequence")
		}
	}
	return nil
}

type PreparedCalls struct {
	Call          CallRecord          `json:"call"`
	PhysicalCalls []PhysicalCall      `json:"physical_calls"`
	Reservations  []BudgetReservation `json:"reservations"`
	Failure       *PortFailure        `json:"failure,omitempty"`
	CallTrace     *CallTrace          `json:"call_trace,omitempty"`
}

func (v PreparedCalls) Validate() error {
	if err := v.Call.Validate(); err != nil {
		return err
	}
	if v.Call.State == CallRecordTerminal {
		if v.CallTrace == nil || (v.Failure == nil) != (v.Call.Failure == nil) {
			return errors.New("terminal prepared bundle has invalid projection")
		}
		if v.Failure != nil {
			if err := v.Failure.Validate(); err != nil {
				return err
			}
			if v.Failure.Code != v.Call.Failure.Code || v.Failure.Class != v.Call.Failure.Class {
				return errors.New("terminal prepared bundle failure differs from call record")
			}
		}
		if err := v.CallTrace.Validate(); err != nil {
			return err
		}
	} else if v.Failure != nil || v.CallTrace != nil || v.Call.State != CallRecordPrepared || len(v.PhysicalCalls) == 0 {
		return errors.New("prepared bundle has invalid cardinality")
	}
	for _, physical := range v.PhysicalCalls {
		if err := physical.Validate(); err != nil {
			return err
		}
	}
	for _, reservation := range v.Reservations {
		if err := reservation.Validate(); err != nil {
			return err
		}
	}
	if v.Call.State == CallRecordTerminal && *v.Call.DispatchKind == DispatchDispatched && len(v.PhysicalCalls) == 0 {
		return errors.New("terminal DISPATCHED bundle lacks physical calls")
	}
	return nil
}

type BeginDispatchRequest struct {
	RunID              RunID         `json:"run_id"`
	ExpectedRunVersion int64         `json:"expected_run_version"`
	StageName          StageName     `json:"stage_name"`
	AttemptID          AttemptID     `json:"attempt_id"`
	CallRecordID       CallRecordID  `json:"call_record_id"`
	AttemptCallID      AttemptCallID `json:"attempt_call_id"`
	IdempotencyKey     string        `json:"idempotency_key"`
	At                 time.Time     `json:"at"`
}

func (v BeginDispatchRequest) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	for _, check := range []error{v.StageName.Validate(), v.AttemptID.Validate(), v.CallRecordID.Validate(), v.AttemptCallID.Validate()} {
		if check != nil {
			return check
		}
	}
	return nil
}

type DispatchGrant struct {
	RunID              RunID            `json:"run_id"`
	ExpectedRunVersion int64            `json:"expected_run_version"`
	StageName          StageName        `json:"stage_name"`
	AttemptID          AttemptID        `json:"attempt_id"`
	CallRecordID       CallRecordID     `json:"call_record_id"`
	AttemptCallID      AttemptCallID    `json:"attempt_call_id"`
	Ordinal            int64            `json:"ordinal"`
	RetryGroup         string           `json:"retry_group"`
	RetryOrdinal       int64            `json:"retry_ordinal"`
	Kind               PhysicalCallKind `json:"kind"`
	Provider           string           `json:"provider"`
	RequestDigest      Digest           `json:"request_digest"`
	IdempotencyKey     string           `json:"idempotency_key"`
	DispatchStartedAt  time.Time        `json:"dispatch_started_at"`
	GrantDigest        Digest           `json:"grant_digest"`
}

func (v DispatchGrant) Validate() error {
	for _, check := range []error{v.RunID.Validate(), v.StageName.Validate(), v.AttemptID.Validate(), v.CallRecordID.Validate(), v.AttemptCallID.Validate(), v.RequestDigest.Validate(), v.GrantDigest.Validate(), validateIdempotencyKey(v.IdempotencyKey), validateUTCTime("dispatch started at", v.DispatchStartedAt)} {
		if check != nil {
			return check
		}
	}
	if v.ExpectedRunVersion <= 0 || v.Ordinal <= 0 || v.RetryOrdinal <= 0 || strings.TrimSpace(v.RetryGroup) == "" || !v.Kind.Valid() || strings.TrimSpace(v.Provider) == "" {
		return errors.New("dispatch grant is invalid")
	}
	return nil
}

type ReservationUsage struct {
	ReservationID ReservationID   `json:"reservation_id"`
	Dimension     BudgetDimension `json:"dimension"`
	Subkey        string          `json:"subkey"`
	Value         int64           `json:"value"`
	Verified      bool            `json:"verified"`
}

func (v ReservationUsage) Validate() error {
	if err := v.ReservationID.Validate(); err != nil {
		return err
	}
	if !v.Dimension.Valid() || strings.TrimSpace(v.Subkey) == "" || len(v.Subkey) > 128 {
		return errors.New("reservation usage dimension or subkey is invalid")
	}
	return nil
}

type CompletePhysicalRequest struct {
	RunID              RunID               `json:"run_id"`
	ExpectedRunVersion int64               `json:"expected_run_version"`
	StageName          StageName           `json:"stage_name"`
	AttemptID          AttemptID           `json:"attempt_id"`
	CallRecordID       CallRecordID        `json:"call_record_id"`
	AttemptCallID      AttemptCallID       `json:"attempt_call_id"`
	State              PhysicalCallState   `json:"state"`
	Outcome            PhysicalOutcomeKind `json:"outcome"`
	Failure            *PortFailure        `json:"failure,omitempty"`
	ProviderRequestID  string              `json:"provider_request_id,omitempty"`
	ResponseDigest     *Digest             `json:"response_digest,omitempty"`
	Usage              []ReservationUsage  `json:"usage"`
	IdempotencyKey     string              `json:"idempotency_key"`
	At                 time.Time           `json:"at"`
}

func (v CompletePhysicalRequest) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	for _, check := range []error{v.StageName.Validate(), v.AttemptID.Validate(), v.CallRecordID.Validate(), v.AttemptCallID.Validate()} {
		if check != nil {
			return check
		}
	}
	if !v.State.Valid() || !v.Outcome.Valid() {
		return errors.New("physical completion state or outcome is invalid")
	}
	if v.ResponseDigest != nil {
		if err := v.ResponseDigest.Validate(); err != nil {
			return err
		}
	}
	if v.Failure != nil {
		if err := v.Failure.Validate(); err != nil {
			return err
		}
	}
	switch v.State {
	case PhysicalCompleted:
		if v.ProviderRequestID == "" || v.ResponseDigest == nil {
			return errors.New("completed request lacks provider identity or response digest")
		}
		if v.Outcome == PhysicalOutcomeSuccess {
			if v.Failure != nil {
				return errors.New("successful completion carries failure")
			}
		} else if v.Outcome == PhysicalOutcomeRetryableFailure {
			if v.Failure == nil || v.Failure.Class != FailureRetryable {
				return errors.New("retryable completion lacks retryable failure")
			}
		} else if v.Outcome != PhysicalOutcomePermanentFailure || v.Failure == nil || v.Failure.Class == FailureRetryable || v.Failure.Class == FailureUnknown {
			return errors.New("completed request has invalid outcome")
		}
	case PhysicalAbortedNoDispatch:
		if v.Outcome != PhysicalOutcomeNoSend || v.Failure == nil || v.ProviderRequestID != "" || v.ResponseDigest != nil || len(v.Usage) != 0 {
			return errors.New("confirmed no-send completion is invalid")
		}
	case PhysicalUnknown:
		if v.Outcome != PhysicalOutcomeUnknown || v.Failure == nil || v.Failure.Code != FailureBoundaryUnknown || v.Failure.Class != FailureUnknown || v.ResponseDigest != nil {
			return errors.New("unknown-boundary completion is invalid")
		}
	default:
		return errors.New("physical completion requires a terminal state")
	}
	seen := make(map[ReservationID]struct{}, len(v.Usage))
	for _, usage := range v.Usage {
		if err := usage.Validate(); err != nil {
			return err
		}
		if _, exists := seen[usage.ReservationID]; exists {
			return errors.New("physical completion contains duplicate reservation usage")
		}
		seen[usage.ReservationID] = struct{}{}
	}
	return nil
}

type FinishCallRequest struct {
	RunID                   RunID          `json:"run_id"`
	ExpectedRunVersion      int64          `json:"expected_run_version"`
	StageName               StageName      `json:"stage_name"`
	AttemptID               AttemptID      `json:"attempt_id"`
	CallRecordID            CallRecordID   `json:"call_record_id"`
	DispatchKind            DispatchKind   `json:"dispatch_kind"`
	ResultAttemptCallID     *AttemptCallID `json:"result_attempt_call_id,omitempty"`
	CacheSourceCallRecordID *CallRecordID  `json:"cache_source_call_record_id,omitempty"`
	CacheHitCallRecordID    *CallRecordID  `json:"cache_hit_call_record_id,omitempty"`
	Failure                 *PortFailure   `json:"failure,omitempty"`
	IdempotencyKey          string         `json:"idempotency_key"`
	At                      time.Time      `json:"at"`
}

func (v FinishCallRequest) Validate() error {
	if err := validateMutation(v.RunID, v.ExpectedRunVersion, v.IdempotencyKey, v.At); err != nil {
		return err
	}
	for _, check := range []error{v.StageName.Validate(), v.AttemptID.Validate(), v.CallRecordID.Validate()} {
		if check != nil {
			return check
		}
	}
	if v.Failure != nil {
		if err := v.Failure.Validate(); err != nil {
			return err
		}
	}
	switch v.DispatchKind {
	case DispatchDispatched:
		if v.ResultAttemptCallID == nil || v.CacheSourceCallRecordID != nil || v.CacheHitCallRecordID != nil {
			return errors.New("DISPATCHED finish has invalid result fields")
		}
		return v.ResultAttemptCallID.Validate()
	case DispatchCacheHit:
		if v.ResultAttemptCallID != nil || v.CacheSourceCallRecordID == nil || v.CacheHitCallRecordID == nil || *v.CacheHitCallRecordID != v.CallRecordID || *v.CacheSourceCallRecordID == *v.CacheHitCallRecordID || v.Failure != nil {
			return errors.New("CACHE_HIT finish has invalid logical provenance")
		}
		return v.CacheSourceCallRecordID.Validate()
	case DispatchNone:
		if v.ResultAttemptCallID != nil || v.CacheSourceCallRecordID != nil || v.CacheHitCallRecordID != nil || v.Failure == nil {
			return errors.New("NO_DISPATCH finish has invalid terminal fields")
		}
		return nil
	default:
		return errors.New("finish call dispatch kind is invalid")
	}
}

type CallPlan struct {
	Digest Digest             `json:"digest"`
	Calls  []PhysicalCallPlan `json:"calls"`
}

func (v CallPlan) Validate(policy RetryPolicy) error {
	if err := v.Digest.Validate(); err != nil {
		return err
	}
	request := PrepareCallsRequest{
		RunID: "run_00000000000000000000000000000000", ExpectedRunVersion: 1,
		StageName: "plan", AttemptID: "attempt_00000000000000000000000000000000",
		CallRecordID: "callrec_00000000000000000000000000000000", PlanDigest: v.Digest,
		Calls: v.Calls, IdempotencyKey: "plan_00000000000000000000000000000000", At: time.Unix(1, 0).UTC(),
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if int64(len(v.Calls)) > policy.MaxAttempts {
		return errors.New("call plan exceeds persisted retry bound")
	}
	return nil
}

type CallPlanDecision struct {
	Plan    *CallPlan    `json:"plan,omitempty"`
	Failure *PortFailure `json:"failure,omitempty"`
}

func (v CallPlanDecision) Validate(policy RetryPolicy) error {
	if (v.Plan == nil) == (v.Failure == nil) {
		return errors.New("call plan decision requires exactly one plan or failure")
	}
	if v.Plan != nil {
		return v.Plan.Validate(policy)
	}
	return v.Failure.Validate()
}

type PhysicalBoundary string

const (
	BoundaryConfirmedNoSend PhysicalBoundary = "CONFIRMED_NO_SEND"
	BoundaryCompleted       PhysicalBoundary = "COMPLETED"
	BoundaryUnknown         PhysicalBoundary = "UNKNOWN"
)

func (v PhysicalBoundary) Valid() bool {
	return v == BoundaryConfirmedNoSend || v == BoundaryCompleted || v == BoundaryUnknown
}

type PhysicalExecution[T any] struct {
	Boundary          PhysicalBoundary   `json:"boundary"`
	Value             *T                 `json:"value,omitempty"`
	Failure           *PortFailure       `json:"failure,omitempty"`
	ProviderRequestID string             `json:"provider_request_id,omitempty"`
	ResponseDigest    *Digest            `json:"response_digest,omitempty"`
	Usage             []ReservationUsage `json:"usage"`
}

func (v PhysicalExecution[T]) Validate() error {
	if !v.Boundary.Valid() || (v.Value == nil) == (v.Failure == nil) {
		return errors.New("physical execution boundary or terminal cardinality is invalid")
	}
	if v.Failure != nil {
		if err := v.Failure.Validate(); err != nil {
			return err
		}
	}
	if v.ResponseDigest != nil {
		if err := v.ResponseDigest.Validate(); err != nil {
			return err
		}
	}
	switch v.Boundary {
	case BoundaryCompleted:
		if v.ProviderRequestID == "" || v.ResponseDigest == nil {
			return errors.New("completed execution lacks provider identity or response digest")
		}
	case BoundaryConfirmedNoSend:
		if v.Failure == nil || v.ProviderRequestID != "" || v.ResponseDigest != nil || len(v.Usage) != 0 {
			return errors.New("confirmed no-send execution has external result fields")
		}
	case BoundaryUnknown:
		if v.Failure == nil || v.Failure.Code != FailureBoundaryUnknown || v.Failure.Class != FailureUnknown || v.ResponseDigest != nil {
			return errors.New("unknown execution lacks boundary-unknown failure")
		}
	}
	for _, usage := range v.Usage {
		if err := usage.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func validateLogicalOperationID(value string) error {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("invalid logical operation ID %q", value)
	}
	return nil
}

func safeAddInt64(left, right int64) (int64, bool) {
	if left < 0 || right < 0 || left > math.MaxInt64-right {
		return 0, false
	}
	return left + right, true
}
