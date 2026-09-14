package execution

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// LLMReadPolicy plans identities and verifies saved bytes without transport.
type LLMReadPolicy interface {
	PlanGenerate(port.GenerateRequest) (port.LLMRequestPlan, error)
	ValidatePhysicalResponse(port.GenerateRequest, port.GenerateResponse) error
}

type ReceiptCallReadStore interface {
	LoadCall(context.Context, domain.CallRecordID) (domain.PreparedCalls, error)
	// ResumeDispatch reads an existing grant; it never authorizes a new send.
	ResumeDispatch(context.Context, int64, domain.AttemptCallID) (domain.DispatchGrant, error)
}

type CommittedLLMReadStore interface {
	ReceiptCallReadStore
	PrivateReceiptReadStore
	ReadCommittedLLMArtifact(context.Context, domain.RunID, domain.ArtifactWriterTokenID) (domain.CacheSource, domain.CacheBlob, error)
}

type CommittedDraftReader interface {
	PlanGenerate(port.GenerateRequest) (port.LLMRequestPlan, error)
	Bind(domain.OpenCallRequest, port.GenerateRequest) (domain.OpenCallRequest, port.GenerateRequest, error)
	ReadCommitted(context.Context, domain.OpenCallRequest, port.GenerateRequest) (domain.CallRecordID, *port.GenerateResponse, error)
	ReadCommittedLLMArtifact(context.Context, domain.RunID, domain.ArtifactWriterTokenID) (domain.CacheSource, domain.CacheBlob, error)
}

// CommittedLLMReader has no clock, writer, dispatcher, or mutable call ledger.
// It rejects unfinished publication instead of recovering it during a read.
type CommittedLLMReader struct {
	ledger   CommittedLLMReadStore
	blobs    port.VerifiedBlobReader
	provider LLMReadPolicy
	policy   FormatRepairPolicy
}

func NewCommittedLLMReader(store CommittedLLMReadStore, blobs port.VerifiedBlobReader, provider LLMReadPolicy, policy FormatRepairPolicy) (*CommittedLLMReader, error) {
	if store == nil || blobs == nil || provider == nil {
		return nil, errors.New("committed responses require evidence storage, blobs and read policy")
	}
	if err := policy.Validate(); err != nil {
		return nil, err
	}
	return &CommittedLLMReader{store, blobs, provider, policy}, nil
}

func (s *CommittedLLMReader) PlanGenerate(request port.GenerateRequest) (port.LLMRequestPlan, error) {
	return s.provider.PlanGenerate(request)
}
func (s *CommittedLLMReader) Bind(open domain.OpenCallRequest, request port.GenerateRequest) (domain.OpenCallRequest, port.GenerateRequest, error) {
	return bindStructuredRequest(s.provider, s.policy, open, request)
}
func (s *CommittedLLMReader) repairRequest(open domain.OpenCallRequest, request port.GenerateRequest, repair port.RepairInput) (domain.OpenCallRequest, port.GenerateRequest, error) {
	return buildRepairRequest(s.provider, s.policy, open, request, repair)
}
func (s *CommittedLLMReader) ReadCommittedLLMArtifact(ctx context.Context, run domain.RunID, token domain.ArtifactWriterTokenID) (domain.CacheSource, domain.CacheBlob, error) {
	return s.ledger.ReadCommittedLLMArtifact(ctx, run, token)
}
func (s *CommittedLLMReader) receipt(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest, grant domain.DispatchGrant) (domain.PhysicalExecution[port.GenerateResponse], *port.RepairInput, bool, error) {
	var empty domain.PhysicalExecution[port.GenerateResponse]
	binding, err := llmResponseBinding(open, request)
	if err != nil {
		return empty, nil, false, err
	}
	local, err := s.ledger.LoadCall(ctx, binding.callID)
	if err != nil {
		return empty, nil, false, err
	}
	if local.Call.State != domain.CallRecordTerminal || local.Call.Failure != nil {
		return empty, nil, false, errors.New("private response is not committed")
	}
	raw, pending, found, err := binding.readReceipt(ctx, s.ledger, s.blobs, grant, nil)
	if err != nil || !found {
		return empty, nil, found, err
	}
	return decodeLLMReceipt(raw, pending, grant, request, binding, s.provider)
}

func (s *StructuredLLMCalls) committedReader() (*CommittedLLMReader, error) {
	store, ok := s.calls.ledger.(CommittedLLMReadStore)
	if !ok {
		return nil, errors.New("committed response requires occurrence provenance reads")
	}
	return NewCommittedLLMReader(store, s.calls.artifacts.blobs, s.calls.provider, s.policy)
}
func (s *StructuredLLMCalls) PlanGenerate(request port.GenerateRequest) (port.LLMRequestPlan, error) {
	return s.calls.provider.PlanGenerate(request)
}
func (s *StructuredLLMCalls) ReadCommittedLLMArtifact(ctx context.Context, run domain.RunID, token domain.ArtifactWriterTokenID) (domain.CacheSource, domain.CacheBlob, error) {
	r, err := s.committedReader()
	if err != nil {
		return domain.CacheSource{}, domain.CacheBlob{}, err
	}
	return r.ReadCommittedLLMArtifact(ctx, run, token)
}

