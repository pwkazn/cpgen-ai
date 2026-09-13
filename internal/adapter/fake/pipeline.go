package fake

import (
	"context"
	"errors"
	"fmt"

	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

type Pipeline struct {
	prepare    workflow.Step[domain.FakeInput, domain.FakePrepared]
	exercise   workflow.Step[domain.FakePrepared, domain.FakeEvidence]
	checkpoint workflow.Step[domain.FakeEvidence, domain.FakeCheckpoint]
}

type DependencyRevalidator interface {
	Revalidate(context.Context, domain.RunView, domain.BlockedCheckpoint) (bool, error)
}

func NewPipeline(
	prepare workflow.Step[domain.FakeInput, domain.FakePrepared],
	exercise workflow.Step[domain.FakePrepared, domain.FakeEvidence],
	checkpoint workflow.Step[domain.FakeEvidence, domain.FakeCheckpoint],
) (Pipeline, error) {
	pipeline := Pipeline{prepare: prepare, exercise: exercise, checkpoint: checkpoint}
	if err := pipeline.Validate(); err != nil {
		return Pipeline{}, err
	}
	return pipeline, nil
}

func (p Pipeline) Validate() error {
	if p.prepare == nil || p.exercise == nil || p.checkpoint == nil {
		return errors.New("all fake pipeline steps are required")
	}
	if p.prepare.Name() != "prepare" || p.exercise.Name() != "exercise" || p.checkpoint.Name() != "checkpoint" {
		return errors.New("fake pipeline has an invalid stage name")
	}
	return nil
}

func (p Pipeline) Prepare() workflow.Step[domain.FakeInput, domain.FakePrepared] { return p.prepare }
func (p Pipeline) Exercise() workflow.Step[domain.FakePrepared, domain.FakeEvidence] {
	return p.exercise
}
func (p Pipeline) Checkpoint() workflow.Step[domain.FakeEvidence, domain.FakeCheckpoint] {
	return p.checkpoint
}

func (p Pipeline) Revalidate(ctx context.Context, view domain.RunView, stage domain.StageName, binding domain.BlockedCheckpoint) (bool, error) {
	var candidate any
	switch stage {
	case "prepare":
		candidate = p.prepare
	case "exercise":
		candidate = p.exercise
	case "checkpoint":
		candidate = p.checkpoint
	default:
		return false, fmt.Errorf("unsupported fake stage %q", stage)
	}
	revalidator, ok := candidate.(DependencyRevalidator)
	if !ok {
		return true, nil
	}
	return revalidator.Revalidate(ctx, view, binding)
}
