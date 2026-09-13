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
	var snapshot []byte
	if sandboxRaw, ok := fields["sandbox"]; ok {
		var sandboxFields map[string]json.RawMessage
		if err := json.Unmarshal(sandboxRaw, &sandboxFields); err != nil {
			return Config{}, err
		}
		if snapshotRaw, ok := sandboxFields["toolchain_lock_snapshot"]; ok {
			if err := json.Unmarshal(snapshotRaw, &snapshot); err != nil {
				return Config{}, err
			}
			delete(sandboxFields, "toolchain_lock_snapshot")
			encodedSandbox, err := json.Marshal(sandboxFields)
			if err != nil {
				return Config{}, err
			}
			fields["sandbox"] = encodedSandbox
		}
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
	if cfg.Sandbox != nil {
		// A nil value means a live config that has not yet been bound. An empty
		// value marks a legacy persisted config and prevents bootstrap from
		// silently adding a new snapshot to its effective identity.
		cfg.Sandbox.ToolchainLockSnapshot = append([]byte{}, snapshot...)
	} else if len(snapshot) != 0 {
		return Config{}, errors.New("effective toolchain snapshot requires sandbox configuration")
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
