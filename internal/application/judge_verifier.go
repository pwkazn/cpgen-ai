package application

import (
	"context"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/port"
)

type JudgeVerifierConfig struct {
	Sandbox             SolutionSandbox
	Publisher           *SandboxArtifactSink
	Blobs               port.VerifiedBlobReader
	ToolchainLockDigest domain.Digest
}

type JudgeVerifier struct{ config JudgeVerifierConfig }

type JudgeVerificationResult struct {
	Report          JudgeVerificationReport
	ReportArtifact  domain.PendingArtifact
	DatasetArtifact *domain.PendingArtifact
	Occurrences     []domain.PendingOccurrence
}

func NewJudgeVerifier(config JudgeVerifierConfig) (*JudgeVerifier, error) {
	if config.Sandbox == nil || config.Publisher == nil || config.Blobs == nil || config.ToolchainLockDigest.Validate() != nil {
		return nil, errors.New("Judge requires its sandbox, toolchain and artifact capabilities")
	}
	return &JudgeVerifier{config}, nil
}

func (s *JudgeVerifier) Verify(ctx context.Context, input JudgeInput) (JudgeVerificationResult, error) {
	var empty JudgeVerificationResult
	dataset, err := input.dataset()
	if err != nil {
		return empty, err
	}
	if s.config.ToolchainLockDigest != input.DataReport.ToolchainLockDigest {
		return empty, errors.New("Judge changed the frozen toolchain")
	}
	report := JudgeVerificationReport{SchemaVersion: judgeVerificationSchema, InputDigest: dataset.VerificationReportDigest, SolutionVerificationDigest: input.DataInput.SolutionVerificationDigest, ToolchainLockDigest: s.config.ToolchainLockDigest, PolicyDigest: judgePolicyDigest(input), Comparison: solutionSampleComparison, Cases: []JudgeCaseEvidence{}}
	var files []domain.PendingArtifact
	publish := func(path domain.SafeRelPath, media string, raw []byte) (domain.PendingArtifact, error) {
		pending, err := s.config.Publisher.Publish(ctx, port.ArtifactDeclaration{LogicalPath: path, Role: domain.ArtifactOutput, MediaType: media, MaxBytes: int64(max(1, len(raw))), Provenance: domain.ProvenanceCandidate{SchemaVersion: judgeVerificationSchema, Producer: "judge-verifier", InputDigest: &report.InputDigest}}, raw)
		if err == nil {
			files = append(files, pending)
		}
		return pending, err
	}
	finish := func() (JudgeVerificationResult, error) {
		if err := report.ValidateFor(input); err != nil {
			return empty, err
		}
		raw, err := json.Marshal(report)
		if err != nil {
			return empty, err
		}
		if len(raw) > 1<<20 {
			return empty, errors.New("Judge report exceeds byte bound")
		}
		artifact, err := publish("judge/verification.json", "application/vnd.cpgen.judge-verification+json", raw)
		if err != nil {
			return empty, err
		}
		result := JudgeVerificationResult{Report: report, ReportArtifact: artifact}
		if report.Passed {
			manifest, err := report.Dataset(input)
			if err != nil {
				return empty, err
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				return empty, err
			}
			pending, err := publish("judge/dataset.json", "application/vnd.cpgen.judged-dataset+json", raw)
			if err != nil {
				return empty, err
			}
			result.DatasetArtifact = &pending
		}
		all := append(files, s.config.Sandbox.Artifacts()...)
		seen := make(map[domain.ArtifactWriterTokenID]bool)
		for _, p := range all {
			if !seen[p.WriterTokenID] {
				pending := p
				result.Occurrences = append(result.Occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending})
				seen[p.WriterTokenID] = true
			}
		}
		return result, nil
	}
	run := func(item DatasetInput, role port.ProgramRole) (port.RunResult, domain.Digest, error) {
		outcome, err := s.config.Sandbox.Run(ctx, judgeRunRequest(input, item.Input, role))
		if err != nil {
			return port.RunResult{}, "", err
		}
		if err := outcome.Validate(); err != nil {
			return port.RunResult{}, "", err
		}
		if outcome.Failure != nil || !outcome.CallTrace.Equal(outcome.Value.CallTrace) {
			return port.RunResult{}, "", errors.New("Judge run lacks a matching completed outcome")
		}
		result := *outcome.Value
		adapted, err := judge.AdaptSolution(result)
		if err != nil || adapted.InfrastructureFailure || adapted.Outcome == nil {
			return port.RunResult{}, "", errors.New("Judge run has no process verdict")
		}
		var token domain.Digest
		if *adapted.Outcome == domain.SolutionOK {
			if result.Stdout == nil {
				return port.RunResult{}, "", errors.New("Judge run lacks stdout")
			}
			raw, err := readSolutionVerificationBlob(ctx, s.config.Blobs, result.Stdout.Blob, 1<<20)
			if err != nil {
				return port.RunResult{}, "", err
			}
			token = solutionTokenDigest(raw)
		}
		return result, token, nil
	}
	for _, item := range dataset.Inputs {
		reference, token, err := run(item, port.RoleSolution)
		if err != nil {
			return empty, err
		}
		check := JudgeCaseEvidence{Input: item, Reference: reference, ReferenceTokenDigest: token}
		failure, err := judgeProcessFailure(reference, token)
		if err != nil {
			return empty, err
		}
		if failure == "" && item.Origin == "generated" && item.Kind == domain.DataCaseSmall {
			brute, bruteToken, err := run(item, port.RoleBrute)
			if err != nil {
				return empty, err
			}
			check.Brute, check.BruteTokenDigest = &brute, bruteToken
		}
		failure, err = judgeCaseFailure(input, check)
		if err != nil {
			return empty, err
		}
		if failure != "" {
			report.Cases = append(report.Cases, check)
			report.Reason = judgeCaseReason(check, failure)
			return finish()
		}
		raw, err := readSolutionVerificationBlob(ctx, s.config.Blobs, reference.Stdout.Blob, 1<<20)
		if err != nil {
			return empty, err
		}
		answer, err := publish(domain.SafeRelPath("judge/"+string(judgeAnswerPath(item))), "text/plain", raw)
		if err != nil {
			return empty, err
		}
		check.Answer = &answer.Blob
		report.Cases = append(report.Cases, check)
	}
	report.Passed = true
	return finish()
}
