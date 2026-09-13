package domain

import (
	"fmt"
	"time"
)

type ArtifactRole string

const (
	ArtifactSource       ArtifactRole = "SOURCE"
	ArtifactProgram      ArtifactRole = "PROGRAM"
	ArtifactInput        ArtifactRole = "INPUT"
	ArtifactOutput       ArtifactRole = "OUTPUT"
	ArtifactStdout       ArtifactRole = "STDOUT"
	ArtifactStderr       ArtifactRole = "STDERR"
	ArtifactCompileLog   ArtifactRole = "COMPILE_LOG"
	ArtifactExecutionLog ArtifactRole = "EXECUTION_LOG"
	ArtifactEvidence     ArtifactRole = "EVIDENCE"
)

func (v ArtifactRole) Valid() bool {
	switch v {
	case ArtifactSource, ArtifactProgram, ArtifactInput, ArtifactOutput, ArtifactStdout, ArtifactStderr, ArtifactCompileLog, ArtifactExecutionLog, ArtifactEvidence:
		return true
	default:
		return false
	}
}
func (v *ArtifactRole) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "ArtifactRole", func(raw string) bool { return ArtifactRole(raw).Valid() }, (*string)(v))
}

type BlobRef struct {
	Digest Digest `json:"digest"`
	Size   int64  `json:"size"`
}

type ArtifactWriterState string

const (
	ArtifactWriterPrepared  ArtifactWriterState = "PREPARED"
	ArtifactWriterOpen      ArtifactWriterState = "OPEN"
	ArtifactWriterSealed    ArtifactWriterState = "SEALED"
	ArtifactWriterFinalized ArtifactWriterState = "FINALIZED"
	ArtifactWriterReleased  ArtifactWriterState = "RELEASED"
)

func (v ArtifactWriterState) Valid() bool {
	return v == ArtifactWriterPrepared || v == ArtifactWriterOpen || v == ArtifactWriterSealed || v == ArtifactWriterFinalized || v == ArtifactWriterReleased
}

// ArtifactDeclarationRecord is the immutable, run-scoped declaration loaded
// by a prepared artifact session.
type ArtifactDeclarationRecord struct {
	ID                ArtifactDeclarationID `json:"id"`
	RunID             RunID                 `json:"run_id"`
	StageName         StageName             `json:"stage_name"`
	AttemptID         AttemptID             `json:"attempt_id"`
	CallRecordID      CallRecordID          `json:"call_record_id"`
	AttemptCallID     AttemptCallID         `json:"attempt_call_id"`
	ReservationID     ReservationID         `json:"reservation_id"`
	ReservationSubkey string                `json:"reservation_subkey"`
	MediaType         string                `json:"media_type"`
	Role              ArtifactRole          `json:"role"`
	LogicalPath       SafeRelPath           `json:"logical_path"`
	MaxBytes          int64                 `json:"max_bytes"`
	Provenance        ProvenanceCandidate   `json:"provenance"`
	CreatedAt         time.Time             `json:"created_at"`
}

