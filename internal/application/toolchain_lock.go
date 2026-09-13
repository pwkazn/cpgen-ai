package application

import (
	"bytes"
	"errors"
	"os"

	"cpgen/internal/config"
	"cpgen/internal/toolchain"
)

// LoadConfiguredToolchainLock reads the immutable lock selected by cfg. A
// persisted snapshot takes precedence over the external path so historical
// runs remain readable after that path is removed or changed.
func LoadConfiguredToolchainLock(cfg config.Config) (toolchain.Lock, error) {
	if cfg.Sandbox == nil {
		return toolchain.Lock{}, errors.New("sandbox configuration is required")
	}
	if len(cfg.Sandbox.ToolchainLockSnapshot) != 0 {
		lock, err := toolchain.LoadLock(bytes.NewReader(cfg.Sandbox.ToolchainLockSnapshot))
		if err != nil {
			return toolchain.Lock{}, err
		}
		digest, err := lock.Digest()
		if err != nil {
			return toolchain.Lock{}, err
		}
		if digest != cfg.Sandbox.ToolchainLockDigest {
			return toolchain.Lock{}, errors.New("sandbox toolchain lock differs from its configured digest")
		}
		return lock, nil
	}
	file, err := os.Open(cfg.Sandbox.ToolchainLockPath)
	if err != nil {
		return toolchain.Lock{}, err
	}
	lock, loadErr := toolchain.LoadLock(file)
	closeErr := file.Close()
	if err := errors.Join(loadErr, closeErr); err != nil {
		return toolchain.Lock{}, err
	}
	digest, err := lock.Digest()
	if err != nil {
		return toolchain.Lock{}, err
	}
	if digest != cfg.Sandbox.ToolchainLockDigest {
		return toolchain.Lock{}, errors.New("sandbox toolchain lock differs from its configured digest")
	}
	return lock, nil
}

// bindToolchainLockSnapshot validates the configured file once and stores the
// canonical lock bytes in the effective configuration used by new runs.
func bindToolchainLockSnapshot(cfg *config.Config) error {
	if cfg == nil || cfg.Sandbox == nil || cfg.Sandbox.ToolchainLockSnapshot != nil {
		return nil
	}
	lock, err := LoadConfiguredToolchainLock(*cfg)
	if err != nil {
		return err
	}
	encoded, err := lock.MarshalIndent()
	if err != nil {
		return err
	}
	copy := *cfg.Sandbox
	copy.ToolchainLockSnapshot = encoded
	cfg.Sandbox = &copy
	return nil
}
