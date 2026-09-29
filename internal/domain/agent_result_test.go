package domain_test

import (
	"testing"

	"cpgen/internal/domain"
)

func TestAgentResultAllowsExactlyOneOutcome(t *testing.T) {
	value := domain.FakePrepared{Digest: domain.SumBytes([]byte("prepared"))}
	result := domain.Success(value)
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	result.Retryable = &domain.RetryableFailure{Code: domain.FailureUnavailable}
	if err := result.Validate(); err == nil {
		t.Fatal("result with two outcomes was accepted")
	}
}