func (v ArtifactDeclarationRecord) Validate() error {
	for name, err := range map[string]error{
		"declaration id": v.ID.Validate(), "run id": v.RunID.Validate(), "attempt id": v.AttemptID.Validate(),
		"call record id": v.CallRecordID.Validate(), "attempt call id": v.AttemptCallID.Validate(), "reservation id": v.ReservationID.Validate(),
	} {
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if err := v.StageName.Validate(); err != nil {
		return err
	}
	if v.MediaType == "" || !v.Role.Valid() || v.MaxBytes <= 0 || v.ReservationSubkey == "" || len(v.ReservationSubkey) > 128 {
		return fmt.Errorf("artifact declaration fields are invalid")
	}
	if err := v.LogicalPath.Validate(); err != nil {
		return err
	}
	return v.Provenance.Validate()
}

type ArtifactWriterToken struct {
	ID            ArtifactWriterTokenID `json:"id"`
	DeclarationID ArtifactDeclarationID `json:"declaration_id"`
	RunID         RunID                 `json:"run_id"`
	State         ArtifactWriterState   `json:"state"`
	Blob          *BlobRef              `json:"blob,omitempty"`
	PinID         BlobPinID             `json:"pin_id"`
	CreatedAt     time.Time             `json:"created_at"`
}

func (v ArtifactWriterToken) Validate() error {
	if err := v.ID.Validate(); err != nil {
		return err
	}
	if err := v.DeclarationID.Validate(); err != nil {
		return err
	}
	if err := v.RunID.Validate(); err != nil {
		return err
	}
	if err := v.PinID.Validate(); err != nil {
		return err
	}
	if !v.State.Valid() {
		return fmt.Errorf("invalid artifact writer state %q", v.State)
	}
	if v.Blob != nil {
		return v.Blob.Validate()
	}
	if v.State == ArtifactWriterSealed || v.State == ArtifactWriterFinalized {
		return fmt.Errorf("%s writer token requires a blob", v.State)
	}
	return nil
}

func (b BlobRef) Validate() error {
	if err := b.Digest.Validate(); err != nil {
		return fmt.Errorf("blob digest: %w", err)
	}
	if b.Size < 0 {
		return fmt.Errorf("blob size must be non-negative")
	}
	return nil
}

type ProvenanceCandidate struct {
	SchemaVersion SchemaVersion `json:"schema_version"`
	Producer      string        `json:"producer"`
	InputDigest   *Digest       `json:"input_digest,omitempty"`
}

func (p ProvenanceCandidate) Validate() error {
	if err := p.SchemaVersion.Validate(); err != nil {
		return err
	}
	if p.Producer == "" {
		return fmt.Errorf("provenance producer is required")
	}
	if p.InputDigest != nil {
		return p.InputDigest.Validate()
	}
	return nil
}

type PendingArtifact struct {
	Blob             BlobRef               `json:"blob"`
	MediaType        string                `json:"media_type"`
	Role             ArtifactRole          `json:"role"`
	LogicalPath      SafeRelPath           `json:"logical_path"`
	CallID           AttemptCallID         `json:"call_id"`
	ReservationID    ReservationID         `json:"reservation_id"`
	WriterTokenID    ArtifactWriterTokenID `json:"writer_token_id"`
	PinID            BlobPinID             `json:"pin_id"`
	PhysicalNewBytes int64                 `json:"physical_new_bytes"`
	Provenance       ProvenanceCandidate   `json:"provenance"`
}

func (a PendingArtifact) Validate() error {
	if err := a.Blob.Validate(); err != nil {
		return err
	}
	if a.MediaType == "" {
		return fmt.Errorf("artifact media type is required")
	}
	if !a.Role.Valid() {
		return fmt.Errorf("invalid artifact role %q", a.Role)
	}
	if err := a.LogicalPath.Validate(); err != nil {
		return err
	}
	for label, err := range map[string]error{
		"call id": a.CallID.Validate(), "reservation id": a.ReservationID.Validate(),
		"writer token id": a.WriterTokenID.Validate(), "pin id": a.PinID.Validate(),
	} {
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
	}
	if a.PhysicalNewBytes < 0 || a.PhysicalNewBytes > a.Blob.Size {
		return fmt.Errorf("artifact physical bytes are inconsistent with blob size")
	}
	return a.Provenance.Validate()
}

// PendingOccurrenceKind identifies the only two ways a stage may attach an
// artifact.  A new write proves ownership of a finalized writer token; a
// cache reuse proves the source occurrence and current logical cache call.
type PendingOccurrenceKind string

const (
	PendingOccurrenceNewWrite   PendingOccurrenceKind = "NEW_WRITE"
	PendingOccurrenceCacheReuse PendingOccurrenceKind = "CACHE_REUSE"
)

func (v PendingOccurrenceKind) Valid() bool {
	return v == PendingOccurrenceNewWrite || v == PendingOccurrenceCacheReuse
}

// PendingCacheReuse is the provenance-only branch of PendingOccurrence.  It
// deliberately has no writer token, reservation, physical call, or pin: a
// cache hit is a new logical use of an already verified source occurrence.
type PendingCacheReuse struct {
	CacheReuseRecordID  CacheReuseRecordID   `json:"cache_reuse_record_id"`
	SourceOccurrenceID  ArtifactOccurrenceID `json:"source_occurrence_id"`
	SourceCallRecordID  CallRecordID         `json:"source_call_record_id"`
	CurrentCallRecordID CallRecordID         `json:"current_call_record_id"`
	Blob                BlobRef              `json:"blob"`
	MediaType           string               `json:"media_type"`
	Role                ArtifactRole         `json:"role"`
	LogicalPath         SafeRelPath          `json:"logical_path"`
	Provenance          ProvenanceCandidate  `json:"provenance"`
}

func (v PendingCacheReuse) Validate() error {
	for name, err := range map[string]error{
		"cache reuse record id":  v.CacheReuseRecordID.Validate(),
		"source occurrence id":   v.SourceOccurrenceID.Validate(),
		"source call record id":  v.SourceCallRecordID.Validate(),
		"current call record id": v.CurrentCallRecordID.Validate(),
	} {
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	if v.SourceCallRecordID == CallRecordID(v.CurrentCallRecordID) {
		return fmt.Errorf("cache reuse source and current call records must differ")
	}
	if err := v.Blob.Validate(); err != nil {
		return err
	}
	if v.MediaType == "" {
		return fmt.Errorf("cache reuse media type is required")
	}
	if !v.Role.Valid() {
		return fmt.Errorf("invalid cache reuse artifact role %q", v.Role)
	}
	if err := v.LogicalPath.Validate(); err != nil {
		return err
	}
	return v.Provenance.Validate()
}

// PendingOccurrence is a strict tagged union.  Exactly one branch must be
// present and it must agree with Kind; this keeps cache hits from smuggling
// physical-write resources into the stage-finish transaction.
type PendingOccurrence struct {
	Kind       PendingOccurrenceKind `json:"kind"`
	NewWrite   *PendingArtifact      `json:"new_write,omitempty"`
	CacheReuse *PendingCacheReuse    `json:"cache_reuse,omitempty"`
}

func (v PendingOccurrence) Validate() error {
	if !v.Kind.Valid() {
		return fmt.Errorf("invalid pending occurrence kind %q", v.Kind)
	}
	switch v.Kind {
	case PendingOccurrenceNewWrite:
		if v.NewWrite == nil || v.CacheReuse != nil {
			return fmt.Errorf("NEW_WRITE occurrence must contain only new_write")
		}
		return v.NewWrite.Validate()
	case PendingOccurrenceCacheReuse:
		if v.CacheReuse == nil || v.NewWrite != nil {
			return fmt.Errorf("CACHE_REUSE occurrence must contain only cache_reuse")
		}
		return v.CacheReuse.Validate()
	default:
		return fmt.Errorf("invalid pending occurrence kind %q", v.Kind)
	}
}
