package workflow

import (
	"context"
	"errors"
	"fmt"

	"cpgen/internal/domain"
)

const Slice1WorkflowRevision = "slice1.fake.v1"

type Slice1Pipeline struct {
	prepare    Step[domain.Slice1Input, domain.Slice1Prepared]
	exercise   Step[domain.Slice1Prepared, domain.Slice1Evidence]
	checkpoint Step[domain.Slice1Evidence, domain.Slice1Checkpoint]
}

type DependencyRevalidator interface {
	Revalidate(context.Context, domain.RunView, domain.Digest) (bool, error)
}

func NewSlice1Pipeline(
	prepare Step[domain.Slice1Input, domain.Slice1Prepared],
	exercise Step[domain.Slice1Prepared, domain.Slice1Evidence],
	checkpoint Step[domain.Slice1Evidence, domain.Slice1Checkpoint],
) (Slice1Pipeline, error) {
	if prepare == nil || exercise == nil || checkpoint == nil {
		return Slice1Pipeline{}, errors.New("all slice1 steps are required")
	}
	for name, pair := range map[string]struct{ got, want domain.StageName }{
		"prepare": {prepare.Name(), "prepare"}, "exercise": {exercise.Name(), "exercise"}, "checkpoint": {checkpoint.Name(), "checkpoint"},
	} {
		if pair.got != pair.want {
			return Slice1Pipeline{}, fmt.Errorf("%s step has name %q, want %q", name, pair.got, pair.want)
		}
	}
	return Slice1Pipeline{prepare: prepare, exercise: exercise, checkpoint: checkpoint}, nil
}

func (p Slice1Pipeline) Validate() error {
	if p.prepare == nil || p.exercise == nil || p.checkpoint == nil {
		return errors.New("all slice1 pipeline steps are required")
	}
	if p.prepare.Name() != "prepare" || p.exercise.Name() != "exercise" || p.checkpoint.Name() != "checkpoint" {
		return errors.New("slice1 pipeline has an invalid stage name")
	}
	return nil
}

func (p Slice1Pipeline) Prepare() Step[domain.Slice1Input, domain.Slice1Prepared] { return p.prepare }
func (p Slice1Pipeline) Exercise() Step[domain.Slice1Prepared, domain.Slice1Evidence] {
	return p.exercise
}
func (p Slice1Pipeline) Checkpoint() Step[domain.Slice1Evidence, domain.Slice1Checkpoint] {
	return p.checkpoint
}

func (p Slice1Pipeline) RunPrepare(ctx context.Context, view domain.RunView, input domain.Slice1Input) (domain.AgentResult[domain.Slice1Prepared], error) {
	return p.prepare.Run(ctx, view, input)
}
func (p Slice1Pipeline) RunExercise(ctx context.Context, view domain.RunView, input domain.Slice1Prepared) (domain.AgentResult[domain.Slice1Evidence], error) {
	return p.exercise.Run(ctx, view, input)
}
func (p Slice1Pipeline) RunCheckpoint(ctx context.Context, view domain.RunView, input domain.Slice1Evidence) (domain.AgentResult[domain.Slice1Checkpoint], error) {
	return p.checkpoint.Run(ctx, view, input)
}

func (p Slice1Pipeline) Revalidate(ctx context.Context, view domain.RunView, stage domain.StageName, input domain.Digest) (bool, error) {
	var candidate any
	switch stage {
	case "prepare":
		candidate = p.prepare
	case "exercise":
		candidate = p.exercise
	case "checkpoint":
		candidate = p.checkpoint
	default:
		return false, fmt.Errorf("unsupported slice1 stage %q", stage)
	}
	revalidator, ok := candidate.(DependencyRevalidator)
	if !ok {
		return true, nil
	}
	return revalidator.Revalidate(ctx, view, input)
}
