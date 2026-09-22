package workflow

import (
	"strings"

	"cpgen/internal/domain"
)

// ContentRetryLimit is shared across the entire run, including resumed runs.
// Transport and JSON format repair retain their separate bounded allowances.
const ContentRetryLimit = 2

// ContentRetryTarget only accepts known content failures. A failed verification
// must regenerate its source and invalidate the suffix before verification runs
// again. The fixed checker and ambiguous similarity decisions require review.
func ContentRetryTarget(revision string, stage domain.StageName, reason string) domain.StageName {
	if revision != RetryingGenerationRevision && revision != ExecutedSamplesRevision {
		return ""
	}
	switch stage {
	case "idea", "statement", "solution", "data":
		if reason == string(stage)+"_binding_rejected" || reason == "llm_format_rejected" || (stage == "idea" && reason == "no_feasible_idea_candidates") {
			return stage
		}
	case "solution_decision":
		if reason == "solution_requires_review:compile.SOLUTION.CE" || reason == "solution_requires_review:compile.BRUTE.CE" {
			return "solution"
		}
		if strings.HasPrefix(reason, "solution_requires_review:sample.") && hasContentVerdict(reason) {
			// Either the sample or the implementation may be wrong. Rebuild both.
			return "statement"
		}
	case "judge":
		if reason == "data_requires_review:compile.GENERATOR.CE" || reason == "data_requires_review:compile.VALIDATOR.CE" {
			return "data"
		}
		for _, prefix := range []string{"data_requires_review:sample.", "data_requires_review:generated."} {
			if strings.HasPrefix(reason, prefix) && (hasContentVerdict(reason) || hasSuffix(reason, ".INVALID", ".VALIDATOR_ERROR", ".validator_stdout_not_empty", ".nondeterministic")) {
				return "data"
			}
		}
	case "quality":
		if strings.HasPrefix(reason, "judge_requires_review:") && hasContentVerdict(reason) {
			return "solution"
		}
	}
	return ""
}

func hasContentVerdict(reason string) bool {
	return hasSuffix(reason, ".WA", ".RE", ".TLE", ".MLE", ".OLE")
}

func hasSuffix(value string, suffixes ...string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}

func AllowsContentRetryTransition(revision string, from, to domain.StageName) bool {
	if revision != RetryingGenerationRevision && revision != ExecutedSamplesRevision {
		return false
	}
	switch from {
	case "idea", "statement", "solution", "data":
		return to == from
	case "solution_decision":
		return to == "solution" || to == "statement"
	case "judge":
		return to == "data"
	case "quality":
		return to == "solution"
	}
	return false
}
