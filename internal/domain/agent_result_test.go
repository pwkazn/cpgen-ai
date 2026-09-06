package domain_test

import (
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestAgentResultAllowsExactlyOneOutcome(t *testing.T) {
	value := domain.Slice1Prepared{Digest: domain.SumBytes([]byte("prepared"))}
	result := domain.Success(value)
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	result.Retryable = &domain.RetryableFailure{Code: domain.FailureUnavailable}
	if err := result.Validate(); err == nil {
		t.Fatal("result with two outcomes was accepted")
	}
}

func TestAgentResultConstructorsCoverTypedOutcomes(t *testing.T) {
	checkpoint := domain.BlockedCheckpoint{
		RunID: domain.RunID("run_0123456789abcdef0123456789abcdef"), StageName: "prepare",
		StageInputDigest: domain.SumBytes([]byte("input")), DependencyID: "provider",
		DependencyDigest: domain.SumBytes([]byte("dependency")), PolicyDigest: domain.SumBytes([]byte("policy")),
		ErrorDigest: domain.SumBytes([]byte("error")), RetryAfter: testTime(), CreatedAt: testTime(),
	}
	if err := domain.Blocked[domain.Slice1Prepared](checkpoint).Validate(); err != nil {
		t.Fatal(err)
	}
}

func testTime() (result time.Time) { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
