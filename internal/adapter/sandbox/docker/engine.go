package docker

import (
	"context"
	"runtime"

	moby "github.com/moby/moby/client"
)

type StaticEngine interface {
	Ping(context.Context, moby.PingOptions) (moby.PingResult, error)
	ServerVersion(context.Context, moby.ServerVersionOptions) (moby.ServerVersionResult, error)
	Info(context.Context, moby.InfoOptions) (moby.SystemInfoResult, error)
	ImageInspect(context.Context, string, ...moby.ImageInspectOption) (moby.ImageInspectResult, error)
	Close() error
}

type Engine interface {
	StaticEngine
	ContainerCreate(context.Context, moby.ContainerCreateOptions) (moby.ContainerCreateResult, error)
	ContainerStart(context.Context, string, moby.ContainerStartOptions) (moby.ContainerStartResult, error)
	ContainerAttach(context.Context, string, moby.ContainerAttachOptions) (moby.ContainerAttachResult, error)
	ContainerWait(context.Context, string, moby.ContainerWaitOptions) moby.ContainerWaitResult
	ContainerInspect(context.Context, string, moby.ContainerInspectOptions) (moby.ContainerInspectResult, error)
	ContainerStop(context.Context, string, moby.ContainerStopOptions) (moby.ContainerStopResult, error)
	ContainerKill(context.Context, string, moby.ContainerKillOptions) (moby.ContainerKillResult, error)
	ContainerRemove(context.Context, string, moby.ContainerRemoveOptions) (moby.ContainerRemoveResult, error)
	VolumeCreate(context.Context, moby.VolumeCreateOptions) (moby.VolumeCreateResult, error)
	VolumeInspect(context.Context, string, moby.VolumeInspectOptions) (moby.VolumeInspectResult, error)
	VolumeRemove(context.Context, string, moby.VolumeRemoveOptions) (moby.VolumeRemoveResult, error)
	Events(context.Context, moby.EventsListOptions) moby.EventsResult
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
