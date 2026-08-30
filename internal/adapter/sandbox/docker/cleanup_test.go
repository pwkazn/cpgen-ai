package docker

import (
	"context"
	"slices"
	"testing"

	"github.com/moby/moby/api/types/container"
	moby "github.com/moby/moby/client"
)

func TestCleanupUsesStopKillWaitInspectAndRequiresProof(t *testing.T) {
	engine := &cleanupEngine{}
	proof, err := portableStop(context.Background(), engine, "target-id", func(result moby.ContainerInspectResult) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if err := proof.Validate(); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(engine.events, []string{"stop", "kill", "wait", "inspect"}) {
		t.Fatalf("cleanup order = %#v", engine.events)
	}

	unproved := &cleanupEngine{runningAfterStop: true, omitWaitResult: true}
	if _, err := portableStop(context.Background(), unproved, "target-id", func(result moby.ContainerInspectResult) error { return nil }); err == nil {
		t.Fatal("cleanup without a stopped proof succeeded")
	}
}

type cleanupEngine struct {
	Engine
	events           []string
	runningAfterStop bool
	omitWaitResult   bool
}

func (e *cleanupEngine) ContainerStop(context.Context, string, moby.ContainerStopOptions) (moby.ContainerStopResult, error) {
	e.events = append(e.events, "stop")
	return moby.ContainerStopResult{}, nil
}
func (e *cleanupEngine) ContainerKill(context.Context, string, moby.ContainerKillOptions) (moby.ContainerKillResult, error) {
	e.events = append(e.events, "kill")
	return moby.ContainerKillResult{}, nil
}
func (e *cleanupEngine) ContainerWait(context.Context, string, moby.ContainerWaitOptions) moby.ContainerWaitResult {
	e.events = append(e.events, "wait")
	results := make(chan container.WaitResponse, 1)
	errors := make(chan error, 1)
	if !e.omitWaitResult {
		results <- container.WaitResponse{StatusCode: 0}
	}
	close(results)
	close(errors)
	return moby.ContainerWaitResult{Result: results, Error: errors}
}
func (e *cleanupEngine) ContainerInspect(context.Context, string, moby.ContainerInspectOptions) (moby.ContainerInspectResult, error) {
	e.events = append(e.events, "inspect")
	state := &container.State{Running: e.runningAfterStop}
	if e.runningAfterStop {
		state.Pid = 42
	}
	return moby.ContainerInspectResult{Container: container.InspectResponse{ID: "target-id", State: state}}, nil
}

var _ Engine = (*cleanupEngine)(nil)
