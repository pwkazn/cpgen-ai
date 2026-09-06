package domain

import (
	"errors"
	"fmt"
	"time"
)

// RetryableFailure is a typed, durable failure outcome. It contains no error
// interface so its evidence can be serialized and replayed exactly.
type RetryableFailure struct {
	Code       PortFailureCode `json:"code"`
	RetryAfter *time.Time      `json:"retry_after,omitempty"`
	Evidence   Digest          `json:"evidence"`
}

func (f RetryableFailure) Validate() error {
	if !f.Code.Valid() || f.Evidence == "" {
		return errors.New("retryable failure code and evidence are required")
	}
	if err := f.Evidence.Validate(); err != nil {
		return err
	}
	if f.RetryAfter != nil {
		return validateUTCTime("retry after", *f.RetryAfter)
	}
	return nil
}

type PermanentFailure struct {
	Code     PortFailureCode `json:"code"`
	Evidence Digest          `json:"evidence"`
}

func (f PermanentFailure) Validate() error {
	if !f.Code.Valid() || f.Evidence == "" {
		return errors.New("permanent failure code and evidence are required")
	}
	return f.Evidence.Validate()
}

type ReviewRequest struct {
	EvidenceDigest Digest `json:"evidence_digest"`
	PolicyDigest   Digest `json:"policy_digest"`
	Reason         string `json:"reason"`
	Waivable       bool   `json:"waivable"`
}

func (r ReviewRequest) Validate() error {
	if err := r.EvidenceDigest.Validate(); err != nil {
		return err
	}
	if err := r.PolicyDigest.Validate(); err != nil {
		return err
	}
	if r.Reason == "" {
		return errors.New("review reason is required")
	}
	return nil
}

type CancellationEvidence struct {
	Cause    ExecutionCause `json:"cause"`
	Evidence Digest         `json:"evidence"`
}

func (e CancellationEvidence) Validate() error {
	if e.Cause != CauseUserCancel && e.Cause != CauseRunBudgetDeadline {
		return fmt.Errorf("invalid cancellation cause %q", e.Cause)
	}
	return e.Evidence.Validate()
}

// AgentResult represents exactly one stage outcome. A successful value and
// each control outcome occupy distinct fields; Validate enforces exclusivity.
type AgentResult[O any] struct {
	Value        *O                    `json:"value,omitempty"`
	Retryable    *RetryableFailure     `json:"retryable,omitempty"`
	Blocked      *BlockedCheckpoint    `json:"blocked,omitempty"`
	Review       *ReviewRequest        `json:"review,omitempty"`
	Failure      *PermanentFailure     `json:"failure,omitempty"`
	Cancellation *CancellationEvidence `json:"cancellation,omitempty"`
}

func (r AgentResult[O]) Validate() error {
	count := 0
	if r.Value != nil {
		count++
	}
	if r.Retryable != nil {
		count++
	}
	if r.Blocked != nil {
		count++
	}
	if r.Review != nil {
		count++
	}
	if r.Failure != nil {
		count++
	}
	if r.Cancellation != nil {
		count++
	}
	if count != 1 {
		return fmt.Errorf("agent result requires exactly one outcome, got %d", count)
	}
	if r.Retryable != nil {
		return r.Retryable.Validate()
	}
	if r.Blocked != nil {
		return r.Blocked.Validate()
	}
	if r.Review != nil {
		return r.Review.Validate()
	}
	if r.Failure != nil {
		return r.Failure.Validate()
	}
	if r.Cancellation != nil {
		return r.Cancellation.Validate()
	}
	return nil
}

func (r AgentResult[O]) Outcome() string {
	switch {
	case r.Value != nil:
		return "SUCCESS"
	case r.Retryable != nil:
		return "RETRYABLE"
	case r.Blocked != nil:
		return "BLOCKED"
	case r.Review != nil:
		return "REVIEW"
	case r.Failure != nil:
		return "FAILURE"
	case r.Cancellation != nil:
		return "CANCELLATION"
	default:
		return ""
	}
}

func Success[O any](value O) AgentResult[O] { return AgentResult[O]{Value: &value} }
func Retry[O any](failure RetryableFailure) AgentResult[O] {
	return AgentResult[O]{Retryable: &failure}
}
func Blocked[O any](checkpoint BlockedCheckpoint) AgentResult[O] {
	return AgentResult[O]{Blocked: &checkpoint}
}
func Review[O any](request ReviewRequest) AgentResult[O] { return AgentResult[O]{Review: &request} }
func Failure[O any](failure PermanentFailure) AgentResult[O] {
	return AgentResult[O]{Failure: &failure}
}
func Cancelled[O any](evidence CancellationEvidence) AgentResult[O] {
	return AgentResult[O]{Cancellation: &evidence}
}
