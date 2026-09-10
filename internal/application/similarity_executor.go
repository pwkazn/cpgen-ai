package application

import (
	"context"
	"errors"
	"slices"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

type SimilarityExecutorConfig struct {
	WorkflowRevision       string
	Generation             *GenerationExecutor
	Provider               similarity.PhysicalProvider
	Policy                 similarity.DecisionPolicy
	Limit                  int
	RetryPolicy            domain.RetryPolicy
	CostUpperBoundMicroUSD int64
}

// SimilarityExecutor collects and commits evidence separately from the later
// decision boundary. The foreground caller owns locks, active time and the
// atomic stage commit, exactly as for the generation executor.
type SimilarityExecutor struct {
	config SimilarityExecutorConfig
	reader *SimilarityReader
}

type SimilarityStageResult struct {
	Outcome     domain.AgentResult[similarity.Evidence]
	Decision    *similarity.Decision
	Occurrences []domain.PendingOccurrence
	CallTrace   domain.CallTrace
}

type SimilarityContent struct {
	Statement GenerationStatementContent
	Input     domain.SimilarityInputV1
	Request   similarity.Request
	Evidence  similarity.Evidence
	Decision  similarity.Decision
}

func NewSimilarityExecutor(config SimilarityExecutorConfig) (*SimilarityExecutor, error) {
	if config.WorkflowRevision == "" {
		config.WorkflowRevision = workflow.Slice2CheckpointWorkflowRevision
	}
	if config.WorkflowRevision != workflow.Slice2CheckpointWorkflowRevision && !workflow.HasSolutionStages(config.WorkflowRevision) {
		return nil, errors.New("similarity executor requires a supported compiled workflow revision")
	}
	if config.Generation == nil || config.Provider == nil || config.CostUpperBoundMicroUSD <= 0 || config.Limit < config.Policy.MinimumHits || config.Limit <= 0 || config.Limit > 10000 {
		return nil, errors.New("similarity executor requires generation storage, provider and bounded policy")
	}
	if err := config.Policy.Validate(); err != nil {
		return nil, err
	}
	if err := config.RetryPolicy.Validate(); err != nil {
		return nil, err
	}
	if config.RetryPolicy.MaxAttempts > 8 {
		return nil, errors.New("similarity transport retry bound exceeds eight attempts")
	}
	store, ok := config.Generation.config.Store.(SimilarityReadStore)
	if !ok {
		return nil, errors.New("similarity executor requires committed evidence storage")
	}
	reader, err := NewSimilarityReader(store, config.Generation.config.Blobs, config.Provider)
	if err != nil {
		return nil, err
	}
	config.Policy.ReviewSources = slices.Clone(config.Policy.ReviewSources)
	return &SimilarityExecutor{config, reader}, nil
}

// InputForProblem performs pure planning without authentication or HTTP. Its
// digest is suitable for Statement's next-stage input before BeginStage.
func (s *SimilarityExecutor) InputForProblem(problem domain.ProblemSpec) (domain.SimilarityInputV1, error) {
	var empty domain.SimilarityInputV1
	if err := problem.Validate(); err != nil {
		return empty, err
	}
	request, err := s.request(problem, "similarity-input-planning/v1")
	if err != nil {
		return empty, err
	}
	plan, err := s.config.Provider.PlanSearch(request)
	if err != nil {
		return empty, err
	}
	raw, err := canonicalJSON(struct {
		Revision string             `json:"revision"`
		Provider domain.Digest      `json:"provider_policy"`
		Decision domain.Digest      `json:"decision_policy"`
		Limit    int                `json:"limit"`
		Retry    domain.RetryPolicy `json:"retry_policy"`
		Cost     int64              `json:"cost_upper_bound_micro_usd"`
	}{s.config.WorkflowRevision, plan.PolicyDigest, s.config.Policy.PolicyDigest, s.config.Limit, s.config.RetryPolicy, s.config.CostUpperBoundMicroUSD})
	if err != nil {
		return empty, err
	}
	return domain.NewSimilarityInputV1(problem, request.CandidateProjectionDigest, s.config.Policy.PolicyDigest, plan.PolicyDigest, domain.SumBytes(raw), s.config.Limit)
}

func (s *SimilarityExecutor) request(problem domain.ProblemSpec, logical string) (similarity.Request, error) {
	projection, err := similarity.NewPackageSafeProjection(problem.Title, problem.Description, problem.RequiredConstraints, problem.Language)
	if err != nil {
		return similarity.Request{}, err
	}
	request, err := similarity.NewRequest(projection, s.config.Policy.PolicyRef, s.config.Policy.PolicyDigest, logical)
	if err != nil {
		return request, err
	}
	request.Limit = s.config.Limit
	return request, request.Validate()
}

func (s *SimilarityExecutor) ReadInput(ctx context.Context, runID domain.RunID) (domain.SimilarityInputV1, error) {
	_, input, err := s.readInput(ctx, runID)
	return input, err
}

func (s *SimilarityExecutor) readInput(ctx context.Context, runID domain.RunID) (GenerationStatementContent, domain.SimilarityInputV1, error) {
	var statement GenerationStatementContent
	var input domain.SimilarityInputV1
	if ctx == nil {
		return statement, input, errors.New("similarity input read requires a context")
	}
	current, err := s.config.Generation.config.Store.GetRun(ctx, runID)
	if err != nil {
		return statement, input, err
	}
	if current.WorkflowRevision != s.config.WorkflowRevision || current.WorkflowDigest != domain.SumBytes([]byte(current.WorkflowRevision)) {
		return statement, input, errors.New("typed similarity requires the compiled checkpoint workflow")
	}
	statement, err = s.config.Generation.Reader().ReadStatement(ctx, runID)
	if err != nil {
		return statement, input, err
	}
	input, err = s.InputForProblem(statement.Problem)
	if err != nil {
		return statement, input, err
	}
	expected, err := s.config.Generation.config.Store.ReadStageInputDigest(ctx, runID, "similarity")
	if err != nil {
		return statement, input, err
	}
	digest, err := input.Digest()
	if err != nil || digest != expected {
		return statement, input, errors.New("similarity input differs from the committed Statement chain or execution policy")
	}
	return statement, input, nil
}

func (s *SimilarityExecutor) attemptRequest(runID domain.RunID, attempt domain.StageAttempt, problem domain.ProblemSpec, input domain.SimilarityInputV1) (similarity.Request, error) {
	digest, err := input.Digest()
	if err != nil {
		return similarity.Request{}, err
	}
	logical := coordinatorMutationID("similarity", runID, attempt.AttemptID, digest)
	return s.request(problem, logical)
}

func (s *SimilarityExecutor) RunSimilarity(ctx context.Context, view domain.RunView, input domain.SimilarityInputV1) (SimilarityStageResult, error) {
	var result SimilarityStageResult
	digest, err := input.Digest()
	if err != nil {
		return result, err
	}
	attempt, err := s.config.Generation.admit(ctx, view, "similarity", digest)
	if err != nil {
		return result, err
	}
	statement, expected, err := s.readInput(ctx, view.RunID())
	if err != nil {
		return result, err
	}
	if input != expected {
		return result, errors.New("similarity input differs from its verified private content chain")
	}
	request, err := s.attemptRequest(view.RunID(), attempt, statement.Problem, input)
	if err != nil {
		return result, err
	}
	plan, err := s.config.Provider.PlanSearch(request)
	if err != nil {
		return result, err
	}
	if plan.PolicyDigest != input.ProviderPolicyDigest {
		return result, errors.New("similarity provider policy changed after input planning")
	}
	generation := s.config.Generation.config
	ledger, err := NewRunBoundLLMLedger(generation.Store, view.RunID(), attempt.StageName, attempt.AttemptID)
	if err != nil {
		return result, err
	}
	calls, err := NewReplayableSimilarityCalls(ledger, s.config.Provider, generation.Blobs, generation.Clock, s.config.CostUpperBoundMicroUSD)
	if err != nil {
		return result, err
	}
	logical := request.LogicalIdempotencyKey
	open := domain.OpenCallRequest{ID: domain.CallRecordID(coordinatorMutationID("callrec", logical)), RunID: view.RunID(), ExpectedRunVersion: view.Version(), StageName: attempt.StageName, AttemptID: attempt.AttemptID, LogicalOperationID: logical, Kind: domain.CallSimilaritySearch, Provider: plan.Provider, RequestDigest: plan.RequestDigest, PolicyDigest: plan.PolicyDigest, RetryPolicy: s.config.RetryPolicy, IdempotencyKey: coordinatorMutationID("open", logical), At: attempt.StartedAt}
	searched, err := calls.SearchWithArtifacts(ctx, open, request)
	result.CallTrace = searched.Outcome.CallTrace
	if err != nil {
		return result, err
	}
	if err := searched.Outcome.Validate(); err != nil {
		return result, err
	}
	if searched.Outcome.Failure != nil {
		result.Outcome = similarityFailure(view, attempt, input.ExecutionPolicyDigest, plan.Provider, searched.Outcome, generation.Clock.Now())
		return result, nil
	}
	if searched.Artifact == nil {
		return result, ErrSimilarityReplayUnavailable
	}
	decision := similarity.Evaluate(s.config.Policy, *searched.Outcome.Value)
	if err := decision.Validate(); err != nil {
		return result, err
	}
	result.Outcome = domain.Success(*searched.Outcome.Value)
	result.Decision = &decision
	result.Occurrences = []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: searched.Artifact}}
	return result, nil
}

