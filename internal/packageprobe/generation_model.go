package packageprobe

import (
	"fmt"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
)

// These package-safe documents describe bindings; reading their claims does
// not authorize READY. The application must prove the current run's reports.
type GenerationQualityDocument struct {
	SchemaVersion              domain.SchemaVersion `json:"schema_version"`
	RunID                      domain.RunID         `json:"run_id"`
	Profile                    string               `json:"profile"`
	ProblemSpecRevision        int64                `json:"problem_spec_revision"`
	Status                     string               `json:"status"`
	QualityReportDigest        domain.Digest        `json:"quality_report_digest"`
	JudgeReportDigest          domain.Digest        `json:"judge_report_digest"`
	DataVerificationDigest     domain.Digest        `json:"data_verification_digest"`
	SolutionVerificationDigest domain.Digest        `json:"solution_verification_digest"`
	ProblemSpecDigest          domain.Digest        `json:"problem_spec_digest"`
	SimilarityDecisionDigest   domain.Digest        `json:"similarity_decision_digest"`
	PolicyDigest               domain.Digest        `json:"policy_digest"`
	CheckerSourceDigest        domain.Digest        `json:"checker_source_digest"`
	TestsDigest                domain.Digest        `json:"tests_digest"`
}

type GenerationProvenance struct {
	SchemaVersion       domain.SchemaVersion `json:"schema_version"`
	RunID               domain.RunID         `json:"run_id"`
	Source              string               `json:"source"`
	WorkflowRevision    string               `json:"workflow_revision"`
	WorkflowDigest      domain.Digest        `json:"workflow_digest"`
	RequestDigest       domain.Digest        `json:"request_digest"`
	ConfigDigest        domain.Digest        `json:"config_digest"`
	ProblemSpecDigest   domain.Digest        `json:"problem_spec_digest"`
	QualityReportDigest domain.Digest        `json:"quality_report_digest"`
	ToolchainLockDigest domain.Digest        `json:"toolchain_lock_digest"`
}

type GenerationTest struct {
	ID      string              `json:"id"`
	Origin  string              `json:"origin"`
	Ordinal int                 `json:"ordinal"`
	Kind    domain.DataCaseKind `json:"kind,omitempty"`
	Seed    *uint64             `json:"seed,omitempty"`
	Input   domain.BlobRef      `json:"input"`
	Answer  domain.BlobRef      `json:"answer"`
}

type GenerationTestsDocument struct {
	SchemaVersion     domain.SchemaVersion `json:"schema_version"`
	DataContentDigest domain.Digest        `json:"data_content_digest"`
	JudgeReportDigest domain.Digest        `json:"judge_report_digest"`
	Plan              domain.TestPlanV1    `json:"plan"`
	Tests             []GenerationTest     `json:"tests"`
}

func validateGenerationDocuments(files map[domain.SafeRelPath][]byte, runID domain.RunID, revision int64, profile string, similarity SimilarityReport, toolchain domain.Digest) error {
	var quality GenerationQualityDocument
	if err := decodeStrict(files["reports/prepackage-quality.json"], &quality); err != nil {
		return err
	}
	if quality.SchemaVersion != "cpgen.prepackage-report/v2" || quality.RunID != runID || quality.Profile != profile || quality.ProblemSpecRevision != revision || quality.Status != "PASSED" || quality.SimilarityDecisionDigest != similarity.DecisionDigest {
		return fmt.Errorf("generation quality report changed its run or acceptance")
	}
	for _, digest := range []domain.Digest{quality.QualityReportDigest, quality.JudgeReportDigest, quality.DataVerificationDigest, quality.SolutionVerificationDigest, quality.ProblemSpecDigest, quality.PolicyDigest, quality.CheckerSourceDigest, quality.TestsDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	if quality.CheckerSourceDigest != domain.SumBytes(files["judge/checker.cpp"]) || quality.CheckerSourceDigest != domain.SumBytes(judge.ExactTokenCheckerSource()) || quality.TestsDigest != domain.SumBytes(files["data/tests.json"]) {
		return fmt.Errorf("generation quality report changed its checker or tests")
	}
	var provenance GenerationProvenance
	if err := decodeStrict(files["reports/provenance.json"], &provenance); err != nil {
		return err
	}
	if provenance.SchemaVersion != "cpgen.provenance/v2" || provenance.RunID != runID || provenance.Source != "mvp-generation" || provenance.WorkflowRevision == "" || len(provenance.WorkflowRevision) > 200 || provenance.WorkflowDigest != domain.SumBytes([]byte(provenance.WorkflowRevision)) || provenance.QualityReportDigest != quality.QualityReportDigest || provenance.ProblemSpecDigest != quality.ProblemSpecDigest {
		return fmt.Errorf("generation provenance changed its run or quality binding")
	}
	for _, digest := range []domain.Digest{provenance.RequestDigest, provenance.ConfigDigest, provenance.ToolchainLockDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	if provenance.ToolchainLockDigest != toolchain {
		return fmt.Errorf("package provenance changed the manifest toolchain")
	}
	var tests GenerationTestsDocument
	if err := decodeStrict(files["data/tests.json"], &tests); err != nil {
		return err
	}
	if tests.SchemaVersion != "cpgen.package-tests/v1" || tests.DataContentDigest.Validate() != nil || tests.JudgeReportDigest != quality.JudgeReportDigest {
		return fmt.Errorf("generation test index lost its Judge binding")
	}
	if err := tests.Plan.Validate(); err != nil {
		return err
	}
	var samples SamplesDocument
	if err := decodeStrict(files["statement/samples.json"], &samples); err != nil {
		return err
	}
	if len(tests.Tests) != len(samples.Samples)+len(tests.Plan.Cases) {
		return fmt.Errorf("generation test index omitted required cases")
	}
	seen := make(map[domain.SafeRelPath]bool)
	for index, test := range tests.Tests {
		if index < len(samples.Samples) {
			if test.Origin != "sample" || test.Ordinal != index+1 || test.Kind != "" || test.Seed != nil {
				return fmt.Errorf("generation sample identity differs")
			}
		} else {
			item := tests.Plan.Cases[index-len(samples.Samples)]
			if test.Origin != "generated" || test.Ordinal != item.Ordinal || test.Kind != item.Kind || test.Seed == nil || *test.Seed != item.Seed {
				return fmt.Errorf("generated case changed its frozen seed or plan")
			}
		}
		if test.ID != fmt.Sprintf("%s-%03d", test.Origin, test.Ordinal) {
			return fmt.Errorf("generation case path identity differs")
		}
		inputPath := domain.SafeRelPath("tests/" + test.ID + ".in")
		answerPath := domain.SafeRelPath("tests/" + test.ID + ".ans")
		for path, ref := range map[domain.SafeRelPath]domain.BlobRef{inputPath: test.Input, answerPath: test.Answer} {
			raw, found := files[path]
			if !found || ref.Validate() != nil || ref.Digest != domain.SumBytes(raw) || ref.Size != int64(len(raw)) {
				return fmt.Errorf("generation test bytes differ: %s", path)
			}
			seen[path] = true
		}
		if test.Origin == "sample" {
			sample := samples.Samples[test.Ordinal-1]
			if string(files[inputPath]) != sample.Input || judge.ExactTokenDigest(files[answerPath]) != judge.ExactTokenDigest([]byte(sample.Output)) {
				return fmt.Errorf("package samples differ from indexed test bytes")
			}
		}
	}
	for path := range files {
		if testPathPattern.MatchString(string(path)) && !seen[path] {
			return fmt.Errorf("generation package contains an unindexed test")
		}
	}
	return nil
}
