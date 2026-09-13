package domain

import (
	"encoding/json"
	"fmt"
)

func unmarshalEnum(data []byte, typeName string, valid func(string) bool, dst *string) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode %s: %w", typeName, err)
	}
	if !valid(value) {
		return fmt.Errorf("unknown %s %q", typeName, value)
	}
	*dst = value
	return nil
}
