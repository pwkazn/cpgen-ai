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
	case GenerationRevision, RetryingGenerationRevision:
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

func SupportsGeneration(revision string) bool {
	d, err := DefinitionFor(revision)
	return err == nil && d.UsesGeneration()
}

func HasSolutionStages(revision string) bool {
	d, err := DefinitionFor(revision)
	return err == nil && d.HasStage("solution")
}
