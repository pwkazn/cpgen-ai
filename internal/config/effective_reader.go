package config

import (
	"bytes"
	"encoding/json"
	"errors"
)

// DecodeEffective reconstructs policy from an existing canonical snapshot.
// It reads no credentials or files and refuses any lossy normalization: the
// resulting configuration must reproduce the original persisted bytes.
func DecodeEffective(raw []byte) (Config, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return Config{}, err
	}
	delete(fields, "schema_version")
	delete(fields, "paths")
	input, err := json.Marshal(fields)
	if err != nil {
		return Config{}, err
	}
	cfg, err := Decode(input)
	if err != nil {
		return Config{}, err
	}
	canonical, err := cfg.Effective()
	if err != nil {
		return Config{}, err
	}
	if !bytes.Equal(canonical, raw) {
		return Config{}, errors.New("effective configuration cannot be reconstructed without changing its persisted identity")
	}
	return cfg, nil
}
