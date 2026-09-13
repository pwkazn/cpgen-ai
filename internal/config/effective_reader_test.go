package config_test

import (
	"bytes"
	"cpgen/internal/config"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestDecodeEffectivePreservesFrozenIdentity(t *testing.T) {
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.Join(t.TempDir(), "state") + "\nruntime:\n  control_poll_interval: 750ms\n"))
	if err != nil {
		t.Fatal(err)
	}
	original, err := cfg.Effective()
	if err != nil {
		t.Fatal(err)
	}
	restored, err := config.DecodeEffective(original)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := restored.Effective()
	if !bytes.Equal(actual, original) || restored.Runtime.ControlPollInterval != cfg.Runtime.ControlPollInterval {
		t.Fatal("frozen policy changed")
	}
	for _, field := range []string{"schema_version", "paths"} {
		var data map[string]json.RawMessage
		_ = json.Unmarshal(original, &data)
		delete(data, field)
		malformed, _ := json.Marshal(data)
		if _, err := config.DecodeEffective(malformed); err == nil {
			t.Fatalf("accepted missing %s", field)
		}
	}
	var data map[string]json.RawMessage
	_ = json.Unmarshal(original, &data)
	data["paths"] = json.RawMessage(`{"state_root":"foreign"}`)
	malformed, _ := json.Marshal(data)
	if _, err := config.DecodeEffective(malformed); err == nil {
		t.Fatal("accepted foreign derived paths")
	}
}
