package application

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/port"
)

const qualitySchema = "cpgen.quality/v1"

type QualityInput struct {
	JudgeInput  JudgeInput
	JudgeReport JudgeVerificationReport
}

func (v QualityInput) Validate() error {
	if err := v.JudgeReport.ValidateFor(v.JudgeInput); err != nil {
		return err
	}
	if !v.JudgeReport.Passed {
		return errors.New("Quality requires a passing committed Judge")
	}
	return nil
}

type QualityCoverage struct {
	SolutionCompiles  int `json:"solution_compiles"`
	SampleRuns        int `json:"sample_runs"`
	DataCompiles      int `json:"data_compiles"`
	ReproducibleCases int `json:"reproducible_cases"`
	ValidatedInputs   int `json:"validated_inputs"`
	ReferenceRuns     int `json:"reference_runs"`
	DifferentialRuns  int `json:"differential_runs"`
}

func qualityCoverage(v QualityInput) QualityCoverage {
	i := v.JudgeInput
	c := QualityCoverage{SolutionCompiles: len(i.SolutionReport.Compiles), SampleRuns: len(i.SolutionReport.Samples), DataCompiles: len(i.DataReport.Compiles), ReproducibleCases: len(i.DataReport.Generated), ValidatedInputs: len(i.DataReport.Samples) + len(i.DataReport.Generated), ReferenceRuns: len(v.JudgeReport.Cases)}
	for _, item := range v.JudgeReport.Cases {
		if item.Brute != nil {
			c.DifferentialRuns++
		}
	}
	return c
}

type qualityCanary struct {
	Name, Input, Candidate, Answer string
	Expected                       domain.CheckerOutcome
}

func qualityCanaries() []qualityCanary {
	return []qualityCanary{{"ac", "checker canary\n", " \tx\u0085y\u3000z\n", "x y z\n", domain.CheckerAC}, {"wa", "checker canary\n", "x WRONG z\n", "x y z\n", domain.CheckerWA}}
}

func qualityCheckerLimits() port.RunLimits {
	return port.RunLimits{Time: 2 * time.Second, MemoryBytes: 256 << 20, PIDs: 64, StdoutBytes: 4096, StderrBytes: 4096}
}

func qualityPolicyDigest(lock domain.Digest) domain.Digest {
	raw, _ := json.Marshal(struct {
		Schema, Comparison  string
		Lock, CheckerSource domain.Digest
		Compile             port.CompileLimits
		Run                 port.RunLimits
		Canaries            []qualityCanary
	}{qualitySchema, judge.ExactTokenComparisonV1, lock, domain.SumBytes(judge.ExactTokenCheckerSource()), solutionCompileLimits(), qualityCheckerLimits(), qualityCanaries()})
	return domain.SumBytes(raw)
}

func qualityCheckerRequest(program, input, candidate, answer domain.BlobRef) port.RunRequest {
	return port.RunRequest{Role: port.RoleChecker, Program: program, Files: []port.InputMount{{Path: "input.txt", Blob: input}, {Path: "output.txt", Blob: candidate}, {Path: "answer.txt", Blob: answer}}, Limits: qualityCheckerLimits()}
}

type QualityCheckerEvidence struct {
	Name      string                `json:"name"`
	Input     domain.BlobRef        `json:"input"`
	Candidate domain.BlobRef        `json:"candidate"`
	Answer    domain.BlobRef        `json:"answer"`
	Expected  domain.CheckerOutcome `json:"expected"`
	Result    port.RunResult        `json:"result"`
}

type QualityReport struct {
	SchemaVersion              string                   `json:"schema_version"`
	RunID                      domain.RunID             `json:"run_id"`
	InputDigest                domain.Digest            `json:"input_digest"`
	ProblemSpecDigest          domain.Digest            `json:"problem_spec_digest"`
	SimilarityDecisionDigest   domain.Digest            `json:"similarity_decision_digest"`
	SolutionVerificationDigest domain.Digest            `json:"solution_verification_digest"`
	DataVerificationDigest     domain.Digest            `json:"data_verification_digest"`
	ToolchainLockDigest        domain.Digest            `json:"toolchain_lock_digest"`
	PolicyDigest               domain.Digest            `json:"policy_digest"`
	Coverage                   QualityCoverage          `json:"coverage"`
	Checker                    SolutionCompileEvidence  `json:"checker"`
	Canaries                   []QualityCheckerEvidence `json:"canaries"`
	Cases                      []QualityCheckerEvidence `json:"cases"`
	Passed                     bool                     `json:"passed"`
	Reason                     string                   `json:"reason,omitempty"`
}

