package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"reflect"
	"time"

	sandboxexec "cpgen/internal/adapter/sandbox"
	docker "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/toolchain"
	"cpgen/internal/workflow"
)

type GenerationRunConfig struct {
	Store               GenerationExecutionStore
	Blobs               *blob.Store
	Clock               clock.Clock
	Locks               *runlock.Manager
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

func NewGenerationRunService(config GenerationRunConfig) (*LocalRunService, error) {
	if config.Similarity == nil || !workflow.ProducesPackage(config.Similarity.Revision()) {
		return nil, errors.New("generation run service requires the package-producing revision")
	}
	return newGenerationRunService(config)
}

func (c GenerationRunConfig) validateResources() error {
	if c.Store == nil || c.Blobs == nil || c.Clock == nil || c.Locks == nil || c.Generation == nil || c.Similarity == nil {
		return errors.New("generation service requires explicit shared resources and stages")
	}
	// Admitted stages must share the exact owner's store. Validate at composition,
	// while run/attempt/policy bindings remain checked again at each effect.
	if !sameDependency(c.Store, c.Generation.StageAdmission.store) || c.Blobs != c.Generation.config.Blobs || c.Locks != c.Generation.config.Locks || !sameDependency(c.Clock, c.Generation.config.Clock) || c.Generation.StageAdmission != c.Similarity.admission {
		return errors.New("generation stages do not belong to the supplied execution resources")
	}
	if !sameDependency(c.Store, c.Similarity.store) || c.Blobs != c.Similarity.blobs || c.Locks != c.Similarity.locks || !sameDependency(c.Clock, c.Similarity.clock) {
		return errors.New("similarity stage does not belong to the supplied execution resources")
	}
	if c.Generation.StageAdmission.policy != domain.SumBytes(c.EffectiveConfigJSON) {
		return errors.New("generation policy differs from frozen configuration")
	}
	return nil
}

func sameDependency(a, b any) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	t := reflect.TypeOf(a)
	return t == reflect.TypeOf(b) && t.Comparable() && a == b
}

func newGenerationRunService(config GenerationRunConfig) (*LocalRunService, error) {
	if config.Generation == nil || config.Generation.DraftExecution == nil || config.Generation.StageAdmission == nil || config.Similarity == nil || config.Similarity.admission != config.Generation.StageAdmission || len(config.EffectiveConfigJSON) == 0 {
		return nil, errors.New("generation service requires shared admission and frozen configuration")
	}
	if err := config.validateResources(); err != nil {
		return nil, err
	}
	if config.Similarity.Revision() == workflow.RetryingGenerationRevision || config.Similarity.Revision() == workflow.ExecutedSamplesRevision {
		if _, ok := config.Store.(port.ContentRetryStore); !ok {
			return nil, errors.New("content retry workflow requires atomic retry storage")
		}
	}
	service, err := newRunService(RunServiceConfig{
		Runtime: config.Store, Reviews: config.Reviews, Locks: config.Locks, Clock: config.Clock,
		ActiveTimeInterval: config.ActiveTimeInterval, ControlPollInterval: config.ControlPollInterval, Reconciler: config.Reconciler, Recovery: config.Recovery,
		EffectiveConfigJSON: config.EffectiveConfigJSON, EffectiveConfigDigest: domain.SumBytes(config.EffectiveConfigJSON),
	}, config.Similarity.Revision(), &fixedStages{generation: config.Generation, similarity: config.Similarity})
	if err != nil {
		return nil, err
	}
	service.stages.store, service.stages.content = config.Store, config.Generation.Reader().options
	service.stages.blobs, service.stages.clock, service.stages.admission = config.Blobs, config.Clock, config.Generation.StageAdmission
	publisher := newStagePublisher(config.Store, config.Blobs, config.Clock)
	if service.graph.definition.HasStage("solution") {
		if config.SolutionSandbox == nil {
			return nil, errors.New("forward Solution workflow requires its durable Docker configuration")
		}
		evidenceStore, ok := config.Store.(SandboxEvidenceReadStore)
		if !ok {
			return nil, errors.New("solution requires committed sandbox evidence storage")
		}
		solutionCalls, err := config.Generation.DraftExecution.calls(config.Store, "solution")
		if err != nil {
			return nil, err
		}
		solutionReader := &SolutionReader{similarity: config.Similarity.Reader(), generation: config.Generation.Reader(), calls: solutionCalls, store: evidenceStore, blobs: config.Blobs, revision: service.graph.revision}
		service.stages.solution = &SolutionExecutor{publisher: publisher, blobs: config.Blobs, reader: solutionReader, drafts: config.Generation.DraftExecution}

		base := *config.SolutionSandbox
		store, ok := config.Store.(sandboxexec.Store)
		if !ok {
			return nil, errors.New("forward Solution workflow requires Docker lifecycle storage")
		}
		base.Store, base.Blobs, base.Clock = store, config.Blobs, config.Clock
		if err := sandboxexec.ValidateDependencies(base); err != nil {
			return nil, err
		}
		rawLock, err := base.Lock.MarshalIndent()
		if err != nil {
			return nil, err
		}
		base.Lock, err = toolchain.LoadLock(bytes.NewReader(rawLock))
		if err != nil {
			return nil, err
		}
		service.stages.sandboxPolicy, service.stages.sandbox = base.ReadPolicy(), solutionSandboxFactory(base)
		if service.graph.definition.ProducesPackage() {
			dataCalls, err := config.Generation.DraftExecution.calls(config.Store, "data")
			if err != nil {
				return nil, err
			}
			dataReader := &DataReader{solution: solutionReader, sandbox: base.ReadPolicy(), generation: config.Generation.Reader(), calls: dataCalls, store: evidenceStore, blobs: config.Blobs}
			service.stages.data = &DataExecutor{publisher: publisher, blobs: config.Blobs, reader: dataReader, drafts: config.Generation.DraftExecution, sandbox: base.ReadPolicy()}

			packageStore, ok := config.Store.(PackageEvidenceReadStore)
			if !ok {
				return nil, errors.New("MVP requires committed package storage")
			}
			qualityReader := &QualityReader{data: dataReader, store: evidenceStore, blobs: config.Blobs, sandbox: base.ReadPolicy(), similarity: config.Similarity.Reader()}
			service.stages.quality = &QualityExecutor{reader: qualityReader, blobs: config.Blobs, admission: config.Generation.StageAdmission, publisher: publisher}
			packageReader := &PackageReader{quality: qualityReader, similarity: config.Similarity.Reader(), store: packageStore, blobs: config.Blobs}
			service.stages.packages = &PackageExecutor{reader: packageReader, admission: config.Generation.StageAdmission, publisher: publisher}
		}
		if service.reconciler == nil {
			reconcileStore, ok := config.Store.(docker.SandboxReconcileStore)
			if !ok {
				return nil, errors.New("Solution workflow requires exact Docker cleanup storage")
			}
			service.reconciler, err = docker.NewSandboxReconciler(docker.SandboxReconcilerOptions{Engine: base.Engine, Store: reconcileStore, EngineIdentityDigest: base.EngineIdentity, CleanupTimeout: base.Limits.CleanupTimeout})
			if err != nil {
				return nil, err
			}
		}
	} else if config.SolutionSandbox != nil {
		return nil, errors.New("Solution sandbox cannot change a historical preview revision")
	}
	if service.stages.packages != nil {
		service.archive = service.stages.packages.reader
	}

	return service, nil
}

