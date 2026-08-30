package domain_test

import (
	"encoding/json"
	"testing"

	"cpgen/internal/domain"
)

func TestDomainEnumsRejectUnknownJSONValues(t *testing.T) {
	t.Parallel()
	targets := []any{
		new(domain.CompileOutcome), new(domain.ProcessOutcome), new(domain.ValidatorOutcome),
		new(domain.CheckerOutcome), new(domain.SolutionVerdict), new(domain.FailureRouteClass),
		new(domain.ExecutionCause), new(domain.ArtifactRole), new(domain.DispatchKind),
		new(domain.FailureClass), new(domain.PortFailureCode),
	}
	for _, target := range targets {
		if err := json.Unmarshal([]byte(`"NOT_A_REAL_VALUE"`), target); err == nil {
			t.Fatalf("%T accepted an unknown enum value", target)
		}
	}
}

func TestDomainEnumsAcceptKnownJSONValues(t *testing.T) {
	t.Parallel()
	var outcome domain.ProcessOutcome
	if err := json.Unmarshal([]byte(`"TLE"`), &outcome); err != nil {
		t.Fatalf("decode known outcome: %v", err)
	}
	if outcome != domain.ProcessTLE {
		t.Fatalf("got %q, want TLE", outcome)
	}
}
