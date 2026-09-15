package workflow

import (
	"testing"

	"cpgen/internal/domain"
)

func TestContentRetryRoutesAndExclusions(t *testing.T) {
	for _, test := range []struct {
		stage  domain.StageName
		reason string
		target domain.StageName
	}{
		{"idea", "idea_binding_rejected", "idea"},
		{"idea", "no_feasible_idea_candidates", "idea"},
		{"statement", "llm_format_rejected", "statement"},
		{"solution", "solution_binding_rejected", "solution"},
		{"data", "data_binding_rejected", "data"},
		{"solution_decision", "solution_requires_review:compile.SOLUTION.CE", "solution"},
		{"solution_decision", "solution_requires_review:sample.2.SOLUTION.WA", "statement"},
		{"judge", "data_requires_review:generated.1.nondeterministic", "data"},
		{"judge", "data_requires_review:sample.1.INVALID", "data"},
		{"quality", "judge_requires_review:tests/001.in:differential.WA", "solution"},
		{"idea", "llm_budget_exhausted", ""},
		{"idea", "llm_boundary_unknown", ""},
		{"idea", "llm_content_rejected", ""},
		{"idea", "active_time_exhausted", ""},
		{"similarity_decision", "similarity_requires_review:REJECT", ""},
		{"solution_decision", "solution_requires_review:unrecognized", ""},
		{"solution_decision", "solution_requires_review:compile.SOLUTION.INFRA_ERROR", ""},
		{"judge", "data_requires_review:generated.1.unrecognized", ""},
		{"quality", "judge_requires_review:tests/001.in:reference.INFRA_ERROR", ""},
		{"package", "quality_requires_review:checker.compile.COMPILE_ERROR", ""},
	} {
		if got := ContentRetryTarget(RetryingGenerationRevision, test.stage, test.reason); got != test.target {
			t.Errorf("%s %s: %s, want %s", test.stage, test.reason, got, test.target)
		}
		if got := ContentRetryTarget(GenerationRevision, test.stage, test.reason); got != "" {
			t.Errorf("old revision retried %s", test.reason)
		}
		if test.target != "" && !AllowsContentRetryTransition(RetryingGenerationRevision, test.stage, test.target) {
			t.Errorf("scheduler rejects content retry %s -> %s", test.stage, test.target)
		}
	}
}
