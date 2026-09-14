package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const llmResponseSchema domain.SchemaVersion = "cpgen.llm-response/v1"
const llmValidationSchema domain.SchemaVersion = "cpgen.llm-validation/v1"
const llmResponseMediaType = "application/vnd.cpgen.llm-response+json"

// ArtifactCallLedger keeps response writes on the existing artifact protocol.
// The caller must hold the run lock and shared artifact-maintenance lock.
type ArtifactCallLedger interface {
	port.CallLedger
	port.ArtifactLedger
	CreateArtifactDeclaration(context.Context, domain.ArtifactDeclarationRecord) error
	ReadArtifactWriter(context.Context, domain.ArtifactDeclarationID) (domain.ArtifactDeclarationRecord, domain.ArtifactWriterToken, error)
	ReadPendingArtifact(context.Context, domain.ArtifactDeclarationID) (domain.PendingArtifact, error)
	ReleaseUnwrittenArtifactReservations(context.Context, domain.OpenCallRequest) error
}

func NewReplayableLLMCalls(ledger ArtifactCallLedger, provider port.PhysicalLLM, blobs *blob.Store, source clock.Clock, costUpperBoundMicroUSD int64) (*LLMCalls, error) {
	if blobs == nil {
		return nil, errors.New("replayable LLM calls require private blob storage")
	}
	calls, err := NewLLMCalls(ledger, provider, source, costUpperBoundMicroUSD)
	if err != nil {
		return nil, err
	}
	calls.artifacts = &llmResponseArtifacts{ledger: ledger, blobs: blobs, clock: source}
	return calls, nil
}

type llmResponseArtifacts struct {
	ledger ArtifactCallLedger
	blobs  *blob.Store
	clock  clock.Clock
}
type llmResponseSession struct {
	*privateResponseSession
	request port.GenerateRequest
}

func (s *llmResponseArtifacts) session(open domain.OpenCallRequest, request port.GenerateRequest) (*llmResponseSession, error) {
	binding, err := llmResponseBinding(open, request)
	if err != nil {
		return nil, err
	}
	return &llmResponseSession{privateResponseSession: &privateResponseSession{privateResponseBinding: binding, ledger: s.ledger, blobs: s.blobs, clock: s.clock}, request: request}, nil
}

func llmResponseBinding(open domain.OpenCallRequest, request port.GenerateRequest) (privateResponseBinding, error) {
	// JSON escaping can expand structured output by up to six times. Reserve
	// bounded metadata space separately and reject overflow before any effect.
	if request.MaxOutput.Bytes <= 0 || request.MaxOutput.Bytes > 64<<20 {
		return privateResponseBinding{}, errors.New("private LLM response output limit must be within 64 MiB")
	}
	raw, err := json.Marshal(struct {
		Schema  domain.SchemaVersion `json:"schema"`
		Call    domain.CallRecordID  `json:"call"`
		Request port.GenerateRequest `json:"request"`
	}{llmResponseSchema, open.ID, request})
	if err != nil {
		return privateResponseBinding{}, err
	}
	return privateResponseBinding{open: open, binding: domain.SumBytes(raw), maxBytes: request.MaxOutput.Bytes*6 + 16384, callID: domain.CallRecordID(MutationID("callrec", "llm-response", open.ID)), prefix: "llm-response", mediaType: llmResponseMediaType, pathPrefix: "private/llm/", schema: llmResponseSchema}, nil
}

type llmResponseReceipt struct {
	SchemaVersion  domain.SchemaVersion                            `json:"schema_version"`
	RequestBinding domain.Digest                                   `json:"request_binding"`
	CallRecordID   domain.CallRecordID                             `json:"call_record_id"`
	AttemptCallID  domain.AttemptCallID                            `json:"attempt_call_id"`
	Execution      domain.PhysicalExecution[port.GenerateResponse] `json:"execution"`
	FormatRepair   *port.RepairInput                               `json:"format_repair,omitempty"`
}

