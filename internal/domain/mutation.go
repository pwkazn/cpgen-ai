package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

type MutationKind string

const (
	MutationContent  MutationKind = "CONTENT"
	MutationMetadata MutationKind = "METADATA"
)

func (v MutationKind) Valid() bool { return v == MutationContent || v == MutationMetadata }

type MutationClaimRequest struct {
	RunID             RunID        `json:"run_id"`
	StageName         StageName    `json:"stage_name"`
	ScopeDigest       Digest       `json:"scope_digest"`
	SourceBatchDigest Digest       `json:"source_batch_digest"`
	Ordinal           int64        `json:"ordinal"`
	Limit             int64        `json:"limit"`
	LimitSnapshot     int64        `json:"limit_snapshot"`
	Kind              MutationKind `json:"kind"`
	IntentDigest      Digest       `json:"intent_digest"`
	At                time.Time    `json:"at"`
}

func (v MutationClaimRequest) Validate() error {
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	for name, err := range map[string]error{"scope digest": v.ScopeDigest.Validate(), "source batch digest": v.SourceBatchDigest.Validate(), "intent digest": v.IntentDigest.Validate()} {
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	limit := v.LimitSnapshot
	if limit == 0 && v.Limit > 0 {
		limit = v.Limit
	}
	if v.Ordinal <= 0 || v.Limit < 0 || limit < 0 || v.Ordinal > limit || !v.Kind.Valid() {
		return errors.New("mutation claim ordinal or limit snapshot is invalid")
	}
	if err := validateUTCTime("mutation claim at", v.At); err != nil {
		return err
	}
	return nil
}

type MutationGrant struct {
	ClaimID           string       `json:"claim_id"`
	RunID             RunID        `json:"run_id"`
	StageName         StageName    `json:"stage_name"`
	ScopeDigest       Digest       `json:"scope_digest"`
	SourceBatchDigest Digest       `json:"source_batch_digest"`
	Ordinal           int64        `json:"ordinal"`
	LimitSnapshot     int64        `json:"limit_snapshot"`
	Kind              MutationKind `json:"kind"`
	IntentDigest      Digest       `json:"intent_digest"`
	GrantDigest       Digest       `json:"grant_digest"`
}

func (v MutationGrant) Validate() error {
	if err := validateMutationID(v.ClaimID); err != nil {
		return err
	}
	request := MutationClaimRequest{RunID: v.RunID, StageName: v.StageName, ScopeDigest: v.ScopeDigest, SourceBatchDigest: v.SourceBatchDigest, Ordinal: v.Ordinal, Limit: v.LimitSnapshot, LimitSnapshot: v.LimitSnapshot, Kind: v.Kind, IntentDigest: v.IntentDigest, At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if err := request.Validate(); err != nil {
		return err
	}
	return v.GrantDigest.Validate()
}

type MutationOperation struct {
	CallRecordID CallRecordID `json:"call_record_id"`
	AttemptID    AttemptID    `json:"attempt_id"`
}

type MutationReservation struct {
	ReservationID ReservationID `json:"reservation_id"`
	CallRecordID  CallRecordID  `json:"call_record_id"`
	AttemptCallID AttemptCallID `json:"attempt_call_id"`
}

type MutationRecordRequest struct {
	Grant              MutationGrant         `json:"grant"`
	RecordID           string                `json:"record_id"`
	Operations         []MutationOperation   `json:"operations"`
	Reservations       []MutationReservation `json:"reservations"`
	OutputOccurrenceID ArtifactOccurrenceID  `json:"output_occurrence_id"`
	At                 time.Time             `json:"at"`
}

func (v MutationRecordRequest) Validate() error {
	if err := v.Grant.Validate(); err != nil {
		return err
	}
	if err := validateMutationID(v.RecordID); err != nil {
		return err
	}
	if len(v.Operations) == 0 || len(v.Reservations) == 0 {
		return errors.New("mutation record requires operation and reservation evidence")
	}
	for _, item := range v.Operations {
		if err := item.CallRecordID.Validate(); err != nil {
			return err
		}
		if err := item.AttemptID.Validate(); err != nil {
			return err
		}
	}
	for _, item := range v.Reservations {
		if err := item.ReservationID.Validate(); err != nil {
			return err
		}
		if err := item.CallRecordID.Validate(); err != nil {
			return err
		}
		if err := item.AttemptCallID.Validate(); err != nil {
			return err
		}
	}
	if err := v.OutputOccurrenceID.Validate(); err != nil {
		return err
	}
	return validateUTCTime("mutation record at", v.At)
}

func validateMutationID(value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 128 || strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("invalid mutation id %q", value)
	}
	return nil
}
