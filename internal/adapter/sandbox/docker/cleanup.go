package docker

import (
	"context"
	"errors"
	"fmt"

	"github.com/containerd/errdefs"
	moby "github.com/moby/moby/client"
)

const portableStopGraceSeconds = 1

type StopProof struct {
	WaitObserved   bool
	InspectStopped bool
	NotFound       bool
}

func (p StopProof) Validate() error {
	if !p.WaitObserved && !p.InspectStopped {
		return fmt.Errorf("target stop is unproven")
	}
	return nil
}

func portableStop(ctx context.Context, engine Engine, id string, verify func(moby.ContainerInspectResult) error) (StopProof, error) {
	if ctx == nil || engine == nil || id == "" || verify == nil {
		return StopProof{}, fmt.Errorf("portable stop requires context, Engine, exact ID, and ownership verifier")
	}
	grace := portableStopGraceSeconds
	var controlErrors []error
	if _, err := engine.ContainerStop(ctx, id, moby.ContainerStopOptions{Signal: "SIGTERM", Timeout: &grace}); err != nil && !errdefs.IsNotFound(err) && !errdefs.IsConflict(err) {
		controlErrors = append(controlErrors, fmt.Errorf("stop target: %w", err))
	}
	if _, err := engine.ContainerKill(ctx, id, moby.ContainerKillOptions{Signal: "SIGKILL"}); err != nil && !errdefs.IsNotFound(err) && !errdefs.IsConflict(err) {
		controlErrors = append(controlErrors, fmt.Errorf("kill target: %w", err))
	}
	proof := StopProof{}
	if _, err := waitContainer(ctx, engine, id); err == nil {
		proof.WaitObserved = true
	} else {
		controlErrors = append(controlErrors, fmt.Errorf("wait for stopped target: %w", err))
	}
	inspected, inspectErr := engine.ContainerInspect(ctx, id, moby.ContainerInspectOptions{})
	switch {
	case inspectErr == nil:
		if err := verify(inspected); err != nil {
			return StopProof{}, errors.Join(append(controlErrors, err)...)
		}
		if inspected.Container.State != nil && !inspected.Container.State.Running && inspected.Container.State.Pid == 0 {
			proof.InspectStopped = true
		} else if proof.WaitObserved {
			return StopProof{}, errors.Join(append(controlErrors, fmt.Errorf("Wait and Inspect provide contradictory target state"))...)
		}
	case errdefs.IsNotFound(inspectErr):
		proof.NotFound = true
	default:
		controlErrors = append(controlErrors, fmt.Errorf("inspect stopped target: %w", inspectErr))
	}
	if err := proof.Validate(); err != nil {
		return StopProof{}, errors.Join(append(controlErrors, err)...)
	}
	return proof, nil
}
