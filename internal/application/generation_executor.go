package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/workflow"
)

type GenerationExecutionStore interface {
	RunLLMStore
	port.GenerationStore
}

type GenerationExecutorConfig struct {
	Store                       GenerationExecutionStore
	Blobs                       *blob.Store
	LLM                         port.PhysicalLLM
	Clock                       clock.Clock
	Locks                       *runlock.Manager
	Content                     GenerationReaderOptions
	IdeaRepair, StatementRepair FormatRepairPolicy
	SolutionRepair              FormatRepairPolicy
	DataRepair                  FormatRepairPolicy
	RetryPolicy                 domain.RetryPolicy
	CostUpperBoundMicroUSD      int64
}

// GenerationExecutor composes only the two typed content stages. Its caller
// owns the foreground run/artifact locks, active-time interval and atomic
// stage commit. Construction and Reader never dispatch a provider request.
type GenerationExecutor struct {
	config GenerationExecutorConfig
	reader *GenerationReader
}

// GenerationStageResult keeps application-owned receipts beside the typed
// business outcome. Only a successful stage may attach its occurrences.
// Call usage has already been settled by the durable ledger.
type GenerationStageResult[T any] struct {
	Outcome     domain.AgentResult[T]
	Occurrences []domain.PendingOccurrence
	CallTraces  []domain.CallTrace
	Usage       port.Usage
	publication *generationCachePublication
}

type generationCachePublication struct {
	cache   *StructuredLLMCache
	open    domain.OpenCallRequest
	request port.GenerateRequest
}

// PublishCache is optional indexing after the authoritative stage commit.
// The cache independently verifies committed producer occurrences and bytes;
// invoking this before commit fails without publishing an entry or dispatch.
func (r GenerationStageResult[T]) PublishCache(ctx context.Context) error {
	if r.publication == nil {
		return nil
	}
	_, err := r.publication.cache.Put(ctx, r.publication.open, r.publication.request)
	return err
}

func NewGenerationExecutor(config GenerationExecutorConfig) (*GenerationExecutor, error) {
	if config.Store == nil || config.Blobs == nil || config.LLM == nil || config.Clock == nil || config.Locks == nil || config.CostUpperBoundMicroUSD <= 0 {
		return nil, errors.New("generation executor requires durable storage, provider, locks, clock and a positive cost ceiling")
	}
	if err := config.RetryPolicy.Validate(); err != nil {
		return nil, err
	}
	if config.RetryPolicy.MaxAttempts > 8 {
		return nil, errors.New("generation transport retry bound exceeds eight attempts")
	}
	service := &GenerationExecutor{config: config}
	idea, err := service.calls(config.Store, "idea")
	if err != nil {
		return nil, err
	}
	statement, err := service.calls(config.Store, "statement")
	if err != nil {
		return nil, err
	}
	service.reader, err = NewGenerationReader(config.Store, idea, statement, config.Content)
	if err != nil {
		return nil, err
	}
	return service, nil
}

func (s *GenerationExecutor) Reader() *GenerationReader { return s.reader }

func (s *GenerationExecutor) calls(ledger LLMArtifactLedger, stage domain.StageName) (*StructuredLLMCalls, error) {
	calls, err := NewReplayableLLMCalls(ledger, s.config.LLM, s.config.Blobs, s.config.Clock, s.config.CostUpperBoundMicroUSD)
	if err != nil {
		return nil, err
	}
	policy := s.config.IdeaRepair
	if stage == "statement" {
		policy = s.config.StatementRepair
	}
	if stage == "solution" {
		policy = s.config.SolutionRepair
	}
	if stage == "data" {
		policy = s.config.DataRepair
	}
	return NewStructuredLLMCalls(calls, policy)
}