func (s *CommittedLLMReader) readCommittedResponse(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (*port.GenerateResponse, error) {
	prepared, err := s.ledger.LoadCall(ctx, open.ID)
	if err != nil {
		return nil, err
	}
	call := prepared.Call
	if call.RunID != open.RunID || call.StageName != open.StageName || call.AttemptID != open.AttemptID || call.RequestDigest != open.RequestDigest || call.PolicyDigest != open.PolicyDigest || call.LogicalOperationID != open.LogicalOperationID || call.State != domain.CallRecordTerminal || call.Failure != nil || call.ResultAttemptCallID == nil {
		return nil, errors.New("cache source is not a matching successful terminal provider call")
	}
	session, err := llmResponseBinding(open, request)
	if err != nil {
		return nil, err
	}
	local, err := s.ledger.LoadCall(ctx, session.callID)
	if err != nil {
		return nil, err
	}
	if local.Call.State != domain.CallRecordTerminal || local.Call.Failure != nil {
		return nil, errors.New("private response is not committed for cache reuse")
	}
	grant, err := s.ledger.ResumeDispatch(ctx, open.ExpectedRunVersion, *call.ResultAttemptCallID)
	if err != nil {
		return nil, err
	}
	execution, diagnostic, found, err := s.receipt(ctx, open, request, grant)
	if err != nil {
		return nil, err
	}
	if !found || diagnostic != nil || execution.Value == nil || execution.Value.RawBlob == nil || !matchesSuccessfulProviderReceipt(prepared, grant.AttemptCallID, execution) {
		return nil, errors.New("committed response does not match the successful provider receipt")
	}
	return execution.Value, nil
}

// ReadCommitted restores an already attached original response or its one
// bounded repair. It never opens, plans, dispatches or settles a logical call.
// The foreground caller holds the run and artifact locks. The returned call
// ID is the original provider provenance, not a new cache-use identity.
func (s *StructuredLLMCalls) ReadCommitted(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (domain.CallRecordID, *port.GenerateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	reader, err := s.committedReader()
	if err != nil {
		return "", nil, err
	}
	return reader.ReadCommitted(ctx, open, request)
}

func (s *CommittedLLMReader) ReadCommitted(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (domain.CallRecordID, *port.GenerateResponse, error) {
	if ctx == nil {
		return "", nil, errors.New("committed response requires a context")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	ledger := s.ledger
	open, request, err := s.Bind(open, request)
	if err != nil {
		return "", nil, err
	}
	prepared, err := s.ledger.LoadCall(ctx, open.ID)
	if err != nil {
		return "", nil, err
	}
	if prepared.Call.Failure != nil {
		if s.policy.MaxRepairs != 1 {
			return "", nil, errors.New("failed original call has no admitted repair")
		}
		// Prevent the general replay path from finalizing an uncommitted local
		// receipt: this API observes only already terminal producing calls.
		session, err := llmResponseBinding(open, request)
		if err != nil {
			return "", nil, err
		}
		local, err := s.ledger.LoadCall(ctx, session.callID)
		if err != nil {
			return "", nil, err
		}
		if local.Call.State != domain.CallRecordTerminal || local.Call.Failure != nil {
			return "", nil, errors.New("original validation receipt is not committed")
		}
		diagnostic, err := s.readFormatRepair(ctx, open, request)
		if err != nil {
			return "", nil, err
		}
		if diagnostic == nil {
			return "", nil, errors.New("failed original response has no verified format diagnostic")
		}
		open, request, err = s.repairRequest(open, request, *diagnostic)
		if err != nil {
			return "", nil, err
		}
	}
	response, err := s.readCommittedResponse(ctx, open, request)
	if err != nil {
		return "", nil, err
	}
	source, item, err := ledger.ReadCommittedLLMArtifact(ctx, open.RunID, response.RawBlob.WriterTokenID)
	if err != nil {
		return "", nil, err
	}
	if source.RunID != open.RunID || source.OccurrenceID != item.SourceOccurrenceID || source.Digest != response.RawBlob.Blob.Digest || item.Blob != response.RawBlob.Blob || item.MediaType != response.RawBlob.MediaType || item.Role != response.RawBlob.Role || item.LogicalPath != response.RawBlob.LogicalPath || item.Provenance.SchemaVersion != response.RawBlob.Provenance.SchemaVersion || item.Provenance.Producer != response.RawBlob.Provenance.Producer || !DigestsMatch(item.Provenance.InputDigest, response.RawBlob.Provenance.InputDigest) {
		return "", nil, errors.New("committed response differs from its attached occurrence")
	}
	return open.ID, response, nil
}
