package fake

import (
	"context"
	"fmt"
	"sync"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type Similarity struct {
	mu       sync.Mutex
	outcomes []domain.MeteredOutcome[port.SimilarityEvidence]
	requests []port.SimilaritySearchRequest
}

func NewSimilarity(outcomes ...domain.MeteredOutcome[port.SimilarityEvidence]) *Similarity {
	return &Similarity{outcomes: append([]domain.MeteredOutcome[port.SimilarityEvidence](nil), outcomes...)}
}

func (f *Similarity) Search(ctx context.Context, request port.SimilaritySearchRequest) (domain.MeteredOutcome[port.SimilarityEvidence], error) {
	if err := ctx.Err(); err != nil {
		return domain.MeteredOutcome[port.SimilarityEvidence]{}, err
	}
	if err := request.Validate(); err != nil {
		return domain.MeteredOutcome[port.SimilarityEvidence]{}, fmt.Errorf("fake similarity request: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, request)
	if len(f.outcomes) == 0 {
		return domain.MeteredOutcome[port.SimilarityEvidence]{}, fmt.Errorf("fake similarity has no queued outcome")
	}
	outcome := f.outcomes[0]
	f.outcomes = f.outcomes[1:]
	if err := outcome.Validate(); err != nil {
		return domain.MeteredOutcome[port.SimilarityEvidence]{}, fmt.Errorf("invalid fake similarity outcome: %w", err)
	}
	if outcome.Value != nil {
		if err := outcome.Value.Validate(); err != nil {
			return domain.MeteredOutcome[port.SimilarityEvidence]{}, fmt.Errorf("invalid fake similarity evidence: %w", err)
		}
		if !outcome.Value.CallTrace.Equal(outcome.CallTrace) {
			return domain.MeteredOutcome[port.SimilarityEvidence]{}, fmt.Errorf("fake similarity value and wrapper call traces differ")
		}
	}
	return outcome, nil
}

func (f *Similarity) Requests() []port.SimilaritySearchRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]port.SimilaritySearchRequest(nil), f.requests...)
}

var _ port.MeteredSimilarity = (*Similarity)(nil)
