package docker

import (
	"context"
	"runtime"

	moby "github.com/moby/moby/client"
)

type Engine interface {
	Ping(context.Context, moby.PingOptions) (moby.PingResult, error)
	ServerVersion(context.Context, moby.ServerVersionOptions) (moby.ServerVersionResult, error)
	Info(context.Context, moby.InfoOptions) (moby.SystemInfoResult, error)
	ImageInspect(context.Context, string, ...moby.ImageInspectOption) (moby.ImageInspectResult, error)
	Close() error
}

type clientFactory func(...moby.Opt) (*moby.Client, error)

func NewEngineClient(config Config) (Engine, error) {
	return newEngineClient(config, runtime.GOOS, moby.New)
}

func newEngineClient(config Config, goos string, factory clientFactory) (Engine, error) {
	endpoint, err := config.Validate(goos)
	if err != nil {
		return nil, err
	}
	client, err := factory(
		moby.WithHost(endpoint.String()),
		moby.WithAPIVersion(config.APIVersion),
	)
	if err != nil {
		return nil, err
	}
	return client, nil
}
