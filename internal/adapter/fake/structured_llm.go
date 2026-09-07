package fake

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// StructuredLLM is a deterministic fixture for provider-boundary tests. It
// consumes responses in FIFO order, records a copy of each request, and runs
// the same local strict response validation expected from a real adapter.
// There is no network or model dependency.
type StructuredLLM struct {
	mu        sync.Mutex
	responses [][]byte
	requests  []port.GenerateRequest
	validator StructuredResponseValidator
	registry  *port.SchemaValidatorRegistry
}

func NewStructuredLLM(responses ...[]byte) *StructuredLLM {
	return NewStructuredLLMWithRegistry(port.DefaultSchemaValidatorRegistry(), responses...)
}

// StructuredResponseValidator lets a test install the typed schema validator
// used by its response type. The callback must call port.DecodeStructuredOutput
// (or DecodeStructuredWithSchema) before applying domain constraints.
type StructuredResponseValidator func(raw []byte, schema port.OutputSchemaRef, maxBytes int64) error

func NewStructuredLLMWithValidator(validator StructuredResponseValidator, responses ...[]byte) *StructuredLLM {
	return newStructuredLLM(validator, nil, responses...)
}

// NewStructuredLLMWithRegistry requires each response schema digest to have
// a registered typed validator. This is the safe default for fixtures that
// model a complete provider boundary.
func NewStructuredLLMWithRegistry(registry *port.SchemaValidatorRegistry, responses ...[]byte) *StructuredLLM {
	return newStructuredLLM(nil, registry, responses...)
}

func newStructuredLLM(validator StructuredResponseValidator, registry *port.SchemaValidatorRegistry, responses ...[]byte) *StructuredLLM {
	queued := make([][]byte, 0, len(responses))
	for _, response := range responses {
		queued = append(queued, append([]byte(nil), response...))
	}
	return &StructuredLLM{responses: queued, validator: validator, registry: registry}
}

func (f *StructuredLLM) Generate(ctx context.Context, request port.GenerateRequest) (domain.MeteredOutcome[port.GenerateResponse], error) {
	if err := ctx.Err(); err != nil {
		return domain.MeteredOutcome[port.GenerateResponse]{}, err
	}
	if err := request.Validate(); err != nil {
		return domain.MeteredOutcome[port.GenerateResponse]{}, fmt.Errorf("fake structured LLM request: %w", err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, cloneGenerateRequest(request))
	if len(f.responses) == 0 {
		f.mu.Unlock()
		return domain.MeteredOutcome[port.GenerateResponse]{}, fmt.Errorf("fake structured LLM has no queued response")
	}
	raw := append([]byte(nil), f.responses[0]...)
	f.responses = f.responses[1:]
	f.mu.Unlock()
	var validationErr error
	if f.validator != nil {
		// Even compatibility callbacks must pass the same generic provider
		// boundary first; a callback returning nil cannot opt out of strict
		// JSON/schema/duplicate-field checks.
		validationErr = port.ValidateStructuredOutput(raw, request.Schema.SchemaVersion, request.MaxOutput.Bytes)
		if validationErr == nil {
			validationErr = f.validator(raw, request.Schema, request.MaxOutput.Bytes)
		}
	} else if f.registry != nil {
		validationErr = f.registry.Validate(raw, request.Schema, request.MaxOutput.Bytes)
	} else {
		validationErr = &port.StructuredOutputError{Code: port.StructuredOutputSchemaUnbound}
	}
	if validationErr != nil {
		var typed *port.StructuredOutputError
		if !errors.As(validationErr, &typed) {
			return domain.MeteredOutcome[port.GenerateResponse]{}, &port.StructuredOutputError{Code: port.StructuredOutputTypeMismatch}
		}
		return domain.MeteredOutcome[port.GenerateResponse]{}, validationErr
	}
	trace := domain.CallTrace{LogicalOperationID: "fake-structured-llm", DispatchKind: domain.DispatchNone}
	response := port.GenerateResponse{Structured: raw, Usage: port.Usage{}, CallTrace: trace}
	outcome := domain.MeteredOutcome[port.GenerateResponse]{Value: &response, CallTrace: trace}
	return outcome, nil
}

func (f *StructuredLLM) Requests() []port.GenerateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := make([]port.GenerateRequest, len(f.requests))
	for index, request := range f.requests {
		requests[index] = cloneGenerateRequest(request)
	}
	return requests
}

var _ port.MeteredLLM = (*StructuredLLM)(nil)
