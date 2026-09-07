package fake

import (
	"context"
	"fmt"
	"sync"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type LLM struct {
	mu       sync.Mutex
	outcomes []domain.MeteredOutcome[port.GenerateResponse]
	requests []port.GenerateRequest
}

func NewLLM(outcomes ...domain.MeteredOutcome[port.GenerateResponse]) *LLM {
	return &LLM{outcomes: append([]domain.MeteredOutcome[port.GenerateResponse](nil), outcomes...)}
}

func (f *LLM) Generate(ctx context.Context, request port.GenerateRequest) (domain.MeteredOutcome[port.GenerateResponse], error) {
	if err := ctx.Err(); err != nil {
		return domain.MeteredOutcome[port.GenerateResponse]{}, err
	}
	if err := request.Validate(); err != nil {
		return domain.MeteredOutcome[port.GenerateResponse]{}, fmt.Errorf("fake LLM request: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, cloneGenerateRequest(request))
	if len(f.outcomes) == 0 {
		return domain.MeteredOutcome[port.GenerateResponse]{}, fmt.Errorf("fake LLM has no queued outcome")
	}
	outcome := f.outcomes[0]
	f.outcomes = f.outcomes[1:]
	if err := outcome.Validate(); err != nil {
		return domain.MeteredOutcome[port.GenerateResponse]{}, fmt.Errorf("invalid fake LLM outcome: %w", err)
	}
	if outcome.Value != nil {
		if err := outcome.Value.Validate(); err != nil {
			return domain.MeteredOutcome[port.GenerateResponse]{}, fmt.Errorf("invalid fake LLM response: %w", err)
		}
		if !outcome.Value.CallTrace.Equal(outcome.CallTrace) {
			return domain.MeteredOutcome[port.GenerateResponse]{}, fmt.Errorf("fake LLM value and wrapper call traces differ")
		}
	}
	return outcome, nil
}

func (f *LLM) Requests() []port.GenerateRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	requests := make([]port.GenerateRequest, len(f.requests))
	for index, request := range f.requests {
		requests[index] = cloneGenerateRequest(request)
	}
	return requests
}

var _ port.MeteredLLM = (*LLM)(nil)
