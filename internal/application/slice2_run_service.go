package application

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"time"

	docker "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
	"cpgen/internal/toolchain"
	"cpgen/internal/workflow"
)

// Slice2RunServiceConfig deliberately derives the runtime, blobs, provider
// clock and lock ownership from one generation executor. Similarity must share
// that exact executor; unrelated stores cannot be composed accidentally.
type Slice2RunServiceConfig struct {
	Generation          *GenerationExecutor
	Similarity          *SimilarityExecutor
	Reviews             port.ReviewStore
	ActiveTimeInterval  time.Duration
	Reconciler          SandboxReconciler
	Recovery            RunRecovery
	EffectiveConfigJSON []byte
	SolutionSandbox     *DockerSandboxConfig
}

func NewSlice2RunService(config Slice2RunServiceConfig) (*LocalRunService, error) {
	if config.Generation == nil || config.Similarity == nil || config.Similarity.config.Generation != config.Generation || len(config.EffectiveConfigJSON) == 0 {
		return nil, errors.New("Slice 2 run service requires one shared executor and frozen configuration")
	}
	generation := config.Generation.config
	service, err := newRunService(RunServiceConfig{
		Runtime: generation.Store, Reviews: config.Reviews, Locks: generation.Locks, Clock: generation.Clock,
		ActiveTimeInterval: config.ActiveTimeInterval, Reconciler: config.Reconciler, Recovery: config.Recovery,
		EffectiveConfigJSON: config.EffectiveConfigJSON, EffectiveConfigDigest: generation.Content.ProviderPolicyDigest,
	}, config.Generation, config.Similarity)
	if err != nil {
		return nil, err
	}
	if workflow.HasSolutionStages(service.graph.revision) {
		if config.SolutionSandbox == nil {
			return nil, errors.New("forward Solution workflow requires its durable Docker configuration")
		}
		service.solution, err = NewSolutionExecutor(config.Similarity)
		if err != nil {
			return nil, err
		}
		base := *config.SolutionSandbox
		store, ok := generation.Store.(DockerSandboxStore)
		if !ok {
			return nil, errors.New("forward Solution workflow requires Docker lifecycle storage")
		}
		base.Store, base.Blobs, base.Clock = store, generation.Blobs, generation.Clock
		if err := validateDockerSandboxDependencies(base); err != nil {
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
		service.solutionSandbox = &base
		if service.graph.revision == workflow.MVPWorkflowRevision {
			service.data, err = NewDataExecutor(service.solution, base)
			if err != nil {
				return nil, err
			}
			service.quality, err = NewQualityExecutor(service.data)
			if err != nil {
				return nil, err
			}
			service.packages, err = NewPackageExecutor(service.quality)
			if err != nil {
				return nil, err
			}
		}
		if service.reconciler == nil {
			reconcileStore, ok := generation.Store.(docker.SandboxReconcileStore)
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
	return service, nil
}

func (s *LocalRunService) generateSlice2(ctx context.Context, request domain.RunRequest) (domain.RunSnapshot, error) {
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
	return s.execute(ctx, runID, &create, domain.Slice1Input{})
}

func (s *LocalRunService) readSlice2Input(ctx context.Context, snapshot domain.RunSnapshot) (any, error) {
	switch snapshot.CurrentStage {
	case "idea":
		return s.generation.config.Store.ReadGenerationSnapshot(ctx, snapshot.RunID)
	case "statement":
		idea, err := s.generation.Reader().ReadIdea(ctx, snapshot.RunID)
		return idea.StatementInput, err
	case "similarity":
		return s.similarity.ReadInput(ctx, snapshot.RunID)
	case "slice2_checkpoint":
		content, err := s.similarity.ReadCommitted(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		expected, err := s.generation.config.Store.ReadStageInputDigest(ctx, snapshot.RunID, snapshot.CurrentStage)
		if err != nil {
			return nil, err
		}
		if expected != content.Evidence.EvidenceDigest {
			return nil, errors.New("checkpoint input differs from committed similarity evidence")
		}
		return content, nil
	case "similarity_decision", "solution", "solution_verify", "solution_checkpoint", "solution_decision":
		return s.readSolutionStageInput(ctx, snapshot)
	case "data", "data_verify", "judge", "quality", "package":
		return s.readDataStageInput(ctx, snapshot)
	default:
		return nil, errors.New("unsupported compiled Slice 2 input")
	}
}

func (s *LocalRunService) beginOrResumeStage(ctx context.Context, snapshot domain.RunSnapshot, input domain.Digest) (domain.StageAttempt, error) {
	if s.generation != nil && snapshot.State == domain.RunRunning {
		attempt, err := s.generation.config.Store.CurrentStageAttempt(ctx, snapshot.RunID, snapshot.CurrentStage)
		if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
			return attempt, err
		}
		if err == nil && attempt.State == domain.StageAttemptRunning {
			if attempt.InputDigest != input {
				return domain.StageAttempt{}, errors.New("interrupted attempt input differs from verified content")
			}
			return attempt, nil
		}
	}
	id, err := domain.NewID("attempt")
	if err != nil {
		return domain.StageAttempt{}, err
	}
	return s.runtime.BeginStage(ctx, domain.BeginStageCommand{RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, StageName: snapshot.CurrentStage, AttemptID: domain.AttemptID(id), InputDigest: input, IdempotencyKey: stableServiceID("begin", snapshot.RunID, snapshot.Version), At: s.clock.Now().UTC()})
}

type stageExecution struct {
	result       domain.AgentResult[any]
	occurrences  []domain.PendingOccurrence
	publishCache func(context.Context) error
}

func (s *LocalRunService) runStage(ctx context.Context, snapshot domain.RunSnapshot, input any) (stageExecution, error) {
	if s.generation == nil {
		result, err := s.runTyped(ctx, snapshot, input)
		return stageExecution{result: result}, err
	}
	view, err := s.view(ctx, snapshot, s.currentAttempt(snapshot.RunID))
	if err != nil {
		return stageExecution{}, err
	}
	switch typed := input.(type) {
	case domain.GenerationRequestSnapshotV1:
		result, err := s.generation.RunIdea(ctx, view, typed)
		return stageExecution{convertResult(result.Outcome), result.Occurrences, result.PublishCache}, err
	case domain.StatementInput:
		result, err := s.generation.RunStatement(ctx, view, typed)
		return stageExecution{convertResult(result.Outcome), result.Occurrences, result.PublishCache}, err
	case domain.SimilarityInputV1:
		result, err := s.similarity.RunSimilarity(ctx, view, typed)
		return stageExecution{result: convertResult(result.Outcome), occurrences: result.Occurrences}, err
	case SimilarityContent:
		if snapshot.CurrentStage == "similarity_decision" {
			result, err := s.solution.ReadInput(ctx, snapshot.RunID)
			return stageExecution{result: convertResult(result)}, err
		}
		// This revision ends at a non-waivable preview boundary. Retain the
		// evaluated decision and its policy without claiming later quality gates
		// or translating a collected evidence result into READY.
		result := domain.Review[any](domain.ReviewRequest{EvidenceDigest: typed.Evidence.EvidenceDigest, PolicyDigest: typed.Input.ExecutionPolicyDigest, Reason: "slice2_unfinished_checkpoint:" + string(typed.Decision.Kind)})
		return stageExecution{result: result}, result.Validate()
	case domain.SolutionDraftInputV1:
		result, err := s.solution.CollectDraft(ctx, view, typed)
		return stageExecution{result: convertResult(result.Outcome), occurrences: result.Occurrences, publishCache: result.PublishCache}, err
	case domain.SolutionContent:
		result, err := s.solution.VerifyDraft(ctx, view, s.newSolutionSandbox)
		return stageExecution{result: domain.Success[any](result.Report), occurrences: result.Occurrences}, err
	case SolutionVerificationReport:
		if snapshot.CurrentStage == "solution_decision" {
			result, err := s.data.ReadInput(ctx, snapshot.RunID)
			return stageExecution{result: convertResult(result)}, err
		}
		digest, err := stableValueDigest(typed)
		if err != nil {
			return stageExecution{}, err
		}
		reason := "solution_requires_review:" + typed.Reason
		if typed.Passed {
			reason = "solution_verified_checkpoint:data_not_implemented"
		}
		result := domain.Review[any](domain.ReviewRequest{EvidenceDigest: digest, PolicyDigest: typed.PolicyDigest, Reason: reason})
		return stageExecution{result: result}, result.Validate()
	case domain.DataDraftInputV1:
		result, err := s.data.CollectDraft(ctx, view, typed)
		return stageExecution{result: convertResult(result.Outcome), occurrences: result.Occurrences, publishCache: result.PublishCache}, err
	case domain.DataContent:
		result, err := s.data.VerifyDraft(ctx, view, s.newSolutionSandbox)
		return stageExecution{result: domain.Success[any](result.Report), occurrences: result.Occurrences}, err
	case DataVerificationReport:
		if typed.Passed {
			result, err := s.data.RunJudge(ctx, view, s.newSolutionSandbox)
			return stageExecution{result: domain.Success[any](result.Report), occurrences: result.Occurrences}, err
		}
		digest, err := stableValueDigest(typed)
		if err != nil {
			return stageExecution{}, err
		}
		reason := "data_requires_review:" + typed.Reason
		result := domain.Review[any](domain.ReviewRequest{EvidenceDigest: digest, PolicyDigest: typed.PolicyDigest, Reason: reason})
		return stageExecution{result: result}, result.Validate()
	case JudgeVerificationReport:
		if typed.Passed {
			result, err := s.quality.Run(ctx, view, s.newSolutionSandbox)
			return stageExecution{result: domain.Success[any](result.Report), occurrences: result.Occurrences}, err
		}
		digest, err := stableValueDigest(typed)
		if err != nil {
			return stageExecution{}, err
		}
		reason := "judge_requires_review:" + typed.Reason
		result := domain.Review[any](domain.ReviewRequest{EvidenceDigest: digest, PolicyDigest: typed.PolicyDigest, Reason: reason})
		return stageExecution{result: result}, result.Validate()
	case QualityReport:
		if typed.Passed {
			result, err := s.packages.Run(ctx, view)
			return stageExecution{result: domain.Success[any](result.Binding), occurrences: result.Occurrences}, err
		}
		digest, err := stableValueDigest(typed)
		if err != nil {
			return stageExecution{}, err
		}
		reason := "quality_requires_review:" + typed.Reason
		result := domain.Review[any](domain.ReviewRequest{EvidenceDigest: digest, PolicyDigest: typed.PolicyDigest, Reason: reason})
		return stageExecution{result: result}, result.Validate()
	default:
		return stageExecution{}, errors.New("unsupported typed Slice 2 invocation")
	}
}

func (s *LocalRunService) bindSlice2Commit(ctx context.Context, snapshot domain.RunSnapshot, value any, command *domain.FinishStageCommand) error {
	var output, next domain.Digest
	switch typed := value.(type) {
	case domain.IdeaBatch:
		if snapshot.CurrentStage != "idea" {
			return errors.New("Idea output stage differs")
		}
		request, err := s.generation.config.Store.ReadGenerationSnapshot(ctx, snapshot.RunID)
		if err != nil {
			return err
		}
		ids, err := typed.OrderedFeasibleCandidateIDs(s.generation.config.Content.SelectionPolicy)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return errors.New("successful Idea has no feasible selection")
		}
		selection, err := domain.NewIdeaSelection(request.RequestDigest, typed, ids[0], s.generation.config.Content.SelectionPolicy, []string{"deterministic_selection"}, []domain.Digest{typed.BatchDigest})
		if err != nil {
			return err
		}
		input := domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: request.SnapshotDigest, IdeaBatchDigest: typed.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}
		if err := input.ValidateChain(request, typed, selection); err != nil {
			return err
		}
		next, err = input.Digest()
		if err != nil {
			return err
		}
		output = typed.BatchDigest
	case domain.ProblemSpec:
		if snapshot.CurrentStage != "statement" {
			return errors.New("Statement output stage differs")
		}
		input, err := s.similarity.InputForProblem(typed)
		if err != nil {
			return err
		}
		next, err = input.Digest()
		if err != nil {
			return err
		}
		output = typed.SpecDigest
	case similarity.Evidence:
		if snapshot.CurrentStage != "similarity" {
			return errors.New("Similarity output stage differs")
		}
		if err := typed.Validate(); err != nil {
			return err
		}
		output, next = typed.EvidenceDigest, typed.EvidenceDigest
	case domain.SolutionDraftInputV1:
		if snapshot.CurrentStage != "similarity_decision" {
			return errors.New("solution acceptance output stage differs")
		}
		var err error
		output, err = typed.Digest()
		if err != nil {
			return err
		}
		next = output
	case domain.SolutionContent:
		if snapshot.CurrentStage != "solution" {
			return errors.New("solution draft output stage differs")
		}
		if err := typed.Validate(); err != nil {
			return err
		}
		output, next = typed.ContentDigest, typed.ContentDigest
	case SolutionVerificationReport:
		if snapshot.CurrentStage != "solution_verify" {
			return errors.New("solution verification output stage differs")
		}
		var err error
		output, err = stableValueDigest(typed)
		if err != nil {
			return err
		}
		next = output
	case domain.DataDraftInputV1:
		if snapshot.CurrentStage != "solution_decision" {
			return errors.New("data acceptance output stage differs")
		}
		var err error
		output, err = typed.Digest()
		if err != nil {
			return err
		}
		next = output
	case domain.DataContent:
		if snapshot.CurrentStage != "data" {
			return errors.New("data draft output stage differs")
		}
		if err := typed.Validate(); err != nil {
			return err
		}
		output, next = typed.ContentDigest, typed.ContentDigest
	case DataVerificationReport:
		if snapshot.CurrentStage != "data_verify" {
			return errors.New("data verification output stage differs")
		}
		var err error
		output, err = stableValueDigest(typed)
		if err != nil {
			return err
		}
		next = output
	case JudgeVerificationReport:
		if snapshot.CurrentStage != "judge" {
			return errors.New("Judge verification output stage differs")
		}
		var err error
		output, err = stableValueDigest(typed)
		if err != nil {
			return err
		}
		next = output
	case QualityReport:
		if snapshot.CurrentStage != "quality" {
			return errors.New("quality output stage differs")
		}
		var err error
		output, err = stableValueDigest(typed)
		if err != nil {
			return err
		}
		next = output
	default:
		return errors.New("unsupported Slice 2 stage output")
	}
	command.OutputDigest, command.NextInputDigest = &output, &next
	return nil
}

func (s *LocalRunService) reconcileSlice2(ctx context.Context, runID domain.RunID) error {
	snapshot, err := s.runtime.GetRun(ctx, runID)
	if err != nil {
		return err
	}
	if err := s.validateGraphPersistence(ctx, snapshot); err != nil {
		return err
	}
	if snapshot.State != domain.RunRunning {
		return nil
	}
	switch snapshot.CurrentStage {
	case "idea", "statement":
		return s.generation.ReconcileStage(ctx, runID)
	case "similarity":
		return s.similarity.ReconcileStage(ctx, runID)
	case "solution":
		return s.solution.ReconcileDraft(ctx, runID)
	case "solution_verify":
		return s.reconcileSolutionVerification(ctx, runID)
	case "data":
		return s.data.ReconcileDraft(ctx, runID)
	case "data_verify":
		return s.reconcileSandboxVerification(ctx, runID, "data_verify")
	case "judge":
		return s.reconcileSandboxVerification(ctx, runID, "judge")
	case "quality":
		return s.reconcileSandboxVerification(ctx, runID, "quality")
	case "package":
		return s.reconcileSandboxVerification(ctx, runID, "package")
	case "slice2_checkpoint", "similarity_decision", "solution_checkpoint", "solution_decision":
		return nil
	default:
		return errors.New("unsupported Slice 2 cleanup stage")
	}
}
