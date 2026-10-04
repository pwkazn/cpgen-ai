package workflow

import (
	"slices"
	"testing"

	"cpgen/internal/domain"
)

// Literal wire identities and ordinals must survive Go API renames. These
// values are stored in runs and included in their workflow digests.
func TestDefinitionPreservesPersistedContracts(t *testing.T) {
	for _, tc := range []struct {
		name, revision, persisted string
		stages                    []domain.StageName
		generation, solution, pkg bool
	}{
		{"generation", GenerationRevision, "mvp.idea.statement.similarity.solution.data.judge.package.v1", []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_decision", "data", "data_verify", "judge", "quality", "package"}, true, true, true},
		{"retrying generation", RetryingGenerationRevision, "mvp.idea.statement.similarity.solution.data.judge.package.v2", []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_decision", "data", "data_verify", "judge", "quality", "package"}, true, true, true},
		{"executed samples", ExecutedSamplesRevision, "mvp.idea.statement.similarity.solution.data.judge.package.v3", []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_decision", "data", "data_verify", "judge", "quality", "package"}, true, true, true},
		{"fake", FakeRevision, "slice1.fake.v1", []domain.StageName{"prepare", "exercise", "checkpoint"}, false, false, false},
		{"legacy similarity", LegacySimilarityRevision, "slice2.idea.statement.similarity.v1", []domain.StageName{"idea", "statement", "similarity"}, true, false, false},
		{"legacy similarity checkpoint", LegacySimilarityCheckpointRevision, "slice2.idea.statement.similarity.checkpoint.v1", []domain.StageName{"idea", "statement", "similarity", "slice2_checkpoint"}, true, false, false},
		{"legacy solution checkpoint", LegacySolutionCheckpointRevision, "slice3.idea.statement.similarity.solution.checkpoint.v1", []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_checkpoint"}, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.revision != tc.persisted {
				t.Fatal("persisted revision changed")
			}
			d, err := DefinitionFor(tc.persisted)
			if err != nil {
				t.Fatal(err)
			}
			if d.Revision() != tc.persisted || !slices.Equal(d.Stages(), tc.stages) {
				t.Fatalf("incompatible definition: %+v", d)
			}
			if d.UsesGeneration() != tc.generation || d.PreservesAttempt() != tc.generation || d.ProducesPackage() != tc.pkg || SupportsGeneration(tc.persisted) != tc.generation || HasSolutionStages(tc.persisted) != tc.solution {
				t.Fatalf("incompatible execution behavior: %+v", d)
			}
		})
	}
}

func TestDefinitionDoesNotExposeMutableCompiledStages(t *testing.T) {
	d, err := DefinitionFor(GenerationRevision)
	if err != nil {
		t.Fatal(err)
	}
	stages := d.Stages()
	stages[0] = "package"
	if d.Stages()[0] != "idea" || !d.PreservesAttempt() || !d.ProducesPackage() {
		t.Fatal("definition changed through returned stages")
	}
	old, err := DefinitionFor(LegacySolutionCheckpointRevision)
	if err != nil {
		t.Fatal(err)
	}
	if old.ProducesPackage() || !old.PreservesAttempt() {
		t.Fatal("historical checkpoint acquired current workflow behavior")
	}
	if _, err := DefinitionFor("future"); err == nil {
		t.Fatal("unknown revision accepted")
	}
	if SupportsGeneration("future") || HasSolutionStages("future") {
		t.Fatal("unknown revision acquired execution capabilities")
	}
}

func TestManualRevisionTargetsRespectFrozenStagesAndRecommendProducers(t *testing.T) {
	for _, tc := range []struct {
		revision  string
		current   domain.StageName
		preferred []domain.StageName
	}{
		{GenerationRevision, "similarity_decision", []domain.StageName{"idea", "statement"}},
		{RetryingGenerationRevision, "similarity_decision", []domain.StageName{"idea", "statement"}},
		{ExecutedSamplesRevision, "similarity_decision", []domain.StageName{"idea", "statement"}},
		{ExecutedSamplesRevision, "judge", []domain.StageName{"statement", "data"}},
		{ExecutedSamplesRevision, "solution_decision", []domain.StageName{"solution", "statement"}},
		{ExecutedSamplesRevision, "quality", []domain.StageName{"solution"}},
		{ExecutedSamplesRevision, "idea", []domain.StageName{"idea"}},
		{FakeRevision, "prepare", []domain.StageName{"prepare"}},
		{FakeRevision, "checkpoint", []domain.StageName{"checkpoint"}},
		{LegacySimilarityRevision, "similarity", []domain.StageName{"idea", "statement"}},
		{LegacySimilarityCheckpointRevision, "slice2_checkpoint", []domain.StageName{"idea", "statement"}},
		{LegacySolutionCheckpointRevision, "solution_checkpoint", []domain.StageName{"solution", "statement"}},
	} {
		t.Run(tc.revision+"/"+string(tc.current), func(t *testing.T) {
			d, err := DefinitionFor(tc.revision)
			if err != nil {
				t.Fatal(err)
			}
			targets := d.ManualRevisionTargets(tc.current)
			if len(targets) < len(tc.preferred) || !slices.Equal(targets[:len(tc.preferred)], tc.preferred) {
				t.Fatalf("recommended targets = %v, want prefix %v", targets, tc.preferred)
			}
			stages := d.Stages()
			current := slices.Index(stages, tc.current)
			if len(targets) != current+1 {
				t.Fatalf("targets = %v, want exactly the current stage and its upstream stages", targets)
			}
			seen := map[domain.StageName]bool{}
			for _, target := range targets {
				index := slices.Index(stages, target)
				if index < 0 || index > current || seen[target] {
					t.Fatalf("unavailable or duplicate target %q in %v", target, targets)
				}
				seen[target] = true
			}
			targets[0] = "mutated"
			if d.ManualRevisionTargets(tc.current)[0] != tc.preferred[0] {
				t.Fatal("caller mutated the definition")
			}
			if len(d.ManualRevisionTargets("future")) != 0 {
				t.Fatal("unknown current stage gained revision targets")
			}
		})
	}
}