func (s *llmResponseSession) publish(ctx context.Context, grant domain.DispatchGrant, writer port.ArtifactWriter, execution *domain.PhysicalExecution[port.GenerateResponse], provider port.PhysicalLLM, repair *port.RepairInput) error {
	if err := execution.Validate(); err != nil {
		return errors.New("invalid response receipt")
	}
	copyExecution := *execution
	schema := llmResponseSchema
	if execution.Value != nil {
		if repair != nil {
			return errors.New("successful receipt cannot contain format repair")
		}
		if err := provider.ValidatePhysicalResponse(s.request, *execution.Value); err != nil {
			return err
		}
		value := *execution.Value
		value.RawBlob = nil
		value.CallTrace = domain.CallTrace{LogicalOperationID: s.open.LogicalOperationID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &grant.AttemptCallID, PhysicalAttemptCallIDs: []domain.AttemptCallID{grant.AttemptCallID}}
		value.ProviderMeta = privateLLMMetadata(value.ProviderMeta)
		copyExecution.Value = &value
	} else {
		if execution.Failure == nil || execution.Failure.Code != domain.FailureProtocol || execution.Failure.Class != domain.FailureRejected || !validPrivateFormatRepair(repair, s.request.Schema.SchemaVersion) {
			return errors.New("invalid format repair receipt")
		}
		schema = llmValidationSchema
		failure := *execution.Failure
		failure.Evidence = nil
		copyExecution.Failure = &failure
	}
	receipt := llmResponseReceipt{schema, s.binding, grant.CallRecordID, grant.AttemptCallID, copyExecution, repair}
	raw, err := json.Marshal(receipt)
	if err != nil {
		return errors.New("cannot encode private response receipt")
	}
	if _, err := writer.Write(raw); err != nil {
		_ = writer.Abort(ctx)
		return err
	}
	pending, err := writer.Finalize(ctx)
	if err != nil {
		decl := s.declaration(grant.Ordinal)
		_, token, readErr := s.ledger.ReadArtifactWriter(ctx, decl.ID)
		if readErr == nil && (token.State == domain.ArtifactWriterSealed || token.State == domain.ArtifactWriterFinalized) {
			return fmt.Errorf("%w: %w", errCallReceiptPending, err)
		}
		return err
	}
	if err := s.markPublished(ctx, grant.Ordinal); err != nil {
		return fmt.Errorf("%w: %w", errCallReceiptPending, err)
	}
	if copyExecution.Value != nil {
		copyExecution.Value.RawBlob = &pending
	} else {
		copyExecution.Failure.Evidence = []domain.PendingArtifact{pending}
	}
	*execution = copyExecution
	return nil
}

func (s *llmResponseSession) replay(ctx context.Context, grant domain.DispatchGrant, provider port.PhysicalLLM) (domain.PhysicalExecution[port.GenerateResponse], bool, error) {
	execution, _, found, err := s.replayReceipt(ctx, grant, provider)
	return execution, found, err
}

func (s *llmResponseSession) replayReceipt(ctx context.Context, grant domain.DispatchGrant, provider port.PhysicalLLM) (domain.PhysicalExecution[port.GenerateResponse], *port.RepairInput, bool, error) {
	var empty domain.PhysicalExecution[port.GenerateResponse]
	raw, pending, found, err := s.readReceipt(ctx, grant)
	if err != nil || !found {
		return empty, nil, found, err
	}
	execution, repair, found, err := decodeLLMReceipt(raw, pending, grant, s.request, s.privateResponseBinding, provider)
	if err != nil || !found {
		return empty, nil, found, err
	}
	if err := s.markPublished(ctx, grant.Ordinal); err != nil {
		return empty, nil, false, fmt.Errorf("%w: %w", errCallReceiptPending, err)
	}
	return execution, repair, true, nil
}

func decodeLLMReceipt(raw []byte, pending domain.PendingArtifact, grant domain.DispatchGrant, request port.GenerateRequest, binding privateResponseBinding, provider LLMReadPolicy) (domain.PhysicalExecution[port.GenerateResponse], *port.RepairInput, bool, error) {
	var empty domain.PhysicalExecution[port.GenerateResponse]
	var header struct {
		SchemaVersion domain.SchemaVersion `json:"schema_version"`
	}
	if json.Unmarshal(raw, &header) != nil || (header.SchemaVersion != llmResponseSchema && header.SchemaVersion != llmValidationSchema) {
		return empty, nil, false, errors.New("private receipt schema is invalid")
	}
	var receipt llmResponseReceipt
	if err := port.DecodeStructuredOutput(raw, header.SchemaVersion, binding.maxBytes, &receipt); err != nil {
		return empty, nil, false, err
	}
	execution := receipt.Execution
	if receipt.RequestBinding != binding.binding || receipt.CallRecordID != grant.CallRecordID || receipt.AttemptCallID != grant.AttemptCallID || execution.Validate() != nil || execution.Boundary != domain.BoundaryCompleted {
		return empty, nil, false, errors.New("private response receipt binding is invalid")
	}
	if receipt.SchemaVersion == llmResponseSchema {
		if execution.Value == nil || execution.Value.RawBlob != nil || receipt.FormatRepair != nil {
			return empty, nil, false, errors.New("invalid successful private receipt")
		}
		if err := provider.ValidatePhysicalResponse(request, *execution.Value); err != nil {
			return empty, nil, false, err
		}
		execution.Value.RawBlob = &pending
	} else {
		if execution.Failure == nil || execution.Failure.Code != domain.FailureProtocol || execution.Failure.Class != domain.FailureRejected || len(execution.Failure.Evidence) != 0 || !validPrivateFormatRepair(receipt.FormatRepair, request.Schema.SchemaVersion) {
			return empty, nil, false, errors.New("invalid private validation receipt")
		}
		execution.Failure.Evidence = []domain.PendingArtifact{pending}
	}
	return execution, receipt.FormatRepair, true, nil
}

