package workflow

import (
	"errors"
	"slices"

	"cpgen/internal/domain"
)

// Definition is a compiled compatibility contract, not a configurable graph.
// Persisted names remain unchanged when application constructors evolve.
type Definition struct {
	revision        string
	stages          []domain.StageName
	generation      bool
	preserveAttempt bool
}

func DefinitionFor(revision string) (Definition, error) {
	d := Definition{revision: revision, generation: true, preserveAttempt: true}
	switch revision {
	case GenerationRevision, RetryingGenerationRevision, ExecutedSamplesRevision:
		d.stages = []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_decision", "data", "data_verify", "judge", "quality", "package"}
	case FakeRevision:
		d.stages = []domain.StageName{"prepare", "exercise", "checkpoint"}
		d.generation, d.preserveAttempt = false, false
	default:
		d.stages = legacyStages(revision)
		if d.stages == nil {
			return Definition{}, errors.New("workflow revision has no compiled compatibility definition")
		}
	}
	return d, nil
}

func (d Definition) Revision() string                     { return d.revision }
func (d Definition) Stages() []domain.StageName           { return slices.Clone(d.stages) }
func (d Definition) UsesGeneration() bool                 { return d.generation }
func (d Definition) PreservesAttempt() bool               { return d.preserveAttempt }
func (d Definition) HasStage(stage domain.StageName) bool { return slices.Contains(d.stages, stage) }
func (d Definition) ProducesPackage() bool                { return d.HasStage("package") }

// ManualRevisionTargets lists the current stage and its upstream stages, with
// likely content producers first. The first entry is the workbench default;
// every entry still requires the persisted review and suffix checks on apply.
func (d Definition) ManualRevisionTargets(current domain.StageName) []domain.StageName {
	index := slices.Index(d.stages, current)
	if index < 0 {
		return []domain.StageName{}
	}
	preferred := []domain.StageName{current}
	switch current {
	case "similarity", "similarity_decision", "slice2_checkpoint":
		preferred = []domain.StageName{"idea", "statement"}
	case "solution_decision", "solution_checkpoint":
		preferred = []domain.StageName{"solution", "statement"}
	case "judge":
		preferred = []domain.StageName{"statement", "data"}
	case "quality":
		preferred = []domain.StageName{"solution"}
	}
	upstream := d.stages[:index+1]
	targets := make([]domain.StageName, 0, len(upstream))
	for _, stage := range append(preferred, upstream...) {
		if slices.Contains(upstream, stage) && !slices.Contains(targets, stage) {
			targets = append(targets, stage)
		}
	}
	return targets
}

func SupportsGeneration(revision string) bool {
	d, err := DefinitionFor(revision)
	return err == nil && d.UsesGeneration()
}

func HasSolutionStages(revision string) bool {
	d, err := DefinitionFor(revision)
	return err == nil && d.HasStage("solution")
}
