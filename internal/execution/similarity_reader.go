package execution

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
		if physical.ID == receipt.AttemptCallID && physical.Kind == domain.PhysicalSimilarityRequest && physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomeSuccess && physical.ProviderRequestID == receipt.Execution.ProviderRequestID && DigestsMatch(physical.ResponseDigest, receipt.Execution.ResponseDigest) {
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
	expectedLocal := domain.CallRecordID(MutationID("callrec", "similarity-response", call.ID))
	if local.Call.ID != expectedLocal || local.Call.RunID != runID || local.Call.StageName != stage || local.Call.AttemptID != attempt.AttemptID || local.Call.Kind != domain.CallSimilaritySearch || local.Call.Provider != "private-blob" || local.Call.LogicalOperationID != "similarity-response:"+string(call.ID) || local.Call.State != domain.CallRecordTerminal || local.Call.Failure != nil || local.Call.RequestDigest != binding || local.Call.PolicyDigest != plan.PolicyDigest {
		return empty, errors.New("committed similarity publication binding differs")
	}
	publication := false
	for _, physical := range local.PhysicalCalls {
		if physical.ID == domain.AttemptCallID(MutationID("artifact", expectedLocal, ordinal)) && physical.Kind == domain.PhysicalLocalArtifactWrite && physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomeSuccess && physical.ResponseDigest != nil && *physical.ResponseDigest == item.Blob.Blob.Digest {
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
