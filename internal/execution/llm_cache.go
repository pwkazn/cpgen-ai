package execution

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
)

type LLMCacheLedger interface {
	port.CacheStore
	port.CacheEntryWriter
	ReadCommittedLLMArtifact(context.Context, domain.RunID, domain.ArtifactWriterTokenID) (domain.CacheSource, domain.CacheBlob, error)
}

type LLMCacheResult struct {
	Hit     bool
	Outcome domain.MeteredOutcome[port.GenerateResponse]
	Reuses  []domain.PendingCacheReuse
}

// StructuredLLMCache reuses only committed, verified results inside the same
// private run. The foreground executor must hold that run's process lock.
// Cache writes occur after stage commit; a miss never dispatches a provider.
type StructuredLLMCache struct {
	mu         sync.Mutex
	structured *StructuredLLMCalls
	ledger     LLMCacheLedger
	locks      *runlock.Manager
	cache      *CacheService
}

func NewStructuredLLMCache(structured *StructuredLLMCalls, ledger LLMCacheLedger, locks *runlock.Manager) (*StructuredLLMCache, error) {
	if structured == nil || ledger == nil || locks == nil {
		return nil, errors.New("structured LLM cache dependencies are required")
	}
	cache, err := NewCacheService(locks, ledger, structured.calls.ledger, structured.calls.artifacts.blobs)
	if err != nil {
		return nil, err
	}
	return &StructuredLLMCache{structured: structured, ledger: ledger, locks: locks, cache: cache}, nil
}

func (s *StructuredLLMCache) Identity(open domain.OpenCallRequest, request port.GenerateRequest) (domain.OpenCallRequest, port.GenerateRequest, domain.CacheKey, domain.Digest, error) {
	var key domain.CacheKey
	open, request, err := s.structured.Bind(open, request)
	if err != nil {
		return open, request, key, "", err
	}
	if request.PrivacyClassification != "private" {
		return open, request, key, "", errors.New("LLM response caching requires the private run partition")
	}
	if request.MaxOutput.Bytes <= 0 || request.MaxOutput.Bytes > 64<<20 {
		return open, request, key, "", errors.New("private LLM cache response limit must be within 64 MiB")
	}
	normalized := request
	normalized.LogicalIdempotencyKey = "cpgen-llm-cache-input-v1"
	plan, err := s.structured.calls.provider.PlanGenerate(normalized)
	if err != nil {
		return open, request, key, "", err
	}
	raw, err := json.Marshal(struct {
		Revision string        `json:"revision"`
		RunID    domain.RunID  `json:"run_id"`
		Input    domain.Digest `json:"input"`
	}{"cpgen.private-llm-cache/v1", open.RunID, plan.RequestDigest})
	if err != nil {
		return open, request, key, "", err
	}
	key = domain.CacheKey{Kind: "llm-structured-response", Digest: domain.SumBytes(raw)}
	return open, request, key, plan.RequestDigest, nil
}

// Put admits the original call or its one successful repair only after the
// complete private output is attached to a committed stage occurrence.
func (s *StructuredLLMCache) Put(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (domain.CacheKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil {
		return domain.CacheKey{}, errors.New("LLM cache context is required")
	}
	open, request, key, input, err := s.Identity(open, request)
	if err != nil {
		return key, err
	}
	guard, err := s.locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		return key, err
	}
	defer guard.Close()
	selectedOpen, selectedRequest := open, request
	prepared, err := s.structured.calls.ledger.LoadCall(ctx, open.ID)
	if err != nil {
		return key, err
	}
	if prepared.Call.Failure != nil {
		if s.structured.policy.MaxRepairs != 1 {
			return key, errors.New("failed model output cannot enter the cache")
		}
		diagnostic, err := s.structured.calls.ReadFormatRepair(ctx, open, request)
		if err != nil {
			return key, err
		}
		if diagnostic == nil {
			return key, errors.New("failed model output has no eligible repair")
		}
		selectedOpen, selectedRequest, err = s.structured.repairRequest(open, request, *diagnostic)
		if err != nil {
			return key, err
		}
	}
	response, err := s.structured.readCommittedResponse(ctx, selectedOpen, selectedRequest)
	if err != nil {
		return key, err
	}
	source, item, err := s.ledger.ReadCommittedLLMArtifact(ctx, open.RunID, response.RawBlob.WriterTokenID)
	if err != nil {
		return key, err
	}
	if item.Blob != response.RawBlob.Blob || item.Role != domain.ArtifactEvidence || item.MediaType != llmResponseMediaType || item.LogicalPath != response.RawBlob.LogicalPath || !strings.HasPrefix(string(item.LogicalPath), "private/llm/") || item.Provenance.SchemaVersion != response.RawBlob.Provenance.SchemaVersion || item.Provenance.Producer != response.RawBlob.Provenance.Producer || !DigestsMatch(item.Provenance.InputDigest, response.RawBlob.Provenance.InputDigest) {
		return key, errors.New("cache source is not the committed private response")
	}
	entry := domain.CacheEntry{Key: key, Kind: key.Kind, SchemaVersion: request.Schema.SchemaVersion, PolicyDigest: request.ProviderPolicyDigest, InputDigest: input, State: domain.CacheEntryValid, CreatedAt: s.structured.calls.clock.Now(), Source: source, Blobs: []domain.CacheBlob{item}}
	return key, s.ledger.PutCacheEntry(ctx, entry)
}