func (s *LocalRunService) generateGenerationRun(ctx context.Context, request domain.RunRequest) (domain.RunSnapshot, error) {
	var empty domain.RunSnapshot
	if ctx == nil {
		return empty, errors.New("generation requires a context")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	submitted, err := domain.GenerationRequestFromRunRequest(request)
	if err != nil {
		return empty, err
	}
	if err := submitted.Validate(); err != nil {
		return empty, err
	}
	seed := int64(0)
	if submitted.Seed != nil {
		seed = *submitted.Seed
	} else {
		var raw [8]byte
		if _, err := rand.Read(raw[:]); err != nil {
			return empty, err
		}
		seed = int64(binary.LittleEndian.Uint64(raw[:]))
	}
	input, err := domain.NewGenerationRequestSnapshotV1(submitted, seed)
	if err != nil {
		return empty, err
	}
	raw, err := submitted.CanonicalJSON()
	if err != nil {
		return empty, err
	}
	id, err := domain.NewID("run")
	if err != nil {
		return empty, err
	}
	runID := domain.RunID(id)
	create := domain.CreateRunRequest{
		RunID: runID, SubmittedRequestJSON: raw, SubmittedRequestDigest: input.RequestDigest, EffectiveSeed: seed,
		RedactedEffectiveConfigJSON: append([]byte(nil), s.effectiveConfigJSON...), RedactedEffectiveConfigDigest: s.effectiveConfigDigest,
		WorkflowRevision: s.graph.revision, WorkflowDigest: domain.SumBytes([]byte(s.graph.revision)),
		SchemaVersion: domain.RequestSchemaV1, BudgetLimits: submitted.BudgetLimits,
		StageSequence: append([]domain.StageName(nil), s.graph.stages...), CreatedAt: s.clock.Now().UTC(), IdempotencyKey: stableServiceID("create", runID, 1),
	}
	if err := create.Validate(); err != nil {
		return empty, err
	}
	return s.execute(ctx, runID, &create, domain.FakeInput{})
}

type stageExecution struct {
	result       domain.AgentResult[any]
	occurrences  []domain.PendingOccurrence
	publishCache func(context.Context) error
}

func solutionSandboxFactory(base sandboxexec.Config) SolutionSandboxFactory {
	return func(_ context.Context, identity port.SandboxAuthorizationIdentity) (SolutionSandbox, toolchain.Lock, error) {
		config := base
		config.Identity = identity
		sandbox, err := sandboxexec.NewSession(config)
		return sandbox, config.Lock, err
	}
}
