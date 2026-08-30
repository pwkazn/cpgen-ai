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
type OwnerID string
type ReservationID string
type ArtifactWriterTokenID string
type BlobPinID string

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
func (id OwnerID) Validate() error       { return validateID("owner id", string(id)) }
func (id ReservationID) Validate() error { return validateID("reservation id", string(id)) }
func (id ArtifactWriterTokenID) Validate() error {
	return validateID("artifact writer token id", string(id))
}
func (id BlobPinID) Validate() error { return validateID("blob pin id", string(id)) }

func (id *RunID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "run id", (*string)(id))
}
func (id *AttemptID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "attempt id", (*string)(id))
}
func (id *AttemptCallID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "attempt call id", (*string)(id))
}
func (id *OwnerID) UnmarshalJSON(data []byte) error {
	return unmarshalID(data, "owner id", (*string)(id))
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
