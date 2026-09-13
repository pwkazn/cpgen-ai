package application

import (
	"context"
	"errors"
	"io"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/similarity"
)

// SimilarityReadPolicy has no transport methods or provider credentials.
type SimilarityReadPolicy interface {
	PlanSearch(similarity.Request) (similarity.PhysicalSearchPlan, error)
	ValidatePhysicalResponse(similarity.Request, similarity.Evidence) error
}
type similarityReadPolicy struct {
	WorkflowRevision       string
	Provider               SimilarityReadPolicy
	Policy                 similarity.DecisionPolicy
	Limit                  int
	RetryPolicy            domain.RetryPolicy
	CostUpperBoundMicroUSD int64
}
type SimilarityContentReader struct {
	config    similarityReadPolicy
	reader    *SimilarityReader
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
	logical := coordinatorMutationID("similarity", runID, attempt.AttemptID, digest)
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

// SimilarityReadStore deliberately exposes no mutation, dispatch or writer
// methods. A sealed receipt belongs to recovery, never to this committed read.
type SimilarityReadStore interface {
	ReadCommittedSimilarityStage(context.Context, domain.RunID, domain.StageName) (port.CommittedPrivateStage, error)
	LoadCall(context.Context, domain.CallRecordID) (domain.PreparedCalls, error)
}

type SimilarityReader struct {
	store    SimilarityReadStore
	blobs    port.VerifiedBlobReader
	provider SimilarityReadPolicy
}

func NewSimilarityReader(store SimilarityReadStore, blobs port.VerifiedBlobReader, provider SimilarityReadPolicy) (*SimilarityReader, error) {
	if store == nil || blobs == nil || provider == nil {
		return nil, errors.New("similarity reader requires committed metadata, verified blobs and provider policy")
	}
	return &SimilarityReader{store, blobs, provider}, nil
}

func (r *SimilarityReader) Read(ctx context.Context, runID domain.RunID, stage domain.StageName, request similarity.Request) (similarity.Evidence, error) {
	input, err := request.Digest()
	if err != nil {
		return similarity.Evidence{}, err
	}
	return r.read(ctx, runID, stage, request, input, nil)
}

// ReadTyped retains the semantic input committed before the attempt existed.
// The application verifies its private ProblemSpec chain; this reader verifies
// the public projection, policies and exact attempt-bound wire receipt.
func (r *SimilarityReader) ReadTyped(ctx context.Context, runID domain.RunID, stage domain.StageName, input domain.SimilarityInputV1, request similarity.Request) (similarity.Evidence, error) {
	digest, err := input.Digest()
	if err != nil {
		return similarity.Evidence{}, err
	}
	return r.read(ctx, runID, stage, request, digest, &input)
}

func (r *SimilarityReader) read(ctx context.Context, runID domain.RunID, stage domain.StageName, request similarity.Request, input domain.Digest, typed *domain.SimilarityInputV1) (similarity.Evidence, error) {
	var empty similarity.Evidence
	if ctx == nil {
		return empty, errors.New("similarity read requires a context")
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	plan, err := r.provider.PlanSearch(request)
	if err != nil {
		return empty, err
	}
	if typed != nil && (typed.CandidateProjectionDigest != request.CandidateProjectionDigest || typed.DecisionPolicyDigest != request.PolicyDigest || typed.ProviderPolicyDigest != plan.PolicyDigest || typed.Limit != request.Limit) {
		return empty, errors.New("similarity wire request differs from its semantic input")
	}
	committed, err := r.store.ReadCommittedSimilarityStage(ctx, runID, stage)
	if err != nil {
		return empty, err
	}
	attempt := committed.Attempt
	if attempt.Validate() != nil || committed.StageVersion <= 0 || attempt.RunID != runID || attempt.StageName != stage || attempt.State != domain.StageAttemptSucceeded || attempt.InputDigest != input || attempt.OutputDigest == nil || len(committed.Artifacts) != 1 {
		return empty, errors.New("committed similarity stage differs from its typed input")
	}
	item := committed.Artifacts[0]
	if item.Kind != domain.PendingOccurrenceNewWrite || item.OccurrenceID != item.Source.OccurrenceID || item.CurrentCallRecordID != item.Source.CallRecordID || item.Source.RunID != runID || item.Source.Digest != item.Blob.Blob.Digest || item.Blob.SourceOccurrenceID != item.OccurrenceID || item.Blob.Role != domain.ArtifactEvidence || item.Blob.MediaType != similarityResponseMediaType {
		return empty, errors.New("committed similarity receipt provenance differs")
	}
	parent, err := r.store.LoadCall(ctx, item.ProviderCallRecordID)
	if err != nil {
		return empty, err
	}
	call := parent.Call
	if call.RunID != runID || call.StageName != stage || call.AttemptID != attempt.AttemptID || call.Kind != domain.CallSimilaritySearch || call.Provider != plan.Provider || call.RequestDigest != plan.RequestDigest || call.PolicyDigest != plan.PolicyDigest || call.LogicalOperationID != request.LogicalIdempotencyKey || call.State != domain.CallRecordTerminal || call.Failure != nil || call.ResultAttemptCallID == nil || parent.CallTrace == nil || parent.CallTrace.Validate() != nil {
		return empty, errors.New("committed similarity provider binding differs")
	}
	binding, err := similarityResponseBinding(call.ID, request, plan)
	if err != nil {
		return empty, err
	}
	limit := plan.MaxResponseBytes*6 + 16384
	if item.Blob.Blob.Size > limit || item.Blob.Provenance.SchemaVersion != similarityResponseSchema || item.Blob.Provenance.Producer != string(similarityResponseSchema) || item.Blob.Provenance.InputDigest == nil || *item.Blob.Provenance.InputDigest != binding || item.Blob.LogicalPath != domain.SafeRelPath("private/similarity/"+string(*call.ResultAttemptCallID)+".json") {
		return empty, errors.New("committed similarity artifact policy differs")
	}
	reader, err := r.blobs.OpenVerified(ctx, item.Blob.Blob)
	if err != nil {
		return empty, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if closeErr := reader.Close(); err != nil || closeErr != nil {
		return empty, errors.Join(err, closeErr)
	}
	var receipt similarityResponseReceipt
	if err := port.DecodeStructuredOutput(raw, similarityResponseSchema, limit, &receipt); err != nil {
		return empty, err
	}
	if receipt.RequestBinding != binding || receipt.CallRecordID != call.ID || receipt.AttemptCallID != *call.ResultAttemptCallID {
		return empty, errors.New("committed similarity receipt substituted its identity")
	}
	if err := validateSimilarityReceiptExecution(parent, request, receipt.AttemptCallID, receipt.Execution, r.provider); err != nil {
		return empty, err
	}
	ordinal := int64(0)
	for _, physical := range parent.PhysicalCalls {
		if physical.ID == receipt.AttemptCallID && physical.Kind == domain.PhysicalSimilarityRequest && physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomeSuccess && physical.ProviderRequestID == receipt.Execution.ProviderRequestID && digestsMatch(physical.ResponseDigest, receipt.Execution.ResponseDigest) {
			ordinal = physical.Ordinal
		}
	}
	if ordinal < 1 {
		return empty, errors.New("committed similarity response has no completed exchange")
	}
	local, err := r.store.LoadCall(ctx, item.Source.CallRecordID)
	if err != nil {
		return empty, err
	}
	expectedLocal := domain.CallRecordID(coordinatorMutationID("callrec", "similarity-response", call.ID))
	if local.Call.ID != expectedLocal || local.Call.RunID != runID || local.Call.StageName != stage || local.Call.AttemptID != attempt.AttemptID || local.Call.Kind != domain.CallSimilaritySearch || local.Call.Provider != "private-blob" || local.Call.LogicalOperationID != "similarity-response:"+string(call.ID) || local.Call.State != domain.CallRecordTerminal || local.Call.Failure != nil || local.Call.RequestDigest != binding || local.Call.PolicyDigest != plan.PolicyDigest {
		return empty, errors.New("committed similarity publication binding differs")
	}
	publication := false
	for _, physical := range local.PhysicalCalls {
		if physical.ID == domain.AttemptCallID(coordinatorMutationID("artifact", expectedLocal, ordinal)) && physical.Kind == domain.PhysicalLocalArtifactWrite && physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomeSuccess && physical.ResponseDigest != nil && *physical.ResponseDigest == item.Blob.Blob.Digest {
			publication = true
		}
	}
	if !publication {
		return empty, errors.New("committed similarity bytes have no completed publication")
	}
	value := receipt.Execution.Value
	evidence, err := similarity.NewEvidenceWithServiceIdentity(request, value.ProviderIdentity, value.ServiceIdentity, value.Hits, value.ObservedAt, value.Usage, value.UsageSource, value.ModelVersion, value.IndexVersion, value.Cache, *parent.CallTrace)
	if err != nil {
		return empty, err
	}
	if evidence.EvidenceDigest != *attempt.OutputDigest {
		return empty, errors.New("committed similarity semantic digest differs")
	}
	return evidence, nil
}
