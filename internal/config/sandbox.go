package config

import (
	"errors"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"cpgen/internal/domain"
)

// SandboxConfig selects only a local Engine and an explicitly pinned lock.
// Execution limits/protocol and helper behavior remain compiled application
// policy. Validation does not inspect Docker or read the lock file.
type SandboxConfig struct {
	EngineEndpoint      string        `json:"engine_endpoint" yaml:"engine_endpoint"`
	ToolchainLockPath   string        `json:"toolchain_lock_path" yaml:"toolchain_lock_path"`
	ToolchainLockDigest domain.Digest `json:"toolchain_lock_digest" yaml:"toolchain_lock_digest"`
}

func (c SandboxConfig) Validate() error {
	for name, value := range map[string]string{"engine_endpoint": c.EngineEndpoint, "toolchain_lock_path": c.ToolchainLockPath} {
		if value == "" || len(value) > 4096 || !utf8.ValidString(value) || strings.ContainsAny(value, "\x00\r\n") || strings.Contains(value, "${") {
			return field("sandbox."+name, errors.New("must be a bounded literal path without controls or interpolation"))
		}
	}
	if !(strings.HasPrefix(c.EngineEndpoint, "unix:///") || strings.HasPrefix(c.EngineEndpoint, "npipe:////./pipe/")) {
		return field("sandbox.engine_endpoint", errors.New("must select an explicit local unix socket or Windows named pipe"))
	}
	if !filepath.IsAbs(c.ToolchainLockPath) {
		return field("sandbox.toolchain_lock_path", errors.New("must be absolute"))
	}
	if err := c.ToolchainLockDigest.Validate(); err != nil {
		return field("sandbox.toolchain_lock_digest", err)
	}
	return nil
}

func effectiveSandbox(c *SandboxConfig) *SandboxConfig {
	if c == nil {
		return nil
	}
	copy := *c
	return &copy
}
