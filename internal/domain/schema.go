package domain

import (
	"encoding/json"
	"fmt"
	"regexp"
)

const DomainSchemaVersion SchemaVersion = "cpgen.domain/v1"

var schemaVersionPattern = regexp.MustCompile(`^cpgen\.[a-z0-9][a-z0-9.-]*/v[1-9][0-9]*$`)

// SchemaVersion is a stable, explicit compatibility boundary for serialized objects.
type SchemaVersion string

func ParseSchemaVersion(value string) (SchemaVersion, error) {
	version := SchemaVersion(value)
	if err := version.Validate(); err != nil {
		return "", err
	}
	return version, nil
}

func (v SchemaVersion) Validate() error {
	if !schemaVersionPattern.MatchString(string(v)) {
		return fmt.Errorf("invalid schema version %q", v)
	}
	return nil
}

func (v *SchemaVersion) UnmarshalJSON(data []byte) error {
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("decode schema version: %w", err)
	}
	parsed, err := ParseSchemaVersion(raw)
	if err != nil {
		return err
	}
	*v = parsed
	return nil
}
