package fake

import (
	"context"
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
}

func NewStructuredLLM(responses ...[]byte) *StructuredLLM {
	queued := make([][]byte, 0, len(responses))
	for _, response := range responses {
		queued = append(queued, append([]byte(nil), response...))
	}
	return &StructuredLLM{responses: queued}
}

func (f *StructuredLLM) Generate(ctx context.Context, request port.GenerateRequest) (domain.MeteredOutcome[port.GenerateResponse], error) {
	if err := ctx.Err(); err != nil {
		return domain.MeteredOutcome[port.GenerateResponse]{}, err
	}
	if err := request.Validate(); err != nil {
		return domain.MeteredOutcome[port.GenerateResponse]{}, fmt.Errorf("fake structured LLM request: %w", err)
	}
	f.mu.Lock()
	f.requests = append(f.requests, request)
	if len(f.responses) == 0 {
		f.mu.Unlock()
		return domain.MeteredOutcome[port.GenerateResponse]{}, fmt.Errorf("fake structured LLM has no queued response")
	}
	raw := append([]byte(nil), f.responses[0]...)
	f.responses = f.responses[1:]
	f.mu.Unlock()
	if err := port.ValidateStructuredOutput(raw, request.Schema.SchemaVersion, request.MaxOutput.Bytes); err != nil {
		return domain.MeteredOutcome[port.GenerateResponse]{}, err
	}
	trace := domain.CallTrace{LogicalOperationID: "fake-structured-llm", DispatchKind: domain.DispatchNone}
	response := port.GenerateResponse{Structured: raw, Usage: port.Usage{}, CallTrace: trace}
	outcome := domain.MeteredOutcome[port.GenerateResponse]{Value: &response, CallTrace: trace}
	return outcome, nil
}

func (f *StructuredLLM) Requests() []port.GenerateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]port.GenerateRequest(nil), f.requests...)
}

var _ port.MeteredLLM = (*StructuredLLM)(nil)