func qualityCheckerFailure(item QualityCheckerEvidence) (string, error) {
	adapted, err := judge.AdaptChecker(item.Result, judge.DefaultTestlibV1)
	if err != nil {
		return "", err
	}
	if adapted.InfrastructureFailure || adapted.Outcome == nil {
		return "", errors.New("checker infrastructure failure cannot form a quality verdict")
	}
	if *adapted.Outcome != item.Expected {
		return item.Name + ":" + string(*adapted.Outcome), nil
	}
	if item.Result.Stdout == nil || item.Result.Stdout.Blob.Size != 0 {
		return "", errors.New("fixed checker lacks empty stdout evidence")
	}
	return "", nil
}

func qualityBlob(raw string) domain.BlobRef {
	return domain.BlobRef{Digest: domain.SumBytes([]byte(raw)), Size: int64(len(raw))}
}

func qualityCase(v JudgeCaseEvidence) QualityCheckerEvidence {
	candidate := v.Reference.Stdout.Blob
	if v.Brute != nil {
		candidate = v.Brute.Stdout.Blob
	}
	return QualityCheckerEvidence{Name: string(v.Input.Path), Input: v.Input.Input, Candidate: candidate, Answer: *v.Answer, Expected: domain.CheckerAC}
}

func (r QualityReport) ValidateFor(runID domain.RunID, v QualityInput) error {
	if err := runID.Validate(); err != nil {
		return err
	}
	if err := v.Validate(); err != nil {
		return err
	}
	digest, err := stableValueDigest(v.JudgeReport)
	if err != nil {
		return err
	}
	i := v.JudgeInput
	if r.SchemaVersion != qualitySchema || r.RunID != runID || r.InputDigest != digest || r.ProblemSpecDigest != i.Data.ProblemSpecDigest || r.SimilarityDecisionDigest != i.DataInput.SolutionInput.SimilarityDecisionDigest || r.SolutionVerificationDigest != i.DataInput.SolutionVerificationDigest || r.DataVerificationDigest != v.JudgeReport.InputDigest || r.ToolchainLockDigest != i.DataReport.ToolchainLockDigest || r.PolicyDigest != qualityPolicyDigest(r.ToolchainLockDigest) || r.Coverage != qualityCoverage(v) {
		return errors.New("quality report changed its run, upstream proof or policy")
	}
	code := judge.ExactTokenCheckerSource()
	source := domain.BlobRef{Digest: domain.SumBytes(code), Size: int64(len(code))}
	bundle := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: "main.cpp", Files: []port.SourceFile{{Path: "main.cpp", Blob: source}}}
	bundleDigest, err := port.ComputeSourceBundleDigest(bundle)
	if err != nil {
		return err
	}
	if r.Checker.Role != port.RoleChecker || r.Checker.Source != source || r.Checker.SourceBundleDigest != bundleDigest {
		return errors.New("quality checker differs from the fixed source")
	}
	if err := r.Checker.Result.Validate(); err != nil {
		return err
	}
	failure := ""
	if r.Checker.Result.Outcome == domain.CompileInfraError {
		return errors.New("quality compile has an infrastructure failure")
	}
	if r.Checker.Result.Outcome != domain.CompileOK {
		failure = "checker.compile." + string(r.Checker.Result.Outcome)
	}
	canaries := qualityCanaries()
	if len(r.Canaries) > len(canaries) || len(r.Cases) > len(v.JudgeReport.Cases) {
		return errors.New("quality report has extra checker results")
	}
	for index, item := range r.Canaries {
		expected := canaries[index]
		if failure != "" || item.Name != expected.Name || item.Input != qualityBlob(expected.Input) || item.Candidate != qualityBlob(expected.Candidate) || item.Answer != qualityBlob(expected.Answer) || item.Expected != expected.Expected {
			return errors.New("quality checker canary changed or followed a failure")
		}
		failure, err = qualityCheckerFailure(item)
		if err != nil {
			return err
		}
	}
	for index, item := range r.Cases {
		expected := qualityCase(v.JudgeReport.Cases[index])
		if failure != "" || len(r.Canaries) != len(canaries) || item.Name != expected.Name || item.Input != expected.Input || item.Candidate != expected.Candidate || item.Answer != expected.Answer || item.Expected != expected.Expected {
			return errors.New("quality case differs from current Judge evidence")
		}
		failure, err = qualityCheckerFailure(item)
		if err != nil {
			return err
		}
	}
	if r.Passed {
		if failure != "" || r.Reason != "" || len(r.Canaries) != len(canaries) || len(r.Cases) != len(v.JudgeReport.Cases) {
			return errors.New("passing quality report omits required checks")
		}
	} else if failure == "" || r.Reason != failure {
		return fmt.Errorf("quality report lacks its exact first failure: %s", failure)
	}
	return nil
}
