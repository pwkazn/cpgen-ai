package domain

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
)

type RunID string
type AttemptID string
type AttemptCallID string
type ReservationID string
type ArtifactWriterTokenID string
type BlobPinID string
type ReviewDecisionID string
type ControlRequestID string
type CallRecordID string
type ArtifactDeclarationID string
type ArtifactOccurrenceID string
type CacheReuseRecordID string
type SandboxExecutionID string
type SandboxResourceID string

var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}_[0-9a-f]{32}$`)

func NewID(prefix string) (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate %s id: %w", prefix, err)
	}
	value := prefix + "_" + hex.EncodeToString(random[:])
	if !idPattern.MatchString(value) {
		return "", fmt.Errorf("invalid id prefix %q", prefix)
	}
	return value, nil
}

func validateID(typeName, value string) error {
	if !idPattern.MatchString(value) {
		return fmt.Errorf("invalid %s %q", typeName, value)
	}
	return nil
}

func unmarshalID(data []byte, typeName string, dst *string) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode %s: %w", typeName, err)
	}
	if err := validateID(typeName, raw); err != nil {
		return err
	}
	*dst = raw
	return nil
}

func (id RunID) Validate() error         { return validateID("run id", string(id)) }
func (id AttemptID) Validate() error     { return validateID("attempt id", string(id)) }
func (id AttemptCallID) Validate() error { return validateID("attempt call id", string(id)) }
func (id ReservationID) Validate() error { return validateID("reservation id", string(id)) }
func (id ArtifactWriterTokenID) Validate() error {
	return validateID("artifact writer token id", string(id))
}
func (id BlobPinID) Validate() error        { return validateID("blob pin id", string(id)) }
func (id ReviewDecisionID) Validate() error { return validateID("review decision id", string(id)) }
func (id ControlRequestID) Validate() error { return validateID("control request id", string(id)) }
func (id CallRecordID) Validate() error     { return validateID("call record id", string(id)) }
func (id ArtifactDeclarationID) Validate() error {
	return validateID("artifact declaration id", string(id))
}
func (id ArtifactOccurrenceID) Validate() error {
	return validateID("artifact occurrence id", string(id))
}
func (id CacheReuseRecordID) Validate() error { return validateID("cache reuse record id", string(id)) }
func (id SandboxExecutionID) Validate() error { return validateID("sandbox execution id", string(id)) }
func (id SandboxResourceID) Validate() error  { return validateID("sandbox resource id", string(id)) }

func (id *RunID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "run id", (*string)(id))
}
func (id *AttemptID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "attempt id", (*string)(id))
}
func (id *AttemptCallID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "attempt call id", (*string)(id))
}
func (id *ReservationID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "reservation id", (*string)(id))
}
func (id *ArtifactWriterTokenID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "artifact writer token id", (*string)(id))
}
func (id *BlobPinID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "blob pin id", (*string)(id))
}
func (id *ReviewDecisionID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "review decision id", (*string)(id))
}
func (id *ControlRequestID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "control request id", (*string)(id))
}
func (id *CallRecordID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "call record id", (*string)(id))
}
func (id *ArtifactDeclarationID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "artifact declaration id", (*string)(id))
}
func (id *ArtifactOccurrenceID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "artifact occurrence id", (*string)(id))
}
func (id *CacheReuseRecordID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "cache reuse record id", (*string)(id))
}
func (id *SandboxExecutionID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "sandbox execution id", (*string)(id))
}
func (id *SandboxResourceID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "sandbox resource id", (*string)(id))
}
