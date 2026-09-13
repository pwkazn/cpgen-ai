package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	docker "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/config"
)

// Bootstrap pins the local Engine and image lock before any live generation.
// Only the explicit forward revision calls this assembly function.
func bootstrapSolutionSandbox(ctx context.Context, cfg config.Config) (DockerSandboxConfig, func() error, error) {
	var empty DockerSandboxConfig
	if cfg.Sandbox == nil {
		return empty, nil, errors.New("Solution workflow requires sandbox configuration")
	}
	lock, err := LoadConfiguredToolchainLock(cfg)
	if err != nil {
		return empty, nil, err
	}
	engineConfig := docker.Config{EngineEndpoint: cfg.Sandbox.EngineEndpoint, APIVersion: docker.RequiredAPIVersion, BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID), ExecutionProtocol: docker.ExecutionProtocolDockerDirectV2}
	static, err := docker.CheckStatic(ctx, engineConfig)
	if err != nil {
		return empty, nil, err
	}
	engine, err := docker.NewEngineClient(engineConfig)
	if err != nil {
		return empty, nil, err
	}
	paths, err := cfg.EffectiveConfig()
	if err != nil {
		_ = engine.Close()
		return empty, nil, err
	}
	executable, err := os.Executable()
	if err != nil {
		_ = engine.Close()
		return empty, nil, err
	}
	watchdog, err := docker.NewDetachedWatchdogController(docker.DetachedWatchdogOptions{Config: engineConfig, EngineIdentity: static.EngineIdentityDigest, ControlDirectory: filepath.Join(paths.Paths.Runtime, "watchdog"), Executable: executable, ArmTimeout: 10 * time.Second})
	if err != nil {
		_ = engine.Close()
		return empty, nil, err
	}
	return DockerSandboxConfig{Engine: engine, Config: engineConfig, Lock: lock, EngineIdentity: static.EngineIdentityDigest, Watchdog: watchdog, Limits: docker.ControlLimits{HelperMemoryBytes: 128 << 20, HelperPIDs: 16, MaxTransferBytes: 64 << 20, CleanupTimeout: cfg.Runtime.CleanupWait}}, engine.Close, nil
}