// Arbitrary provider metadata and prompt variables are excluded. These keys
// contain the same bounded, sanitized diagnostics used by the provider adapter.
func privateLLMMetadata(meta map[string]string) map[string]string {
	result := map[string]string{}
	for _, key := range []string{"model", "request_digest", "wire_request_digest", "logical_identity_digest", "response_digest", "usage_source", "usage_settlement", "attempt_count", "idempotency", "provider_request_id", "provider_model", "finish_reason", "adapter", "cache_provenance"} {
		if value, ok := meta[key]; ok && len(value) <= 256 {
			result[key] = value
		}
	}
	return result
}

func validPrivateFormatRepair(input *port.RepairInput, schema domain.SchemaVersion) bool {
	if input == nil || input.SchemaVersion != schema || input.Validate(port.DefaultRepairInputMaxBytes) != nil || len(input.FieldPaths) != 0 || len(input.InvalidFragment) != 0 {
		return false
	}
	for _, code := range input.ErrorCodes {
		if !code.FormatRepairable() {
			return false
		}
	}
	return true
}

// ReadFormatRepair returns diagnostics only from a verified private receipt
// bound to the terminal physical failure. It never grants provider dispatch.
// A nil result means that this failure is ineligible for JSON-format repair.
func (s *LLMCalls) ReadFormatRepair(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (*port.RepairInput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx == nil || s.artifacts == nil {
		return nil, errors.New("format repair requires context and private response storage")
	}
	return verifyFormatRepair(ctx, s.ledger, s.provider, open, request, func(ctx context.Context, grant domain.DispatchGrant) (domain.PhysicalExecution[port.GenerateResponse], *port.RepairInput, bool, error) {
		session, err := s.artifacts.session(open, request)
		if err != nil {
			return domain.PhysicalExecution[port.GenerateResponse]{}, nil, false, err
		}
		return session.replayReceipt(ctx, grant, s.provider)
	})
}

func verifyFormatRepair(ctx context.Context, ledger ReceiptCallReadStore, provider LLMReadPolicy, open domain.OpenCallRequest, request port.GenerateRequest, replay func(context.Context, domain.DispatchGrant) (domain.PhysicalExecution[port.GenerateResponse], *port.RepairInput, bool, error)) (*port.RepairInput, error) {
	if err := open.Validate(); err != nil {
		return nil, err
	}
	plan, err := provider.PlanGenerate(request)
	if err != nil {
		return nil, err
	}
	if open.Kind != domain.CallLLMGenerate || open.RequestDigest != plan.RequestDigest || open.Provider != plan.Provider || open.PolicyDigest != request.ProviderPolicyDigest || open.LogicalOperationID != request.LogicalIdempotencyKey {
		return nil, errors.New("format repair request binding differs")
	}
	prepared, err := ledger.LoadCall(ctx, open.ID)
	if err != nil {
		return nil, err
	}
	call := prepared.Call
	if call.RunID != open.RunID || call.StageName != open.StageName || call.AttemptID != open.AttemptID || call.RequestDigest != open.RequestDigest || call.PolicyDigest != open.PolicyDigest || call.LogicalOperationID != open.LogicalOperationID || call.State != domain.CallRecordTerminal {
		return nil, errors.New("format repair requires a matching terminal call")
	}
	if call.Failure == nil || call.Failure.Code != domain.FailureProtocol || call.Failure.Class != domain.FailureRejected || len(call.Failure.Evidence) == 0 {
		return nil, nil
	}
	if call.ResultAttemptCallID == nil || len(call.Failure.Evidence) != 1 {
		return nil, errors.New("format repair lacks one physical validation receipt")
	}
	grant, err := ledger.ResumeDispatch(ctx, open.ExpectedRunVersion, *call.ResultAttemptCallID)
	if err != nil {
		return nil, err
	}
	execution, repair, found, err := replay(ctx, grant)
	if err != nil {
		return nil, err
	}
	if !found || repair == nil || execution.Failure == nil || len(execution.Failure.Evidence) != 1 {
		return nil, errors.New("format repair receipt is unavailable")
	}
	expected, actual := call.Failure.Evidence[0], execution.Failure.Evidence[0]
	if expected.Blob != actual.Blob || expected.WriterTokenID != actual.WriterTokenID || expected.ReservationID != actual.ReservationID || expected.CallID != actual.CallID || expected.PinID != actual.PinID {
		return nil, errors.New("format repair evidence differs from durable failure")
	}
	for _, physical := range prepared.PhysicalCalls {
		if physical.ID == grant.AttemptCallID && physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomePermanentFailure && physical.ProviderRequestID == execution.ProviderRequestID && DigestsMatch(physical.ResponseDigest, execution.ResponseDigest) {
			return repair, nil
		}
	}
	return nil, errors.New("format repair receipt differs from completed physical call")
}

func (s *CommittedLLMReader) readFormatRepair(ctx context.Context, open domain.OpenCallRequest, request port.GenerateRequest) (*port.RepairInput, error) {
	return verifyFormatRepair(ctx, s.ledger, s.provider, open, request, func(ctx context.Context, grant domain.DispatchGrant) (domain.PhysicalExecution[port.GenerateResponse], *port.RepairInput, bool, error) {
		return s.receipt(ctx, open, request, grant)
	})
}
