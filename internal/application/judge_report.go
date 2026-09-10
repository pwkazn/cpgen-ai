package application

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/port"
)

const judgeVerificationSchema = "cpgen.judge-verification/v1"

// JudgeInput must be reconstructed from committed upstream stages by the
// application. Structural validation alone does not establish that provenance.
type JudgeInput struct {
	DataInput      domain.DataDraftInputV1
	Data           domain.DataContent
	DataReport     DataVerificationReport
	SolutionReport SolutionVerificationReport
}

func (v JudgeInput) dataset() (DatasetManifest, error) {
	if err := v.SolutionReport.ValidateFor(v.DataInput.SolutionInput, v.DataInput.Solution); err != nil {
		return DatasetManifest{}, err
	}
	digest, err := stableValueDigest(v.SolutionReport)
	if err != nil || !v.SolutionReport.Passed || digest != v.DataInput.SolutionVerificationDigest || v.SolutionReport.ToolchainLockDigest != v.DataReport.ToolchainLockDigest {
		return DatasetManifest{}, errors.New("Judge requires the same passing Solution and Data toolchain")
	}
	return v.DataReport.Dataset(v.DataInput, v.Data)
}

func judgeRunLimits(v JudgeInput) port.RunLimits {
	p := v.DataInput.SolutionInput.Problem
	return port.RunLimits{Time: time.Duration(p.TimeLimitMS) * time.Millisecond, MemoryBytes: p.MemoryLimitMB << 20, PIDs: 64, StdoutBytes: 1 << 20, StderrBytes: 1 << 20}
}

func judgePolicyDigest(v JudgeInput) domain.Digest {
	raw, _ := json.Marshal(struct {
		Schema, Comparison, BruteCases string
		Lock                           domain.Digest
		Limits                         port.RunLimits
	}{judgeVerificationSchema, solutionSampleComparison, "every-generated-small-v1", v.DataReport.ToolchainLockDigest, judgeRunLimits(v)})
	return domain.SumBytes(raw)
}

func judgeRunRequest(v JudgeInput, input domain.BlobRef, role port.ProgramRole) port.RunRequest {
	index := 0
	if role == port.RoleBrute {
		index = 1
	}
	return port.RunRequest{Role: role, Program: v.SolutionReport.Compiles[index].Result.Program.Blob, Stdin: &input, Limits: judgeRunLimits(v)}
}

type JudgeCaseEvidence struct {
	Input                DatasetInput    `json:"input"`
	Reference            port.RunResult  `json:"reference"`
	ReferenceTokenDigest domain.Digest   `json:"reference_token_digest,omitempty"`
	Brute                *port.RunResult `json:"brute,omitempty"`
	BruteTokenDigest     domain.Digest   `json:"brute_token_digest,omitempty"`
	Answer               *domain.BlobRef `json:"answer,omitempty"`
}

type JudgeVerificationReport struct {
	SchemaVersion              string              `json:"schema_version"`
	InputDigest                domain.Digest       `json:"input_digest"`
	SolutionVerificationDigest domain.Digest       `json:"solution_verification_digest"`
	ToolchainLockDigest        domain.Digest       `json:"toolchain_lock_digest"`
	PolicyDigest               domain.Digest       `json:"policy_digest"`
	Comparison                 string              `json:"comparison"`
	Cases                      []JudgeCaseEvidence `json:"cases"`
	Passed                     bool                `json:"passed"`
	Reason                     string              `json:"reason,omitempty"`
}

func judgeProcessFailure(result port.RunResult, token domain.Digest) (string, error) {
	adapted, err := judge.AdaptSolution(result)
	if err != nil {
		return "", err
	}
	if adapted.InfrastructureFailure || adapted.Outcome == nil {
		return "", errors.New("Judge infrastructure failure is not a content verdict")
	}
	if *adapted.Outcome != domain.SolutionOK {
		if token != "" {
			return "", errors.New("failed Judge process claimed output tokens")
		}
		return string(*adapted.Outcome), nil
	}
	if result.Stdout == nil || result.Stdout.Blob.Size > 1<<20 || token.Validate() != nil {
		return "", errors.New("Judge process lacks bounded output and token evidence")
	}
	return "", nil
}

