package similarity

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"cpgen/internal/domain"
)

// Fake is a deterministic, provider-free searcher for stage tests. Outcomes
// are consumed in queue order and requests are retained as defensive copies;
// it never performs network I/O or reads credentials.
type Fake struct {
	mu       sync.Mutex
	outcomes []EvidenceOutcome
	requests []Request
}

func NewFake(outcomes ...EvidenceOutcome) *Fake {
	return &Fake{outcomes: append([]EvidenceOutcome(nil), outcomes...)}
}

func NewFakeEvidence(values ...Evidence) *Fake {
	outcomes := make([]EvidenceOutcome, 0, len(values))
	for i := range values {
		value := values[i]
		outcomes = append(outcomes, EvidenceOutcome{Value: &value, CallTrace: value.CallTrace})
	}
	return NewFake(outcomes...)
}

func (f *Fake) SearchEvidence(ctx context.Context, request Request) (EvidenceOutcome, error) {
	var empty EvidenceOutcome
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := request.Validate(); err != nil {
		return empty, fmt.Errorf("fake similarity request: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, cloneRequest(request))
	if len(f.outcomes) == 0 {
		return empty, errors.New("fake similarity has no queued outcome")
	}
	outcome := f.outcomes[0]
	f.outcomes = f.outcomes[1:]
	if err := outcome.Validate(); err != nil {
		return empty, fmt.Errorf("fake similarity outcome: %w", err)
	}
	if outcome.Value != nil {
		if outcome.Value.RequestDigest != mustDigest(request) {
			return empty, errors.New("fake similarity evidence is bound to another request")
		}
		if outcome.Value.PolicyDigest != request.PolicyDigest {
			return empty, errors.New("fake similarity evidence is bound to another policy")
		}
	}
	return cloneOutcome(outcome), nil
}

func (f *Fake) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]Request, len(f.requests))
	for i := range f.requests {
		result[i] = cloneRequest(f.requests[i])
	}
	return result
}

func cloneRequest(request Request) Request {
	request.NormalizedTags = append([]string(nil), request.NormalizedTags...)
	return request
}

func cloneOutcome(outcome EvidenceOutcome) EvidenceOutcome {
	result := outcome
	if outcome.Failure != nil {
		failure := *outcome.Failure
		failure.Evidence = append([]domain.PendingArtifact(nil), outcome.Failure.Evidence...)
		result.Failure = &failure
	}
	if outcome.Value != nil {
		value := *outcome.Value
		value.Hits = append([]Hit(nil), outcome.Value.Hits...)
		value.Cache.SourceOccurrenceIDs = append([]domain.ArtifactOccurrenceID(nil), outcome.Value.Cache.SourceOccurrenceIDs...)
		value.CallTrace.PhysicalAttemptCallIDs = append([]domain.AttemptCallID(nil), outcome.Value.CallTrace.PhysicalAttemptCallIDs...)
		result.Value = &value
	}
	return result
}
