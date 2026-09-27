package application

import (
	"bytes"
	"context"
	"errors"
	"os"

	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/toolchain"
	"cpgen/internal/workflow"
)

// FreezeEffectiveConfig captures the same toolchain snapshot that Bootstrap
// persists for new executable runs, without constructing provider or Docker clients.
func FreezeEffectiveConfig(cfg config.Config) (config.Config, []byte, error) {
	if cfg.Workflow != nil && workflow.HasSolutionStages(cfg.Workflow.Revision) {
		if err := bindToolchainLockSnapshot(&cfg); err != nil {
			return cfg, nil, err
		}
	}
	raw, err := cfg.Effective()
	if err != nil {
		return cfg, nil, err
	}
	return cfg, raw, nil
}

type frozenConfigReader interface {
	GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
	RunViewDocuments(context.Context, domain.RunID) ([]byte, []byte, error)
}

// ConfigForRun restores the exact toolchain snapshot bound to an existing run
// and rejects a configuration that no longer matches the persisted digest.
func ConfigForRun(ctx context.Context, cfg config.Config, runID domain.RunID, reader frozenConfigReader) (config.Config, error) {
	if reader == nil {
		return cfg, errors.New("runtime store cannot read frozen configuration")
	}
	run, err := reader.GetRun(ctx, runID)
	if err != nil {
		return cfg, err
	}
	_, raw, err := reader.RunViewDocuments(ctx, runID)
	if err != nil {
		return cfg, err
	}
	frozen, err := config.DecodeEffective(raw)
	if err != nil {
		return cfg, err
	}
	if domain.SumBytes(raw) != run.ConfigDigest {
		return cfg, errors.New("persisted effective configuration differs from its run binding")
	}
	currentEffective, err := cfg.EffectiveConfig()
	if err != nil {
		return cfg, err
	}
	frozenEffective, err := frozen.EffectiveConfig()
	if err != nil {
		return cfg, err
	}
	if currentEffective.Paths.StateRoot != frozenEffective.Paths.StateRoot {
		return cfg, errors.New("frozen configuration belongs to a different workspace")
	}
	if frozen.Sandbox != nil && len(frozen.Sandbox.ToolchainLockSnapshot) != 0 {
		if _, err := LoadConfiguredToolchainLock(frozen); err != nil {
			return cfg, err
		}
	}
	return frozen, nil
}

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