func (s *StructuredLLMCalls) readCommittedResponse(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (*port.GenerateResponse, error) {
	reader, err := s.committedReader()
	if err != nil {
		return nil, err
	}
	return reader.readCommittedResponse(ctx, open, request)
}

func matchesSuccessfulProviderReceipt(prepared domain.PreparedCalls, id domain.AttemptCallID, execution domain.PhysicalExecution[port.GenerateResponse]) bool {
	if prepared.Call.State != domain.CallRecordTerminal || prepared.Call.Failure != nil || prepared.Call.ResultAttemptCallID == nil || *prepared.Call.ResultAttemptCallID != id || execution.Value == nil || execution.Failure != nil || execution.Boundary != domain.BoundaryCompleted {
		return false
	}
	for _, physical := range prepared.PhysicalCalls {
		if physical.ID == id {
			return physical.Kind == domain.PhysicalLLMRequest && physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomeSuccess && physical.ProviderRequestID == execution.ProviderRequestID && DigestsMatch(physical.ResponseDigest, execution.ResponseDigest)
		}
	}
	return false
}

func (s *StructuredLLMCache) Reuse(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (LLMCacheResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result LLMCacheResult
	if ctx == nil {
		return result, errors.New("LLM cache context is required")
	}
	open, request, key, input, err := s.Identity(open, request)
	if err != nil {
		return result, err
	}
	lookup := domain.CacheLookup{RunID: open.RunID, Key: key, SchemaVersion: request.Schema.SchemaVersion, PolicyDigest: request.ProviderPolicyDigest, InputDigest: input, At: s.structured.calls.clock.Now()}
	open.Kind = domain.CallCacheReuse
	open.Provider = "private-llm-cache"
	open.RequestDigest = key.Digest
	finish := domain.FinishCallRequest{RunID: open.RunID, ExpectedRunVersion: open.ExpectedRunVersion, StageName: open.StageName, AttemptID: open.AttemptID, CallRecordID: open.ID, IdempotencyKey: MutationID("finish", open.ID, "private-llm-cache"), At: open.At}
	reuse := domain.CommitCacheReuse{CacheReuseRecordID: domain.CacheReuseRecordID(MutationID("reuse", open.ID, "private-llm-cache"))}
	var response *port.GenerateResponse
	hit, err := s.cache.ReuseValidated(ctx, domain.CacheReuseRequest{Lookup: lookup, OpenCall: open, FinishCall: finish, Reuse: reuse}, func(candidate domain.CacheCandidate) error {
		var err error
		response, err = s.validateCandidate(ctx, open, request, candidate)
		return err
	})
	if err != nil || !hit.Hit {
		return result, err
	}
	if response == nil {
		return result, errors.New("cache hit has no validated response")
	}
	response.RawBlob = nil
	response.Usage = port.Usage{}
	response.CallTrace = hit.Trace
	response.ProviderMeta = privateLLMMetadata(response.ProviderMeta)
	response.ProviderMeta["cache_provenance"] = "current_run_ledger"
	response.ProviderMeta["usage_source"] = "cache_zero"
	response.ProviderMeta["usage_settlement"] = "cache_zero"
	result = LLMCacheResult{Hit: true, Outcome: domain.MeteredOutcome[port.GenerateResponse]{Value: response, CallTrace: hit.Trace}, Reuses: hit.Reuses}
	return result, nil
}

func (s *StructuredLLMCache) validateCandidate(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest, candidate domain.CacheCandidate) (*port.GenerateResponse, error) {
	if len(candidate.Entry.Blobs) != 1 || len(candidate.SourceOccurrenceIDs) != 1 {
		return nil, errors.New("LLM cache requires one complete private response")
	}
	item := candidate.Entry.Blobs[0]
	if item.Role != domain.ArtifactEvidence || item.MediaType != llmResponseMediaType || !strings.HasPrefix(string(item.LogicalPath), "private/llm/") || item.Blob.Size > request.MaxOutput.Bytes*6+16384 {
		return nil, errors.New("LLM cache artifact policy differs")
	}
	reader, err := s.structured.calls.artifacts.blobs.OpenVerified(ctx, item.Blob)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, request.MaxOutput.Bytes*6+16385))
	closeErr := reader.Close()
	if err != nil || closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	var receipt llmResponseReceipt
	if err := port.DecodeStructuredOutput(raw, llmResponseSchema, request.MaxOutput.Bytes*6+16384, &receipt); err != nil {
		return nil, err
	}
	if receipt.FormatRepair != nil || receipt.Execution.Value == nil || receipt.Execution.Value.RawBlob != nil {
		return nil, errors.New("LLM cache receipt is not a successful original publication")
	}
	if receipt.Execution.Validate() != nil || item.Provenance.InputDigest == nil || *item.Provenance.InputDigest != receipt.RequestBinding {
		return nil, errors.New("LLM cache artifact binding differs from its response receipt")
	}
	sources := candidate.Entry.Sources
	if len(sources) == 0 {
		sources = []domain.CacheSource{candidate.Entry.Source}
	}
	if len(sources) != 1 || sources[0].RunID != open.RunID || sources[0].CallRecordID != candidate.SourceCallRecordID {
		return nil, errors.New("LLM cache source is outside the private run")
	}
	local, err := s.structured.calls.ledger.LoadCall(ctx, candidate.SourceCallRecordID)
	if err != nil {
		return nil, err
	}
	if local.Call.RunID != open.RunID || local.Call.State != domain.CallRecordTerminal || local.Call.Failure != nil || local.Call.Kind != domain.CallLLMGenerate || local.Call.Provider != "private-blob" || local.Call.LogicalOperationID != "llm-response:"+string(receipt.CallRecordID) || local.Call.RequestDigest != receipt.RequestBinding || local.Call.PolicyDigest != request.ProviderPolicyDigest {
		return nil, errors.New("LLM cache source provenance differs from the private receipt")
	}
	localReceipt := false
	for _, physical := range local.PhysicalCalls {
		if physical.Kind == domain.PhysicalLocalArtifactWrite && physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomeSuccess && physical.ResponseDigest != nil && *physical.ResponseDigest == item.Blob.Digest {
			localReceipt = true
		}
	}
	if !localReceipt {
		return nil, errors.New("LLM cache blob has no completed local publication")
	}
	parent, err := s.structured.calls.ledger.LoadCall(ctx, receipt.CallRecordID)
	if err != nil {
		return nil, err
	}
	if parent.Call.RunID != open.RunID || parent.Call.StageName != local.Call.StageName || parent.Call.AttemptID != local.Call.AttemptID || parent.Call.PolicyDigest != request.ProviderPolicyDigest || !matchesSuccessfulProviderReceipt(parent, receipt.AttemptCallID, receipt.Execution) || receipt.Execution.Value.CallTrace.LogicalOperationID != parent.Call.LogicalOperationID {
		return nil, errors.New("LLM cache parent does not match the verified provider receipt")
	}
	if err := s.structured.calls.provider.ValidatePhysicalResponse(request, *receipt.Execution.Value); err != nil {
		return nil, err
	}
	return receipt.Execution.Value, nil
}