func (s *GenerationExecutor) RunIdea(ctx context.Context, view domain.RunView, input domain.GenerationRequestSnapshotV1) (GenerationStageResult[domain.IdeaBatch], error) {
	result, err := s.CollectIdeaCandidates(ctx, view, input)
	if err != nil || result.Outcome.Value == nil {
		return result, err
	}
	batch := *result.Outcome.Value
	feasible, err := batch.OrderedFeasibleCandidateIDs(s.config.Content.SelectionPolicy)
	if err != nil {
		return result, err
	}
	if len(feasible) == 0 {
		result.Outcome = domain.Review[domain.IdeaBatch](domain.ReviewRequest{EvidenceDigest: batch.BatchDigest, PolicyDigest: s.config.Content.ProviderPolicyDigest, Reason: "no_feasible_idea_candidates"})
		result.Occurrences, result.publication = nil, nil
	}
	return result, nil
}

// CollectIdeaCandidates returns a verified initial batch and its private
// receipts independently of selection. A compatible composing workflow may
// commit this proof before deciding whether any candidate is feasible. The
// existing preview continues to invoke RunIdea and retains its old semantics.
func (s *GenerationExecutor) CollectIdeaCandidates(ctx context.Context, view domain.RunView, input domain.GenerationRequestSnapshotV1) (GenerationStageResult[domain.IdeaBatch], error) {
	var result GenerationStageResult[domain.IdeaBatch]
	if err := input.Validate(); err != nil {
		return result, err
	}
	attempt, err := s.admit(ctx, view, "idea", input.SnapshotDigest)
	if err != nil {
		return result, err
	}
	persisted, err := s.config.Store.ReadGenerationSnapshot(ctx, view.RunID())
	if err != nil {
		return result, err
	}
	if input.SnapshotDigest != persisted.SnapshotDigest {
		return result, errors.New("Idea input differs from the persisted request snapshot")
	}
	draftInput, err := domain.NewIdeaDraftInput(persisted, s.config.Content.IdeaCount)
	if err != nil {
		return result, err
	}
	variables, err := draftInput.CanonicalJSON()
	if err != nil {
		return result, err
	}
	generated, err := s.generate(ctx, view, attempt, variables)
	result = generationResult[domain.IdeaBatch](generated)
	if err != nil {
		return result, err
	}
	if generated.outcome.Failure != nil {
		result.Outcome = generationFailure[domain.IdeaBatch](view, attempt, s.config.Content.ProviderPolicyDigest, generated.outcome, s.config.Clock.Now())
		return result, nil
	}
	var draft domain.IdeaDraftV1
	if err := json.Unmarshal(generated.outcome.Value.Structured, &draft); err != nil {
		return result, err
	}
	batch, err := draft.Bind(draftInput)
	if err != nil {
		result.Outcome = generationContentReview[domain.IdeaBatch](s.config.Content.ProviderPolicyDigest, generated.outcome.CallTrace, "idea_binding_rejected")
		return result, nil
	}
	result.Outcome = domain.Success(batch)
	result.Occurrences, result.publication = generated.occurrences, generated.publication
	return result, nil
}

func (s *GenerationExecutor) RunStatement(ctx context.Context, view domain.RunView, input domain.StatementInput) (GenerationStageResult[domain.ProblemSpec], error) {
	var result GenerationStageResult[domain.ProblemSpec]
	inputDigest, err := input.Digest()
	if err != nil {
		return result, err
	}
	attempt, err := s.admit(ctx, view, "statement", inputDigest)
	if err != nil {
		return result, err
	}
	idea, err := s.reader.ReadIdea(ctx, view.RunID())
	if err != nil {
		return result, err
	}
	if input != idea.StatementInput {
		return result, errors.New("Statement input differs from the verified committed Idea chain")
	}
	draftInput, err := domain.NewStatementDraftInput(input, idea.Snapshot, idea.Batch, idea.Selection)
	if err != nil {
		return result, err
	}
	variables, err := draftInput.CanonicalJSON()
	if err != nil {
		return result, err
	}
	generated, err := s.generate(ctx, view, attempt, variables)
	result = generationResult[domain.ProblemSpec](generated)
	if err != nil {
		return result, err
	}
	if generated.outcome.Failure != nil {
		result.Outcome = generationFailure[domain.ProblemSpec](view, attempt, s.config.Content.ProviderPolicyDigest, generated.outcome, s.config.Clock.Now())
		return result, nil
	}
	var draft domain.StatementDraftV1
	if err := json.Unmarshal(generated.outcome.Value.Structured, &draft); err != nil {
		return result, err
	}
	problem, err := draft.Bind(draftInput, s.config.Content.StatementRevision)
	if err != nil {
		result.Outcome = generationContentReview[domain.ProblemSpec](s.config.Content.ProviderPolicyDigest, generated.outcome.CallTrace, "statement_binding_rejected")
		return result, nil
	}
	result.Outcome = domain.Success(problem)
	result.Occurrences, result.publication = generated.occurrences, generated.publication
	return result, nil
}

