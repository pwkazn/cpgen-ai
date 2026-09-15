package application

import (
	"context"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/agent"
	"cpgen/internal/clock"
	"cpgen/internal/config"
	"cpgen/internal/runlock"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

func bootstrapGenerationRunService(ctx context.Context, cfg config.Config, store *sqlite.Store, blobs *blob.Store, locks *runlock.Manager, reconciler SandboxReconciler, effectiveJSON []byte) (*LocalRunService, func() error, error) {
	content, retry, err := BuildGenerationExecutionSettings(cfg)
	if err != nil {
		return nil, nil, err
	}
	llmConfig, _, err := BuildLLMConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	model, err := agent.NewLangChain(llmConfig)
	if err != nil {
		return nil, nil, err
	}
	ideaRepair, err := BuildFormatRepairPolicy(cfg, "idea.draft")
	if err != nil {
		return nil, nil, err
	}
	statementRepair, err := BuildFormatRepairPolicy(cfg, "statement.draft")
	if err != nil {
		return nil, nil, err
	}
	solutionRepair, err := BuildFormatRepairPolicy(cfg, "solution.draft")
	if err != nil {
		return nil, nil, err
	}
	dataRepair, err := BuildFormatRepairPolicy(cfg, "data.draft")
	if err != nil {
		return nil, nil, err
	}
	generation, err := NewGenerationExecutor(GenerationExecutorConfig{
		Store: store, Blobs: blobs, LLM: model, Clock: clock.Real{}, Locks: locks, Content: content,
		IdeaRepair: ideaRepair, StatementRepair: statementRepair, SolutionRepair: solutionRepair, DataRepair: dataRepair, RetryPolicy: retry, CostUpperBoundMicroUSD: cfg.Workflow.LLMCostUpperBoundMicroUSD,
	})
	if err != nil {
		return nil, nil, err
	}
	similarityConfig, policy, err := BuildSimilarityConfig(cfg)
	if err != nil {
		return nil, nil, err
	}
	provider, err := similarity.New(similarityConfig)
	if err != nil {
		return nil, nil, err
	}
	evidence, err := NewSimilarityExecutorWithConfig(SimilarityStageConfig{Statement: generation.Reader(), Admission: generation.StageAdmission, Store: store, Blobs: blobs, Clock: clock.Real{}, Locks: locks, Provider: provider, Policy: policy, Limit: cfg.Similarity.Limit, RetryPolicy: retry, CostUpperBoundMicroUSD: cfg.Workflow.SimilarityCostUpperBoundMicroUSD, WorkflowRevision: cfg.Workflow.Revision})
	if err != nil {
		return nil, nil, err
	}
	var sandboxConfig *sandboxexec.Config
	var closeSandbox func() error
	if workflow.HasSolutionStages(cfg.Workflow.Revision) {
		base, closer, err := bootstrapSolutionSandbox(ctx, cfg)
		if err != nil {
			return nil, nil, err
		}
		sandboxConfig, reconciler, closeSandbox = &base, nil, closer
	}
	runConfig := GenerationRunConfig{
		Store: store, Blobs: blobs, Clock: clock.Real{}, Locks: locks,
		Generation: generation, Similarity: evidence, Reviews: store,
		ActiveTimeInterval: cfg.Runtime.AccountingHeartbeat, ControlPollInterval: cfg.Runtime.ControlPollInterval,
		Reconciler: reconciler, EffectiveConfigJSON: effectiveJSON, SolutionSandbox: sandboxConfig,
	}
	var service *LocalRunService
	if workflow.ProducesPackage(cfg.Workflow.Revision) {
		service, err = NewGenerationRunService(runConfig)
	} else {
		service, err = newGenerationRunService(runConfig)
	}
	if err != nil {
		if closeSandbox != nil {
			_ = closeSandbox()
		}
		return nil, nil, err
	}
	return service, closeSandbox, nil
}
