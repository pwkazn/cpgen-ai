package application

import (
	"archive/zip"
	"bytes"
	"context"
	"cpgen/internal/agent"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
	"errors"
	"io"
)

// Structured private receipts are never sent to the browser. Rebuild the
// domain content with frozen policy and the existing read-only proof chain.
func (r *WorkbenchReader) readDraftContent(ctx context.Context, id domain.RunID, a port.WorkbenchArtifactSnapshot) ([]byte, error) {
	store, ok := r.store.(CommittedPackageReadStore)
	if !ok {
		return nil, errors.New("committed content reader unavailable")
	}
	run, err := store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	_, raw, err := store.RunViewDocuments(ctx, id)
	if err != nil {
		return nil, err
	}
	if domain.SumBytes(raw) != run.ConfigDigest {
		return nil, errors.New("frozen configuration digest mismatch")
	}
	cfg, err := config.DecodeEffective(raw)
	if err != nil {
		return nil, err
	}
	stage, err := store.ReadCommittedLLMStage(ctx, id, a.StageName)
	if err != nil {
		return nil, err
	}
	found := false
	for _, item := range stage.Artifacts {
		if item.OccurrenceID == a.OccurrenceID {
			found = true
		}
	}
	if !found {
		return nil, errors.New("occurrence is not part of the current committed draft")
	}
	if a.StageName == "statement" && run.State == domain.RunReady {
		packages, err := NewCommittedPackageReader(store, r.blobs)
		if err != nil {
			return nil, err
		}
		raw, _, err := packages.ReadArchive(ctx, id)
		if err != nil {
			return nil, err
		}
		archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			return nil, err
		}
		for _, f := range archive.File {
			if f.Name == "statement/statement.md" {
				rc, err := f.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()
				return io.ReadAll(io.LimitReader(rc, DefaultWorkbenchArtifactLimit+1))
			}
		}
		return nil, errors.New("verified package has no statement")
	}
	content, retry, err := BuildGenerationExecutionSettings(cfg)
	if err != nil {
		return nil, err
	}
	modelConfig, _, err := BuildLLMConfig(cfg)
	if err != nil {
		return nil, err
	}
	model, err := agent.NewReadPolicy(modelConfig)
	if err != nil {
		return nil, err
	}
	drafts := map[domain.StageName]durable.CommittedDraftReader{}
	for _, name := range []domain.StageName{"idea", "statement", "solution"} {
		policy, err := BuildFormatRepairPolicy(cfg, string(name)+".draft")
		if err != nil {
			return nil, err
		}
		drafts[name], err = durable.NewCommittedLLMReader(store, r.blobs, model, policy)
		if err != nil {
			return nil, err
		}
	}
	generation, err := NewGenerationReader(store, drafts["idea"], drafts["statement"], content)
	if err != nil {
		return nil, err
	}
	if a.StageName == "statement" {
		statement, err := generation.ReadStatement(ctx, id)
		if err != nil {
			return nil, err
		}
		return []byte("草稿 · 样例尚未通过最终题包核验\n\n" + renderPackageStatement(statement.Problem)), nil
	}
	providerConfig, policy, err := BuildSimilarityConfig(cfg)
	if err != nil {
		return nil, err
	}
	provider, err := similarity.NewReadPolicy(providerConfig)
	if err != nil {
		return nil, err
	}
	evidence, err := durable.NewSimilarityReader(store, r.blobs, provider)
	if err != nil {
		return nil, err
	}
	similarityReader := &SimilarityContentReader{config: similarityReadPolicy{cfg.Workflow.Revision, provider, policy, cfg.Similarity.Limit, retry, cfg.Workflow.SimilarityCostUpperBoundMicroUSD}, reader: evidence, store: store, statement: generation}
	solution := &SolutionReader{similarity: similarityReader, generation: generation, calls: drafts["solution"], store: store, blobs: r.blobs, revision: cfg.Workflow.Revision}
	draft, err := solution.ReadDraft(ctx, id)
	if err != nil {
		return nil, err
	}
	return []byte(draft.Explanation + "\n\n## 标准解法\n\n```cpp\n" + draft.ReferenceCode + "\n```\n\n## 暴力解法\n\n```cpp\n" + draft.BruteCode + "\n```"), nil
}
