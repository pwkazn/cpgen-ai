package application

import (
	"bytes"
	"errors"
	"time"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
	"cpgen/internal/toolchain"
	"cpgen/internal/workflow"
)

// Fixture adapters preserve existing crash/provenance test setup while production
// composition passes stage capabilities directly.

func NewSolutionExecutor(evidence *SimilarityExecutor, generation *GenerationExecutor) (*SolutionExecutor, error) {
	if evidence == nil || generation == nil || evidence.admission != generation.StageAdmission || !workflow.HasSolutionStages(evidence.Revision()) {
		return nil, errors.New("solution executor requires the forward solution workflow")
	}
	calls, err := generation.calls(generation.config.Store, "solution")
	if err != nil {
		return nil, err
	}
	store, ok := generation.config.Store.(SandboxEvidenceReadStore)
	if !ok {
		return nil, errors.New("solution requires committed sandbox evidence storage")
	}
	reader := &SolutionReader{similarity: evidence.Reader(), generation: generation.Reader(), calls: calls, store: store, blobs: generation.config.Blobs, revision: evidence.Revision()}
	return &SolutionExecutor{publisher: newStagePublisher(generation.config.Store, generation.config.Blobs, generation.config.Clock), blobs: generation.config.Blobs, reader: reader, drafts: generation.DraftExecution}, nil
}

func NewDataExecutor(solution *SolutionExecutor, sandbox sandboxexec.Config) (*DataExecutor, error) {
	if solution == nil || !workflow.ProducesPackage(solution.reader.revision) {
		return nil, errors.New("data executor requires the forward MVP workflow")
	}
	raw, err := sandbox.Lock.MarshalIndent()
	if err != nil {
		return nil, err
	}
	sandbox.Lock, err = toolchain.LoadLock(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	calls, err := solution.drafts.calls(solution.drafts.config.Store, "data")
	if err != nil {
		return nil, err
	}
	reader := &DataReader{solution: solution.reader, sandbox: sandbox.ReadPolicy(), generation: solution.reader.generation, calls: calls, store: solution.reader.store, blobs: solution.reader.blobs}
	return &DataExecutor{publisher: solution.publisher, blobs: solution.blobs, reader: reader, drafts: solution.drafts, sandbox: sandbox.ReadPolicy()}, nil
}

func NewQualityExecutor(data *DataExecutor) (*QualityExecutor, error) {
	if data == nil {
		return nil, errors.New("quality requires its current Data/Judge executor")
	}
	return &QualityExecutor{blobs: data.reader.blobs, reader: &QualityReader{data: data.reader, store: data.reader.store, blobs: data.reader.blobs, sandbox: data.sandbox, similarity: data.reader.solution.similarity}, admission: data.drafts.StageAdmission, publisher: newStagePublisher(data.drafts.config.Store, data.drafts.config.Blobs, data.drafts.config.Clock)}, nil
}

func NewPackageExecutor(quality *QualityExecutor) (*PackageExecutor, error) {
	if quality == nil {
		return nil, errors.New("package requires the current Quality executor")
	}
	store, ok := quality.reader.store.(PackageEvidenceReadStore)
	if !ok {
		return nil, errors.New("package requires committed package storage")
	}
	return &PackageExecutor{reader: &PackageReader{quality: quality.reader, similarity: quality.reader.similarity, store: store, blobs: quality.blobs}, admission: quality.admission, publisher: quality.publisher}, nil
}

type SimilarityExecutorConfig struct {
	WorkflowRevision       string
	Generation             *GenerationExecutor
	Provider               similarity.PhysicalProvider
	Policy                 similarity.DecisionPolicy
	Limit                  int
	RetryPolicy            domain.RetryPolicy
	CostUpperBoundMicroUSD int64
}

func NewSimilarityExecutor(c SimilarityExecutorConfig) (*SimilarityExecutor, error) {
	if c.Generation == nil {
		return nil, errors.New("fixture generation is required")
	}
	g := c.Generation.config
	store, ok := g.Store.(SimilarityStageStore)
	if !ok {
		return nil, errors.New("fixture similarity storage is required")
	}
	return NewSimilarityExecutorWithConfig(SimilarityStageConfig{Statement: c.Generation.Reader(), Admission: c.Generation.StageAdmission, Store: store, Blobs: g.Blobs, Clock: g.Clock, Locks: g.Locks, WorkflowRevision: c.WorkflowRevision, Provider: c.Provider, Policy: c.Policy, Limit: c.Limit, RetryPolicy: c.RetryPolicy, CostUpperBoundMicroUSD: c.CostUpperBoundMicroUSD})
}

// Slice2RunServiceConfig deliberately derives the runtime, blobs, provider
// clock and lock ownership from one generation executor. Similarity must share
// that exact executor; unrelated stores cannot be composed accidentally.
type Slice2RunServiceConfig struct {
	Generation          *GenerationExecutor
	Similarity          *SimilarityExecutor
	Reviews             port.ReviewStore
	ActiveTimeInterval  time.Duration
	ControlPollInterval time.Duration
	Reconciler          SandboxReconciler
	Recovery            RunRecovery
	EffectiveConfigJSON []byte
	SolutionSandbox     *sandboxexec.Config
}

// NewSlice2RunService is the compatibility entry for persisted preview and
// checkpoint workflows. Current production composition uses NewGenerationRunService.
func NewSlice2RunService(config Slice2RunServiceConfig) (*LocalRunService, error) {
	if config.Generation == nil {
		return nil, errors.New("legacy generation executor is required")
	}
	g := config.Generation.config
	return newGenerationRunService(GenerationRunConfig{
		Store: g.Store, Blobs: g.Blobs, Clock: g.Clock, Locks: g.Locks,
		Generation: config.Generation, Similarity: config.Similarity, Reviews: config.Reviews,
		ActiveTimeInterval: config.ActiveTimeInterval, ControlPollInterval: config.ControlPollInterval,
		Reconciler: config.Reconciler, Recovery: config.Recovery, EffectiveConfigJSON: config.EffectiveConfigJSON, SolutionSandbox: config.SolutionSandbox,
	})
}