func (s *GenerationExecutor) admit(ctx context.Context, view domain.RunView, stage domain.StageName, input domain.Digest) (domain.StageAttempt, error) {
	var empty domain.StageAttempt
	if ctx == nil {
		return empty, errors.New("generation execution requires a context")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	compatible := view.WorkflowRevision() == workflow.Slice2WorkflowRevision || view.WorkflowRevision() == workflow.Slice2CheckpointWorkflowRevision || workflow.HasSolutionStages(view.WorkflowRevision())
	if !compatible || view.SchemaVersion() != domain.RequestSchemaV1 || view.State() != domain.RunRunning || view.CurrentStage() != stage || view.AttemptID().Validate() != nil {
		return empty, errors.New("generation requires the exact current supported stage view")
	}
	current, err := s.config.Store.GetRun(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	if current.State != domain.RunRunning || current.CurrentStage != stage || current.WorkflowRevision != view.WorkflowRevision() || current.SchemaVersion != view.SchemaVersion() || current.RequestDigest != view.RequestDigest() || current.ConfigDigest != view.ConfigDigest() || current.WorkflowDigest != view.WorkflowDigest() || current.WorkflowDigest != domain.SumBytes([]byte(view.WorkflowRevision())) || current.Version < view.Version() || current.ActiveStartedAt == nil || current.ConfigDigest != s.config.Content.ProviderPolicyDigest {
		return empty, errors.New("generation view or frozen provider configuration differs from the active run")
	}
	snapshot, err := s.config.Store.ReadGenerationSnapshot(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	if snapshot.Request.BudgetLimits != view.Budget().Limits {
		return empty, errors.New("generation view budget differs from the admitted request")
	}
	attempt, err := s.config.Store.CurrentStageAttempt(ctx, view.RunID(), stage)
	if err != nil {
		return empty, err
	}
	if attempt.AttemptID != view.AttemptID() || attempt.State != domain.StageAttemptRunning || attempt.InputDigest != input {
		return empty, errors.New("generation input or attempt differs from the current stage")
	}
	return attempt, nil
}

type generatedDraft struct {
	outcome     domain.MeteredOutcome[port.GenerateResponse]
	occurrences []domain.PendingOccurrence
	traces      []domain.CallTrace
	usage       port.Usage
	publication *generationCachePublication
}

func generationResult[T any](draft generatedDraft) GenerationStageResult[T] {
	return GenerationStageResult[T]{CallTraces: draft.traces, Usage: draft.usage}
}

func (s *GenerationExecutor) generate(ctx context.Context, view domain.RunView, attempt domain.StageAttempt, variables []byte) (generatedDraft, error) {
	var result generatedDraft
	ledger, err := NewRunBoundLLMLedger(s.config.Store, view.RunID(), attempt.StageName, attempt.AttemptID)
	if err != nil {
		return result, err
	}
	service, err := s.calls(ledger, attempt.StageName)
	if err != nil {
		return result, err
	}
	cache, err := NewStructuredLLMCache(service, s.config.Store, s.config.Locks)
	if err != nil {
		return result, err
	}
	open, request, err := s.draftCall(view.RunID(), view.Version(), attempt, variables)
	if err != nil {
		return result, err
	}
	existing, readErr := s.config.Store.ReadLogicalCall(ctx, open.ID)
	if readErr != nil && !errors.Is(readErr, sqlite.ErrNotFound) {
		return result, readErr
	}
	checkpoint, err := s.config.Store.ReadAttemptDependencyCheckpoint(ctx, view.RunID(), attempt.StageName, attempt.AttemptID)
	if err != nil {
		return result, err
	}
	if checkpoint != nil && checkpoint.PolicyDigest != s.config.Content.ProviderPolicyDigest {
		return result, errors.New("blocked generation dependency policy differs from frozen execution")
	}
	if checkpoint != nil && existing.Kind == domain.CallCacheReuse {
		return result, errors.New("blocked dependency recheck cannot be satisfied by a cached result")
	}
	if checkpoint == nil && (errors.Is(readErr, sqlite.ErrNotFound) || existing.Kind == domain.CallCacheReuse) {
		hit, err := cache.Reuse(ctx, open, request)
		if err != nil {
			return result, err
		}
		if hit.Hit {
			result.outcome, result.traces = hit.Outcome, []domain.CallTrace{hit.Outcome.CallTrace}
			for i := range hit.Reuses {
				reuse := hit.Reuses[i]
				result.occurrences = append(result.occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceCacheReuse, CacheReuse: &reuse})
			}
			return result, hit.Outcome.Validate()
		}
		if readErr == nil {
			return result, ErrLLMReplayUnavailable
		}
	}
	generated, err := service.Generate(ctx, open, request)
	result.outcome, result.traces, result.usage = generated.Outcome, generated.CallTraces, generated.Usage
	if err != nil {
		return result, err
	}
	if err := generated.Outcome.Validate(); err != nil {
		return result, err
	}
	if generated.Outcome.Value != nil {
		for i := range generated.Artifacts {
			artifact := generated.Artifacts[i]
			result.occurrences = append(result.occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &artifact})
		}
		result.publication = &generationCachePublication{cache: cache, open: open, request: request}
	}
	return result, nil
}

func generationContentReview[T any](policy domain.Digest, trace domain.CallTrace, reason string) domain.AgentResult[T] {
	evidence, _ := canonicalJSON(struct {
		Reason string           `json:"reason"`
		Trace  domain.CallTrace `json:"trace"`
	}{reason, trace})
	return domain.Review[T](domain.ReviewRequest{EvidenceDigest: domain.SumBytes(evidence), PolicyDigest: policy, Reason: reason})
}

func generationFailure[T any](view domain.RunView, attempt domain.StageAttempt, policy domain.Digest, outcome domain.MeteredOutcome[port.GenerateResponse], now time.Time) domain.AgentResult[T] {
	failure := outcome.Failure
	reason := "llm_content_rejected"
	switch {
	case failure.Code == domain.FailureBudgetExhausted:
		reason = "llm_budget_exhausted"
	case failure.Class == domain.FailureUnknown || failure.Code == domain.FailureBoundaryUnknown:
		reason = "llm_boundary_unknown"
	case failure.Class == domain.FailureRetryable || failure.Class == domain.FailureBlocked || failure.Class == domain.FailureIncompatible:
		evidence, _ := canonicalJSON(struct {
			Code  domain.PortFailureCode `json:"code"`
			Class domain.FailureClass    `json:"class"`
			Trace domain.CallTrace       `json:"trace"`
		}{failure.Code, failure.Class, outcome.CallTrace})
		dependency := fmt.Sprintf("llm:%s", attempt.StageName)
		retryAfter := now.UTC()
		if failure.RetryAfter != nil && failure.RetryAfter.After(retryAfter) {
			retryAfter = failure.RetryAfter.UTC()
		}
		return domain.Blocked[T](domain.BlockedCheckpoint{RunID: view.RunID(), StageName: attempt.StageName, StageInputDigest: attempt.InputDigest, DependencyID: dependency, DependencyDigest: domain.SumBytes([]byte(dependency + "\x00" + string(policy))), PolicyDigest: policy, ErrorDigest: domain.SumBytes(evidence), RetryAfter: retryAfter, CreatedAt: now.UTC()})
	}
	return generationContentReview[T](policy, outcome.CallTrace, reason)
}