// ReadCommitted reconstructs both private generation content and similarity
// evidence. Neither this path nor ReadInput can issue a provider request.
func (s *SimilarityExecutor) ReadCommitted(ctx context.Context, runID domain.RunID) (SimilarityContent, error) {
	var result SimilarityContent
	statement, input, err := s.readInput(ctx, runID)
	if err != nil {
		return result, err
	}
	attempt, err := s.config.Generation.config.Store.CurrentStageAttempt(ctx, runID, "similarity")
	if err != nil {
		return result, err
	}
	request, err := s.attemptRequest(runID, attempt, statement.Problem, input)
	if err != nil {
		return result, err
	}
	evidence, err := s.reader.ReadTyped(ctx, runID, "similarity", input, request)
	if err != nil {
		return result, err
	}
	decision := similarity.Evaluate(s.config.Policy, evidence)
	if err := decision.Validate(); err != nil {
		return result, err
	}
	return SimilarityContent{statement, input, request, evidence, decision}, nil
}

func similarityFailure(view domain.RunView, attempt domain.StageAttempt, policy domain.Digest, provider string, outcome domain.MeteredOutcome[similarity.Evidence], now time.Time) domain.AgentResult[similarity.Evidence] {
	failure := outcome.Failure
	reason := "similarity_content_rejected"
	switch {
	case failure.Code == domain.FailureBudgetExhausted:
		reason = "similarity_budget_exhausted"
	case failure.Class == domain.FailureUnknown || failure.Code == domain.FailureBoundaryUnknown:
		reason = "similarity_boundary_unknown"
	case failure.Class == domain.FailureRetryable || failure.Class == domain.FailureBlocked || failure.Class == domain.FailureIncompatible:
		evidence, _ := canonicalJSON(struct {
			Code  domain.PortFailureCode `json:"code"`
			Class domain.FailureClass    `json:"class"`
			Trace domain.CallTrace       `json:"trace"`
		}{failure.Code, failure.Class, outcome.CallTrace})
		dependency := "similarity:" + provider
		retryAfter := now.UTC()
		if failure.RetryAfter != nil && failure.RetryAfter.After(retryAfter) {
			retryAfter = failure.RetryAfter.UTC()
		}
		return domain.Blocked[similarity.Evidence](domain.BlockedCheckpoint{RunID: view.RunID(), StageName: attempt.StageName, StageInputDigest: attempt.InputDigest, DependencyID: dependency, DependencyDigest: domain.SumBytes([]byte(dependency + "\x00" + string(policy))), PolicyDigest: policy, ErrorDigest: domain.SumBytes(evidence), RetryAfter: retryAfter, CreatedAt: now.UTC()})
	}
	return generationContentReview[similarity.Evidence](policy, outcome.CallTrace, reason)
}
