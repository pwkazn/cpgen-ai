package workflow

import "cpgen/internal/domain"

// Revision values are persisted identities, including their historical spelling.
// Changing these strings would change workflow digests and break stored runs.
const (
	GenerationRevision         = "mvp.idea.statement.similarity.solution.data.judge.package.v1"
	RetryingGenerationRevision = "mvp.idea.statement.similarity.solution.data.judge.package.v2"
	FakeRevision               = "slice1.fake.v1"

	LegacySimilarityRevision           = "slice2.idea.statement.similarity.v1"
	LegacySimilarityCheckpointRevision = "slice2.idea.statement.similarity.checkpoint.v1"
	LegacySolutionCheckpointRevision   = "slice3.idea.statement.similarity.solution.checkpoint.v1"
)

func ProducesPackage(revision string) bool {
	return revision == GenerationRevision || revision == RetryingGenerationRevision
}

// Historical runs use the same application scheduler and executors. Only their
// stored stage order and stopping points differ from the complete pipeline.
func legacyStages(revision string) []domain.StageName {
	switch revision {
	case LegacySimilarityRevision:
		return []domain.StageName{"idea", "statement", "similarity"}
	case LegacySimilarityCheckpointRevision:
		return []domain.StageName{"idea", "statement", "similarity", "slice2_checkpoint"}
	case LegacySolutionCheckpointRevision:
		return []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_checkpoint"}
	default:
		return nil
	}
}
