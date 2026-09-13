package fake

import "cpgen/internal/port"

type PrepareCapabilities struct {
	LLM       port.MeteredLLM
	Artifacts port.MeteredArtifactSink
}

type ExerciseCapabilities struct {
	Sandbox   port.MeteredSandbox
	Artifacts port.MeteredArtifactSink
	Blobs     port.VerifiedBlobReader
}

type CheckpointCapabilities struct {
	Similarity port.MeteredSimilarity
	Cache      port.CacheStore
	Mutations  port.MutationAuthorizer
}