func judgeCaseFailure(v JudgeInput, item JudgeCaseEvidence) (string, error) {
	failure, err := judgeProcessFailure(item.Reference, item.ReferenceTokenDigest)
	if err != nil {
		return "", err
	}
	if failure != "" {
		failure = "reference." + failure
	}
	if failure == "" && item.Input.Origin == "sample" && item.ReferenceTokenDigest != solutionTokenDigest([]byte(v.DataInput.SolutionInput.Problem.Samples[item.Input.Ordinal-1].Output)) {
		failure = "reference.WA"
	}
	needsBrute := failure == "" && item.Input.Origin == "generated" && item.Input.Kind == domain.DataCaseSmall
	if needsBrute {
		if item.Brute == nil || !independentDataRuns(item.Reference.CallTrace, item.Brute.CallTrace) {
			return "", errors.New("Judge small case lacks an independent Brute execution")
		}
		bruteFailure, err := judgeProcessFailure(*item.Brute, item.BruteTokenDigest)
		if err != nil {
			return "", err
		}
		if bruteFailure != "" {
			failure = "brute." + bruteFailure
		} else if item.ReferenceTokenDigest != item.BruteTokenDigest {
			failure = "differential.WA"
		}
	} else if item.Brute != nil || item.BruteTokenDigest != "" {
		return "", errors.New("Judge has an unplanned Brute execution")
	}
	return failure, nil
}

func (r JudgeVerificationReport) ValidateFor(v JudgeInput) error {
	dataset, err := v.dataset()
	if err != nil {
		return err
	}
	if r.SchemaVersion != judgeVerificationSchema || r.InputDigest != dataset.VerificationReportDigest || r.SolutionVerificationDigest != v.DataInput.SolutionVerificationDigest || r.ToolchainLockDigest != v.DataReport.ToolchainLockDigest || r.PolicyDigest != judgePolicyDigest(v) || r.Comparison != solutionSampleComparison {
		return errors.New("Judge report changed its upstream evidence or policy")
	}
	if len(r.Cases) == 0 || len(r.Cases) > len(dataset.Inputs) {
		return errors.New("Judge report lacks bounded ordered cases")
	}
	failure := ""
	for i, item := range r.Cases {
		if failure != "" || !reflect.DeepEqual(item.Input, dataset.Inputs[i]) {
			return errors.New("Judge case changed its input or continued after failure")
		}
		failure, err = judgeCaseFailure(v, item)
		if err != nil {
			return err
		}
		if failure != "" {
			failure = string(item.Input.Path) + ":" + failure
			if item.Answer != nil {
				return errors.New("failed Judge case published an answer")
			}
		} else if item.Answer == nil || *item.Answer != item.Reference.Stdout.Blob {
			return errors.New("Judge answer differs from the successful reference output")
		}
	}
	if r.Passed {
		if failure != "" || r.Reason != "" || len(r.Cases) != len(dataset.Inputs) {
			return errors.New("passing Judge report omits required checks")
		}
	} else if failure == "" || r.Reason != failure {
		return errors.New("Judge report lacks its exact first failure")
	}
	return nil
}

type JudgedCase struct {
	DatasetInput
	AnswerPath domain.SafeRelPath `json:"answer_path"`
	Answer     domain.BlobRef     `json:"answer"`
}

type JudgedDataset struct {
	SchemaVersion           string        `json:"schema_version"`
	DataVerificationDigest  domain.Digest `json:"data_verification_digest"`
	JudgeVerificationDigest domain.Digest `json:"judge_verification_digest"`
	Cases                   []JudgedCase  `json:"cases"`
}

func judgeAnswerPath(input DatasetInput) domain.SafeRelPath {
	return domain.SafeRelPath(strings.TrimSuffix(string(input.Path), ".in") + ".out")
}

func (r JudgeVerificationReport) Dataset(v JudgeInput) (JudgedDataset, error) {
	if err := r.ValidateFor(v); err != nil {
		return JudgedDataset{}, err
	}
	if !r.Passed {
		return JudgedDataset{}, errors.New("failed Judge cannot produce a judged dataset")
	}
	digest, err := stableValueDigest(r)
	if err != nil {
		return JudgedDataset{}, err
	}
	result := JudgedDataset{SchemaVersion: "cpgen.judged-dataset/v1", DataVerificationDigest: r.InputDigest, JudgeVerificationDigest: digest, Cases: make([]JudgedCase, 0, len(r.Cases))}
	for _, item := range r.Cases {
		result.Cases = append(result.Cases, JudgedCase{item.Input, judgeAnswerPath(item.Input), *item.Answer})
	}
	return result, nil
}

func judgeCaseReason(item JudgeCaseEvidence, failure string) string {
	return fmt.Sprintf("%s:%s", item.Input.Path, failure)
}
