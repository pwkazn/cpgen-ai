package port

import (
	"context"

	"cpgen/internal/domain"
)

// GenerationStore supplies authoritative inputs to application-owned stages.
// The foreground caller holds the run lock. Blob bytes still require a
// separate verified reader after the metadata read transaction ends.
type GenerationStore interface {
	ReadGenerationSnapshot(context.Context, domain.RunID) (domain.GenerationRequestSnapshotV1, error)
	ReadCommittedLLMStage(context.Context, domain.RunID, domain.StageName) (CommittedLLMStage, error)
	ReadStageAttempt(context.Context, domain.RunID, domain.StageName, domain.AttemptID) (domain.StageAttempt, error)
	ReadAttemptLLMCalls(context.Context, domain.RunID, domain.StageName, domain.AttemptID) ([]domain.CallRecord, error)
	ReadStageInputDigest(context.Context, domain.RunID, domain.StageName) (domain.Digest, error)
	ReadAttemptDependencyCheckpoint(context.Context, domain.RunID, domain.StageName, domain.AttemptID) (*domain.BlockedCheckpoint, error)
}

type CommittedPrivateStage struct {
	StageVersion int64
	Attempt      domain.StageAttempt
	Artifacts    []CommittedPrivateStageArtifact
}

type CommittedLLMStage = CommittedPrivateStage

// Current provenance and the original producer are distinct for cache reuse.
// No writer token, dispatch grant or cache-index dependency is returned.
type CommittedPrivateStageArtifact struct {
	Kind                 domain.PendingOccurrenceKind
	OccurrenceID         domain.ArtifactOccurrenceID
	CurrentCallRecordID  domain.CallRecordID
	ProviderCallRecordID domain.CallRecordID
	Source               domain.CacheSource
	Blob                 domain.CacheBlob
}

type CommittedLLMStageArtifact = CommittedPrivateStageArtifact
