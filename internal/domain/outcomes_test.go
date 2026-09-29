package domain_test

import (
	"encoding/json"
	"testing"

	"cpgen/internal/domain"
)

func TestJSONEnumsRejectUnsupportedValues(t *testing.T) {
	t.Parallel()
	targets := []struct {
		name   string
		target any
	}{
		{"compile outcome", new(domain.CompileOutcome)}, {"process outcome", new(domain.ProcessOutcome)},
		{"validator outcome", new(domain.ValidatorOutcome)}, {"checker outcome", new(domain.CheckerOutcome)},
		{"solution verdict", new(domain.SolutionVerdict)}, {"failure route", new(domain.FailureRouteClass)},
		{"execution cause", new(domain.ExecutionCause)}, {"artifact role", new(domain.ArtifactRole)},
		{"dispatch kind", new(domain.DispatchKind)}, {"failure class", new(domain.FailureClass)},
		{"port failure", new(domain.PortFailureCode)}, {"run state", new(domain.RunState)},
		{"stage state", new(domain.StageState)}, {"stage attempt state", new(domain.StageAttemptState)},
		{"review decision kind", new(domain.ReviewDecisionKind)}, {"review decision state", new(domain.ReviewDecisionState)},
	}
	for _, tc := range targets {
		if err := json.Unmarshal([]byte(`"NOT_A_REAL_VALUE"`), tc.target); err == nil {
			t.Errorf("%s accepted an unknown enum value", tc.name)
		}
	}
	for _, raw := range []string{`"quiesce"`, `"lease_lost"`} {
		var cause domain.ExecutionCause
		if err := json.Unmarshal([]byte(raw), &cause); err == nil {
			t.Errorf("obsolete execution cause %s was accepted", raw)
		}
	}
}
