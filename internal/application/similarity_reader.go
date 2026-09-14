package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/similarity"
)

type similarityReadPolicy struct {
	WorkflowRevision       string
	Provider               durable.SimilarityReadPolicy
	Policy                 similarity.DecisionPolicy
	Limit                  int
	RetryPolicy            domain.RetryPolicy
	CostUpperBoundMicroUSD int64
}
type SimilarityContentReader struct {
	config    similarityReadPolicy
	reader    *durable.SimilarityReader
	store     SimilarityContentReadStore
	statement *GenerationReader
}

func (s *SimilarityContentReader) InputForProblem(problem domain.ProblemSpec) (domain.SimilarityInputV1, error) {
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

func (s *SimilarityContentReader) request(problem domain.ProblemSpec, logical string) (similarity.Request, error) {
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

func (s *SimilarityContentReader) ReadInput(ctx context.Context, runID domain.RunID) (domain.SimilarityInputV1, error) {
	_, input, err := s.readInput(ctx, runID)
	return input, err
}

func (s *SimilarityContentReader) readInput(ctx context.Context, runID domain.RunID) (GenerationStatementContent, domain.SimilarityInputV1, error) {
	var statement GenerationStatementContent
	var input domain.SimilarityInputV1
	if ctx == nil {
		return statement, input, errors.New("similarity input read requires a context")
	}
	current, err := s.store.GetRun(ctx, runID)
	if err != nil {
		return statement, input, err
	}
	if current.WorkflowRevision != s.config.WorkflowRevision || current.WorkflowDigest != domain.SumBytes([]byte(current.WorkflowRevision)) {
		return statement, input, errors.New("typed similarity requires the compiled checkpoint workflow")
	}
	statement, err = s.statement.ReadStatement(ctx, runID)
	if err != nil {
		return statement, input, err
	}
	input, err = s.InputForProblem(statement.Problem)
	if err != nil {
		return statement, input, err
	}
	expected, err := s.store.ReadStageInputDigest(ctx, runID, "similarity")
	if err != nil {
		return statement, input, err
	}
	digest, err := input.Digest()
	if err != nil || digest != expected {
		return statement, input, errors.New("similarity input differs from the committed Statement chain or execution policy")
	}
	return statement, input, nil
}

func (s *SimilarityContentReader) attemptRequest(runID domain.RunID, attempt domain.StageAttempt, problem domain.ProblemSpec, input domain.SimilarityInputV1) (similarity.Request, error) {
	digest, err := input.Digest()
	if err != nil {
		return similarity.Request{}, err
	}
	logical := durable.MutationID("similarity", runID, attempt.AttemptID, digest)
	return s.request(problem, logical)
}

func (s *SimilarityContentReader) ReadCommitted(ctx context.Context, runID domain.RunID) (SimilarityContent, error) {
	var result SimilarityContent
	statement, input, err := s.readInput(ctx, runID)
	if err != nil {
		return result, err
	}
	attempt, err := s.store.CurrentStageAttempt(ctx, runID, "similarity")
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
