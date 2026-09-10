package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// ReadCommitted restores an already attached original response or its one
// bounded repair. It never opens, plans, dispatches or settles a logical call.
// The foreground caller holds the run and artifact locks. The returned call
// ID is the original provider provenance, not a new cache-use identity.
func (s *StructuredLLMCalls) ReadCommitted(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (domain.CallRecordID, *port.GenerateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil {
		return "", nil, errors.New("committed response requires a context")
	}
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	ledger, ok := s.calls.ledger.(LLMCacheLedger)
	if !ok {
		return "", nil, errors.New("committed response requires occurrence provenance reads")
	}
	open, request, err := s.bind(open, request)
	if err != nil {
		return "", nil, err
	}
	prepared, err := s.calls.ledger.LoadCall(ctx, open.ID)
	if err != nil {
		return "", nil, err
	}
	if prepared.Call.Failure != nil {
		if s.policy.MaxRepairs != 1 {
			return "", nil, errors.New("failed original call has no admitted repair")
		}
		// Prevent the general replay path from finalizing an uncommitted local
		// receipt: this API observes only already terminal producing calls.
		session, err := s.calls.artifacts.session(open, request)
		if err != nil {
			return "", nil, err
		}
		local, err := s.calls.ledger.LoadCall(ctx, session.callID)
		if err != nil {
			return "", nil, err
		}
		if local.Call.State != domain.CallRecordTerminal || local.Call.Failure != nil {
			return "", nil, errors.New("original validation receipt is not committed")
		}
		diagnostic, err := s.calls.ReadFormatRepair(ctx, open, request)
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
	if source.RunID != open.RunID || source.OccurrenceID != item.SourceOccurrenceID || source.Digest != response.RawBlob.Blob.Digest || item.Blob != response.RawBlob.Blob || item.MediaType != response.RawBlob.MediaType || item.Role != response.RawBlob.Role || item.LogicalPath != response.RawBlob.LogicalPath || item.Provenance.SchemaVersion != response.RawBlob.Provenance.SchemaVersion || item.Provenance.Producer != response.RawBlob.Provenance.Producer || !digestsMatch(item.Provenance.InputDigest, response.RawBlob.Provenance.InputDigest) {
		return "", nil, errors.New("committed response differs from its attached occurrence")
	}
	return open.ID, response, nil
}
