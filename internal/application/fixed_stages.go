package application

import (
	"context"
	"errors"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

// fixedStages owns typed business dispatch. It neither controls execution
// lifetime nor closes resources; sandbox creation is an injected capability.
type fixedStages struct {
	pipeline      fake.Pipeline
	generation    *GenerationExecutor
	similarity    *SimilarityExecutor
	solution      *SolutionExecutor
	data          *DataExecutor
	quality       *QualityExecutor
	packages      *PackageExecutor
	store         GenerationExecutionStore
	blobs         *blob.Store
	clock         clock.Clock
	admission     *StageAdmission
	content       GenerationReaderOptions
	sandboxPolicy SandboxReadPolicy
	sandbox       SolutionSandboxFactory
}

func (s *fixedStages) readGenerationInput(ctx context.Context, snapshot domain.RunSnapshot) (any, error) {
	switch snapshot.CurrentStage {
	case "idea":
		return s.store.ReadGenerationSnapshot(ctx, snapshot.RunID)
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
		expected, err := s.store.ReadStageInputDigest(ctx, snapshot.RunID, snapshot.CurrentStage)
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

func (s *fixedStages) runStage(ctx context.Context, view domain.RunView, input any) (stageExecution, error) {
	if !workflow.SupportsGeneration(view.WorkflowRevision()) {
		result, err := s.runTyped(ctx, view, input)
		return stageExecution{result: result}, err
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
		if view.CurrentStage() == "similarity_decision" {
			result, err := s.solution.reader.ReadInput(ctx, view.RunID())
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
		result, err := s.solution.VerifyDraft(ctx, view, s.sandbox)
		return stageExecution{result: domain.Success[any](result.Report), occurrences: result.Occurrences}, err
	case SolutionVerificationReport:
		if view.CurrentStage() == "solution_decision" {
			result, err := s.data.reader.ReadInput(ctx, view.RunID())
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
		result, err := s.data.VerifyDraft(ctx, view, s.sandbox)
		return stageExecution{result: domain.Success[any](result.Report), occurrences: result.Occurrences}, err
	case DataVerificationReport:
		if typed.Passed {
			result, err := s.data.RunJudge(ctx, view, s.sandbox)
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
			result, err := s.quality.Run(ctx, view, s.sandbox)
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

func (s *fixedStages) bindCommit(ctx context.Context, snapshot domain.RunSnapshot, value any, command *domain.FinishStageCommand) error {
	var output, next domain.Digest
	switch typed := value.(type) {
	case domain.IdeaBatch:
		if snapshot.CurrentStage != "idea" {
			return errors.New("Idea output stage differs")
		}
		request, err := s.store.ReadGenerationSnapshot(ctx, snapshot.RunID)
		if err != nil {
			return err
		}
		ids, err := typed.OrderedFeasibleCandidateIDs(s.content.SelectionPolicy)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return errors.New("successful Idea has no feasible selection")
		}
		selection, err := domain.NewIdeaSelection(request.RequestDigest, typed, ids[0], s.content.SelectionPolicy, []string{"deterministic_selection"}, []domain.Digest{typed.BatchDigest})
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

func (s *fixedStages) runTyped(ctx context.Context, view domain.RunView, input any) (domain.AgentResult[any], error) {
	switch typed := input.(type) {
	case domain.FakeInput:
		result, err := s.pipeline.Prepare().Run(ctx, view, typed)
		return convertResult(result), err
	case domain.FakePrepared:
		result, err := s.pipeline.Exercise().Run(ctx, view, typed)
		return convertResult(result), err
	case domain.FakeEvidence:
		result, err := s.pipeline.Checkpoint().Run(ctx, view, typed)
		return convertResult(result), err
	default:
		return domain.AgentResult[any]{}, errors.New("unsupported typed stage input")
	}
}

func (s *fixedStages) readSolutionStageInput(ctx context.Context, snapshot domain.RunSnapshot) (any, error) {
	if s.solution == nil || s.sandbox == nil {
		return nil, errors.New("Solution stage has no compatible executor")
	}
	var input any
	switch snapshot.CurrentStage {
	case "similarity_decision":
		content, err := s.similarity.ReadCommitted(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = content
	case "solution":
		accepted, err := s.solution.reader.ReadInput(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		if accepted.Value == nil {
			return nil, errors.New("Solution stage has no current acceptance")
		}
		input = *accepted.Value
	case "solution_verify":
		content, err := s.solution.reader.ReadDraft(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = content
	case "solution_checkpoint", "solution_decision":
		report, err := s.solution.reader.ReadVerification(ctx, snapshot.RunID, s.sandboxPolicy)
		if err != nil {
			return nil, err
		}
		input = report
	default:
		return nil, errors.New("unsupported Solution stage")
	}
	expected, err := s.store.ReadStageInputDigest(ctx, snapshot.RunID, snapshot.CurrentStage)
	if err != nil {
		return nil, err
	}
	digest, err := stageInputDigest(input)
	if err != nil {
		return nil, err
	}
	if digest != expected {
		return nil, errors.New("Solution stage input differs from its committed predecessor")
	}
	return input, nil
}

func (s *fixedStages) readDataStageInput(ctx context.Context, snapshot domain.RunSnapshot) (any, error) {
	if s.data == nil {
		return nil, errors.New("Data stage has no compatible executor")
	}
	var input any
	switch snapshot.CurrentStage {
	case "data":
		accepted, err := s.data.reader.ReadInput(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		if accepted.Value == nil {
			return nil, errors.New("Data stage has no current passing Solution")
		}
		input = *accepted.Value
	case "data_verify":
		content, err := s.data.reader.ReadDraft(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = content
	case "judge":
		report, err := s.data.reader.ReadVerification(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = report
	case "quality":
		report, err := s.data.reader.ReadJudgeVerification(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = report
	case "package":
		report, err := s.quality.reader.ReadReport(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = report
	default:
		return nil, errors.New("unsupported Data stage")
	}
	expected, err := s.store.ReadStageInputDigest(ctx, snapshot.RunID, snapshot.CurrentStage)
	if err != nil {
		return nil, err
	}
	digest, err := stageInputDigest(input)
	if err != nil {
		return nil, err
	}
	if digest != expected {
		return nil, errors.New("Data stage input differs from its committed predecessor")
	}
	return input, nil
}

func (s *fixedStages) revalidate(ctx context.Context, view domain.RunView, stage domain.StageName, binding domain.BlockedCheckpoint) (bool, error) {
	return s.pipeline.Revalidate(ctx, view, stage, binding)
}

// Reconcile only effects of the current compiled stage; no new work is admitted.
func (s *fixedStages) reconcileStage(ctx context.Context, snapshot domain.RunSnapshot, reconciler SandboxReconciler) error {
	if snapshot.State != domain.RunRunning {
		return nil
	}
	runID := snapshot.RunID
	switch snapshot.CurrentStage {
	case "idea", "statement":
		return s.generation.ReconcileStage(ctx, runID)
	case "similarity":
		return s.similarity.ReconcileStage(ctx, runID)
	case "solution":
		return s.solution.ReconcileDraft(ctx, runID)
	case "data":
		return s.data.ReconcileDraft(ctx, runID)
	case "solution_verify", "data_verify", "judge", "quality", "package":
		return s.reconcileSandboxVerification(ctx, runID, snapshot.CurrentStage, reconciler)
	case "slice2_checkpoint", "similarity_decision", "solution_checkpoint", "solution_decision":
		return nil
	default:
		return errors.New("current stage has no compiled recovery adapter")
	}
}

// Terminal cleanup preserves completed evidence and conservatively settles
// uncertain creates only after the exact Docker ownership has been cleaned.
// This path never compiles, runs, starts a model, or republishes host content.
func (s *fixedStages) reconcileSandboxVerification(ctx context.Context, runID domain.RunID, stage domain.StageName, reconciler SandboxReconciler) error {
	current, attempt, err := s.admission.reconciliationAttempt(ctx, runID)
	if err != nil || attempt == nil {
		return err
	}
	if current.CurrentStage != stage || reconciler == nil {
		return errors.New("sandbox verification cleanup requires its exact stage and Docker reconciler")
	}
	report, err := reconciler.ReconcileRun(ctx, runID)
	if err != nil {
		return err
	}
	if !report.Completed || cleanupEvidenceMatchesRun(runID, report) {
		return ErrCleanupPending
	}
	store, ok := s.store.(interface {
		ReadAttemptSandboxCalls(context.Context, domain.RunID, domain.StageName, domain.AttemptID) ([]domain.CallRecord, error)
	})
	if !ok {
		return errors.New("solution cleanup requires scoped call history")
	}
	calls, err := store.ReadAttemptSandboxCalls(ctx, runID, attempt.StageName, attempt.AttemptID)
	if err != nil {
		return err
	}
	ledger, err := NewRunBoundLLMLedger(s.store, runID, attempt.StageName, attempt.AttemptID)
	if err != nil {
		return err
	}
	identity, err := stagePublicationIdentity(*attempt, current.Version)
	if err != nil {
		return err
	}
	for _, call := range calls {
		if call.State == domain.CallRecordTerminal {
			continue
		}
		if call.Provider != "blob" && call.Provider != "docker" {
			return errors.New("sandbox cleanup found an unsupported provider")
		}
		failure := &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
		if call.State == domain.CallRecordOpen {
			_, err := ledger.FinishCall(ctx, domain.FinishCallRequest{RunID: runID, ExpectedRunVersion: current.Version, StageName: attempt.StageName, AttemptID: attempt.AttemptID, CallRecordID: call.ID, DispatchKind: domain.DispatchNone, Failure: failure, IdempotencyKey: coordinatorMutationID("finish", call.ID), At: s.clock.Now().UTC()})
			if err != nil {
				return err
			}
			continue
		}
		p, err := ledger.LoadCall(ctx, call.ID)
		if err != nil {
			return err
		}
		if len(p.PhysicalCalls) != 1 {
			return errors.New("sandbox cleanup call has multiple physical identities")
		}
		physical := p.PhysicalCalls[0]
		if call.Provider == "blob" && physical.State != domain.PhysicalCompleted && physical.State != domain.PhysicalAbortedNoDispatch {
			declID := domain.ArtifactDeclarationID(coordinatorMutationID("decl", call.ID))
			decl, token, err := ledger.ReadArtifactWriter(ctx, declID)
			if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
				return err
			}
			if err == nil {
				if token.State == domain.ArtifactWriterSealed {
					session, err := NewPreparedArtifactSession(ledger, s.blobs, p)
					if err != nil {
						return err
					}
					writer, err := session.Prepare(ctx, decl.ID)
					if err != nil {
						return err
					}
					if _, err := writer.Finalize(ctx); err != nil {
						return err
					}
					token.State = domain.ArtifactWriterFinalized
				}
				if token.State == domain.ArtifactWriterFinalized {
					pending, err := ledger.ReadPendingArtifact(ctx, decl.ID)
					if err != nil {
						return err
					}
					sink := &SandboxArtifactSink{ledger: ledger, blobs: s.blobs, clock: s.clock, identity: identity}
					if err := sink.complete(ctx, decl, pending); err != nil {
						return err
					}
					continue
				}
				if token.State != domain.ArtifactWriterReleased {
					if err := ledger.ReleaseArtifact(ctx, token.ID); err != nil {
						return err
					}
				}
			}
		}
		if physical.State == domain.PhysicalDispatching || physical.State == domain.PhysicalSent {
			state, outcome := domain.PhysicalUnknown, domain.PhysicalOutcomeUnknown
			failure = &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
			if call.Provider == "blob" {
				state, outcome = domain.PhysicalAbortedNoDispatch, domain.PhysicalOutcomeNoSend
				failure = &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
			}
			if err := ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: runID, ExpectedRunVersion: current.Version, StageName: attempt.StageName, AttemptID: attempt.AttemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, State: state, Outcome: outcome, Failure: failure, IdempotencyKey: coordinatorMutationID("terminal_cleanup", physical.ID), At: s.clock.Now().UTC()}); err != nil {
				return err
			}
		}
		binding := port.SandboxResourceCall{CallRecordID: call.ID, AttemptCallID: physical.ID}
		if err := finishSandboxResourceCalls(ctx, ledger, s.clock, identity, []port.SandboxResourceCall{binding}); err != nil {
			return err
		}
	}
	return nil
}
