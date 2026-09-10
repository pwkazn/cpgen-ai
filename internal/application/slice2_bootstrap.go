package application

import (
	"context"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/agent"
	"cpgen/internal/clock"
	"cpgen/internal/config"
	"cpgen/internal/runlock"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

func bootstrapSlice2RunService(ctx context.Context, cfg config.Config, store *sqlite.Store, blobs *blob.Store, locks *runlock.Manager, reconciler SandboxReconciler, effectiveJSON []byte) (*LocalRunService, error) {
	content, retry, err := BuildSlice2ExecutionSettings(cfg)
	if err != nil {
		return nil, err
	}
	llmConfig, _, err := BuildLLMConfig(cfg)
	if err != nil {
		return nil, err
	}
	model, err := agent.NewLangChain(llmConfig)
	if err != nil {
		return nil, err
	}
	ideaRepair, err := BuildFormatRepairPolicy(cfg, "idea.draft")
	if err != nil {
		return nil, err
	}
	statementRepair, err := BuildFormatRepairPolicy(cfg, "statement.draft")
	if err != nil {
		return nil, err
	}
	solutionRepair, err := BuildFormatRepairPolicy(cfg, "solution.draft")
	if err != nil {
		return nil, err
	}
	dataRepair, err := BuildFormatRepairPolicy(cfg, "data.draft")
	if err != nil {
		return nil, err
	}
	generation, err := NewGenerationExecutor(GenerationExecutorConfig{
		Store: store, Blobs: blobs, LLM: model, Clock: clock.Real{}, Locks: locks, Content: content,
		IdeaRepair: ideaRepair, StatementRepair: statementRepair, SolutionRepair: solutionRepair, DataRepair: dataRepair, RetryPolicy: retry, CostUpperBoundMicroUSD: cfg.Workflow.LLMCostUpperBoundMicroUSD,
	})
	if err != nil {
		return nil, err
	}
	similarityConfig, policy, err := BuildSimilarityConfig(cfg)
	if err != nil {
		return nil, err
	}
	provider, err := similarity.New(similarityConfig)
	if err != nil {
		return nil, err
	}
	evidence, err := NewSimilarityExecutor(SimilarityExecutorConfig{Generation: generation, Provider: provider, Policy: policy, Limit: cfg.Similarity.Limit, RetryPolicy: retry, CostUpperBoundMicroUSD: cfg.Workflow.SimilarityCostUpperBoundMicroUSD, WorkflowRevision: cfg.Workflow.Revision})
	if err != nil {
		return nil, err
	}
	serviceConfig := Slice2RunServiceConfig{Generation: generation, Similarity: evidence, Reviews: store, ActiveTimeInterval: cfg.Runtime.AccountingHeartbeat, Reconciler: reconciler, EffectiveConfigJSON: effectiveJSON}
	var closeSandbox func() error
	if workflow.HasSolutionStages(cfg.Workflow.Revision) {
		base, closer, err := bootstrapSolutionSandbox(ctx, cfg)
		if err != nil {
			return nil, err
		}
		serviceConfig.SolutionSandbox, serviceConfig.Reconciler, closeSandbox = &base, nil, closer
	}
	service, err := NewSlice2RunService(serviceConfig)
	if err != nil {
		if closeSandbox != nil {
			_ = closeSandbox()
		}
		return nil, err
	}
	service.closeSandbox = closeSandbox
	return service, nil
}
