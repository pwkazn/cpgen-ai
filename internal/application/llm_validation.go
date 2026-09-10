package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

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
	if err := open.Validate(); err != nil {
		return nil, err
	}
	plan, err := s.provider.PlanGenerate(request)
	if err != nil {
		return nil, err
	}
	if open.Kind != domain.CallLLMGenerate || open.RequestDigest != plan.RequestDigest || open.Provider != plan.Provider || open.PolicyDigest != request.ProviderPolicyDigest || open.LogicalOperationID != request.LogicalIdempotencyKey {
		return nil, errors.New("format repair request binding differs")
	}
	prepared, err := s.ledger.LoadCall(ctx, open.ID)
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
	grant, err := s.ledger.ResumeDispatch(ctx, open.ExpectedRunVersion, *call.ResultAttemptCallID)
	if err != nil {
		return nil, err
	}
	session, err := s.artifacts.session(open, request)
	if err != nil {
		return nil, err
	}
	execution, repair, found, err := session.replayReceipt(ctx, grant, s.provider)
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
		if physical.ID == grant.AttemptCallID && physical.State == domain.PhysicalCompleted && physical.Outcome != nil && *physical.Outcome == domain.PhysicalOutcomePermanentFailure && physical.ProviderRequestID == execution.ProviderRequestID && digestsMatch(physical.ResponseDigest, execution.ResponseDigest) {
			return repair, nil
		}
	}
	return nil, errors.New("format repair receipt differs from completed physical call")
}
