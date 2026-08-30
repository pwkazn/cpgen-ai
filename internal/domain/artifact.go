package domain

import "fmt"

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
