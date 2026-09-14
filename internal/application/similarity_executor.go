package application

import (
	"context"
	"errors"
	"slices"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/runlock"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

type SimilarityStageConfig struct {
	WorkflowRevision       string
	Statement              *GenerationReader
	Admission              *StageAdmission
	Store                  SimilarityStageStore
	Blobs                  *blob.Store
	Clock                  clock.Clock
	Locks                  *runlock.Manager
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
	reader    *SimilarityContentReader
	admission *StageAdmission
	store     durable.Store
	blobs     *blob.Store
	clock     clock.Clock
	locks     *runlock.Manager
	provider  similarity.PhysicalProvider
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

func NewSimilarityExecutorWithConfig(config SimilarityStageConfig) (*SimilarityExecutor, error) {
	if config.WorkflowRevision == "" {
		config.WorkflowRevision = workflow.LegacySimilarityCheckpointRevision
	}
	if config.WorkflowRevision != workflow.LegacySimilarityCheckpointRevision && !workflow.HasSolutionStages(config.WorkflowRevision) {
		return nil, errors.New("similarity executor requires a supported compiled workflow revision")
	}
	if config.Statement == nil || config.Admission == nil || config.Store == nil || config.Blobs == nil || config.Clock == nil || config.Locks == nil || config.Provider == nil || config.CostUpperBoundMicroUSD <= 0 || config.Limit < config.Policy.MinimumHits || config.Limit <= 0 || config.Limit > 10000 {
		return nil, errors.New("similarity executor requires generation storage, provider and bounded policy")
	}
	if !sameDependency(config.Store, config.Admission.store) || !sameDependency(config.Store, config.Statement.store) {
		return nil, errors.New("similarity evidence and admission must share execution storage")
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
	reader, err := durable.NewSimilarityReader(config.Store, config.Blobs, config.Provider)
	if err != nil {
		return nil, err
	}
	config.Policy.ReviewSources = slices.Clone(config.Policy.ReviewSources)
	policy := similarityReadPolicy{config.WorkflowRevision, config.Provider, config.Policy, config.Limit, config.RetryPolicy, config.CostUpperBoundMicroUSD}
	content := &SimilarityContentReader{config: policy, reader: reader, store: config.Store, statement: config.Statement}
	return &SimilarityExecutor{reader: content, admission: config.Admission, store: config.Store, blobs: config.Blobs, clock: config.Clock, locks: config.Locks, provider: config.Provider}, nil
}

// InputForProblem performs pure planning without authentication or HTTP. Its
// digest is suitable for Statement's next-stage input before BeginStage.

func (s *SimilarityExecutor) RunSimilarity(ctx context.Context, view domain.RunView, input domain.SimilarityInputV1) (SimilarityStageResult, error) {
	var result SimilarityStageResult
	digest, err := input.Digest()
	if err != nil {
		return result, err
	}
	attempt, err := s.admission.admit(ctx, view, "similarity", digest)
	if err != nil {
		return result, err
	}
	statement, expected, err := s.reader.readInput(ctx, view.RunID())
	if err != nil {
		return result, err
	}
	if input != expected {
		return result, errors.New("similarity input differs from its verified private content chain")
	}
	request, err := s.reader.attemptRequest(view.RunID(), attempt, statement.Problem, input)
	if err != nil {
		return result, err
	}
	plan, err := s.provider.PlanSearch(request)
	if err != nil {
		return result, err
	}
	if plan.PolicyDigest != input.ProviderPolicyDigest {
		return result, errors.New("similarity provider policy changed after input planning")
	}
	ledger, err := durable.NewRunLedger(s.store, view.RunID(), attempt.StageName, attempt.AttemptID)
	if err != nil {
		return result, err
	}
	calls, err := durable.NewReplayableSimilarityCalls(ledger, s.provider, s.blobs, s.clock, s.reader.config.CostUpperBoundMicroUSD)
	if err != nil {
		return result, err
	}
	logical := request.LogicalIdempotencyKey
	open := domain.OpenCallRequest{ID: domain.CallRecordID(durable.MutationID("callrec", logical)), RunID: view.RunID(), ExpectedRunVersion: view.Version(), StageName: attempt.StageName, AttemptID: attempt.AttemptID, LogicalOperationID: logical, Kind: domain.CallSimilaritySearch, Provider: plan.Provider, RequestDigest: plan.RequestDigest, PolicyDigest: plan.PolicyDigest, RetryPolicy: s.reader.config.RetryPolicy, IdempotencyKey: durable.MutationID("open", logical), At: attempt.StartedAt}
	searched, err := calls.SearchWithArtifacts(ctx, open, request)
	result.CallTrace = searched.Outcome.CallTrace
	if err != nil {
		return result, err
	}
	if err := searched.Outcome.Validate(); err != nil {
		return result, err
	}
	if searched.Outcome.Failure != nil {
		result.Outcome = similarityFailure(view, attempt, input.ExecutionPolicyDigest, plan.Provider, searched.Outcome, s.clock.Now())
		return result, nil
	}
	if searched.Artifact == nil {
		return result, durable.ErrSimilarityReplayUnavailable
	}
	decision := similarity.Evaluate(s.reader.config.Policy, *searched.Outcome.Value)
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

func (s *SimilarityExecutor) Reader() *SimilarityContentReader { return s.reader }
func (s *SimilarityExecutor) Revision() string {
	if s == nil || s.reader == nil {
		return ""
	}
	return s.reader.config.WorkflowRevision
}
func (s *SimilarityExecutor) ReadInput(ctx context.Context, run domain.RunID) (domain.SimilarityInputV1, error) {
	return s.reader.ReadInput(ctx, run)
}
func (s *SimilarityExecutor) ReadCommitted(ctx context.Context, run domain.RunID) (SimilarityContent, error) {
	return s.reader.ReadCommitted(ctx, run)
}
func (s *SimilarityExecutor) InputForProblem(problem domain.ProblemSpec) (domain.SimilarityInputV1, error) {
	return s.reader.InputForProblem(problem)
}
