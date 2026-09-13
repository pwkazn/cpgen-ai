package agent

import (
	"context"
	"errors"
	"os"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

var _ port.PhysicalLLM = (*LangChain)(nil)

func (a *LangChain) ValidatePhysicalResponse(request port.GenerateRequest, response port.GenerateResponse) error {
	if a == nil || a.core == nil {
		return &Error{Code: ErrorConfiguration}
	}
	if _, err := a.core.generationDefinition(request); err != nil {
		return err
	}
	if err := response.Validate(); err != nil {
		return &Error{Code: ErrorProtocol}
	}
	return a.core.config.SchemaRegistry.Validate(response.Structured, request.Schema, request.MaxOutput.Bytes)
}

// PlanGenerate validates the complete request before any reservation or send.
// It deliberately performs neither credential reads nor external I/O.
func (a *LangChain) PlanGenerate(request port.GenerateRequest) (port.LLMRequestPlan, error) {
	if a == nil || a.core == nil {
		return port.LLMRequestPlan{}, &Error{Code: ErrorConfiguration}
	}
	definition, err := a.core.generationDefinition(request)
	if err != nil {
		return port.LLMRequestPlan{}, err
	}
	body, digest, err := a.core.requestBody(request, definition)
	if err != nil {
		return port.LLMRequestPlan{}, &Error{Code: ErrorConfiguration}
	}
	return port.LLMRequestPlan{RequestDigest: digest, Provider: strings.ToLower(a.core.endpoint.Hostname()), InputTokenUpperBound: int64(len(body)), OutputTokenUpperBound: request.MaxOutput.Tokens}, nil
}

// GeneratePhysical accepts the ledger's physical identity and never enters
// the standalone retry loop, even if Config.MaxAttempts is greater than one.
func (a *LangChain) GeneratePhysical(ctx context.Context, request port.GenerateRequest, id domain.AttemptCallID) (port.PhysicalLLMResult, error) {
	var result port.PhysicalLLMResult
	if a == nil || a.core == nil || ctx == nil || id.Validate() != nil {
		return result, &Error{Code: ErrorConfiguration}
	}
	definition, err := a.core.generationDefinition(request)
	if err != nil {
		return result, err
	}
	body, requestDigest, err := a.core.requestBody(request, definition)
	if err != nil {
		return result, &Error{Code: ErrorConfiguration}
	}
	logicalDigest, err := a.core.logicalIdentityDigest(request)
	if err != nil {
		return result, &Error{Code: ErrorConfiguration}
	}
	noSend := func(code domain.PortFailureCode, class domain.FailureClass) (port.PhysicalLLMResult, error) {
		return port.PhysicalLLMResult{Execution: domain.PhysicalExecution[port.GenerateResponse]{Boundary: domain.BoundaryConfirmedNoSend, Failure: &domain.PortFailure{Code: code, Class: class}}}, nil
	}
	if ctx.Err() != nil {
		return noSend(domain.FailureTransport, domain.FailureRejected)
	}
	key, ok := os.LookupEnv(a.core.config.APIKeyEnv)
	if !ok || !validCredential(key) {
		return noSend(domain.FailurePolicyRejected, domain.FailureBlocked)
	}
	logicalID := "llm:" + strings.TrimPrefix(string(logicalDigest), "sha256:")
	raw, status, providerID, retryAfter, sent, sendErr := a.core.doRequest(ctx, body, key, "cpgen-llm-"+strings.TrimPrefix(string(logicalDigest), "sha256:"))
	if sendErr != nil {
		if !sent {
			return noSend(failureCode(sendErr), failureClass(sendErr))
		}
		result.Execution = domain.PhysicalExecution[port.GenerateResponse]{Boundary: domain.BoundaryUnknown, Failure: &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}}
		result.Usage, _ = conservativeUsage(nil, len(body), request.MaxOutput.Tokens)
		return result, nil
	}
	usage := usageForAttempt(raw, status, len(body), request.MaxOutput.Tokens)
	result.Usage, result.UsageVerified = usage.Usage, usage.Source == "provider_verified"
	digest := domain.SumBytes(raw)
	// Some providers omit request IDs, especially on errors. This local receipt
	// is explicitly labelled, distinct from a claimed provider-issued identity.
	if providerID == "" {
		providerID = "local-receipt:" + string(id)
	}
	result.Execution = domain.PhysicalExecution[port.GenerateResponse]{Boundary: domain.BoundaryCompleted, ProviderRequestID: providerID, ResponseDigest: &digest}
	if status < 200 || status >= 300 {
		classified := classifyHTTP(status, retryAfter)
		result.Execution.Failure = &domain.PortFailure{Code: failureCode(classified), Class: failureClass(classified)}
		if retryAfter > 0 {
			at := a.core.now().Add(retryAfter)
			result.Execution.Failure.RetryAfter = &at
		}
		return result, nil
	}
	validator := func(raw []byte) error {
		return a.core.config.SchemaRegistry.Validate(raw, request.Schema, request.MaxOutput.Bytes)
	}
	response, err := a.core.decodeResponse(raw, body, request, requestDigest, logicalDigest, validator, logicalID, []domain.AttemptCallID{id}, providerID)
	if err != nil {
		result.Execution.Failure = &domain.PortFailure{Code: domain.FailureProtocol, Class: domain.FailureRejected}
		var validation *port.StructuredOutputError
		if errors.As(err, &validation) && validation.Code.FormatRepairable() {
			input, inputErr := port.NewBoundedRepairInput(request.Schema.SchemaVersion, []*port.StructuredOutputError{{Code: validation.Code}}, nil, port.DefaultRepairInputMaxBytes)
			if inputErr == nil {
				result.FormatRepair = &input
			}
		}
		return result, nil
	}
	response.ProviderMeta["adapter"] = "langchaingo-openai-v1"
	response.Usage = result.Usage
	result.Execution.Value = &response
	return result, nil
}
