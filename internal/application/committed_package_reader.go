package application

import (
	"context"
	"cpgen/internal/agent"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
	"errors"
)

// CommittedPackageReader constructs the evidence chain from each run's frozen
// settings. It has no stage execution, lock ownership or external transport.
// Its caller owns shared run and artifact locks for the complete read.
type CommittedPackageReadStore interface {
	FrozenReadPolicyStore
	GenerationReadStore
	CommittedLLMReadStore
	SimilarityReadStore
	SimilarityContentReadStore
	SandboxEvidenceReadStore
	PackageEvidenceReadStore
}

type CommittedPackageReader struct {
	store CommittedPackageReadStore
	blobs port.VerifiedBlobReader
}

func NewCommittedPackageReader(store CommittedPackageReadStore, blobs port.VerifiedBlobReader) (*CommittedPackageReader, error) {
	if store == nil || blobs == nil {
		return nil, errors.New("committed package reader requires local storage")
	}
	return &CommittedPackageReader{store, blobs}, nil
}
func (r *CommittedPackageReader) ReadArchive(ctx context.Context, runID domain.RunID) ([]byte, domain.VerifiedPackageRecord, error) {
	var empty domain.VerifiedPackageRecord
	cfg, sandbox, err := NewFrozenReadPolicy(ctx, r.store, runID)
	if err != nil {
		return nil, empty, err
	}
	content, retry, err := BuildGenerationExecutionSettings(cfg)
	if err != nil {
		return nil, empty, err
	}
	modelConfig, _, err := BuildLLMConfig(cfg)
	if err != nil {
		return nil, empty, err
	}
	model, err := agent.NewReadPolicy(modelConfig)
	if err != nil {
		return nil, empty, err
	}
	drafts := make(map[domain.StageName]CommittedDraftReader, 4)
	for _, stage := range []domain.StageName{"idea", "statement", "solution", "data"} {
		repair, err := BuildFormatRepairPolicy(cfg, string(stage)+".draft")
		if err != nil {
			return nil, empty, err
		}
		drafts[stage], err = NewCommittedLLMReader(r.store, r.blobs, model, repair)
		if err != nil {
			return nil, empty, err
		}
	}
	generation, err := NewGenerationReader(r.store, drafts["idea"], drafts["statement"], content)
	if err != nil {
		return nil, empty, err
	}
	providerConfig, policy, err := BuildSimilarityConfig(cfg)
	if err != nil {
		return nil, empty, err
	}
	provider, err := similarity.NewReadPolicy(providerConfig)
	if err != nil {
		return nil, empty, err
	}
	evidence, err := NewSimilarityReader(r.store, r.blobs, provider)
	if err != nil {
		return nil, empty, err
	}
	similarityReader := &SimilarityContentReader{config: similarityReadPolicy{cfg.Workflow.Revision, provider, policy, cfg.Similarity.Limit, retry, cfg.Workflow.SimilarityCostUpperBoundMicroUSD}, reader: evidence, store: r.store, statement: generation}
	solution := &SolutionReader{similarity: similarityReader, generation: generation, calls: drafts["solution"], store: r.store, blobs: r.blobs, revision: cfg.Workflow.Revision}
	data := &DataReader{solution: solution, sandbox: sandbox, generation: generation, calls: drafts["data"], store: r.store, blobs: r.blobs}
	quality := &QualityReader{data: data, store: r.store, blobs: r.blobs, sandbox: sandbox, similarity: similarityReader}
	packages := &PackageReader{quality: quality, similarity: similarityReader, store: r.store, blobs: r.blobs}
	return packages.ReadArchive(ctx, runID)
}
