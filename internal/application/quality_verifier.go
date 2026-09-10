package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

type QualityVerificationResult struct {
	Report         QualityReport
	ReportArtifact domain.PendingArtifact
	Occurrences    []domain.PendingOccurrence
}

type QualityVerifier struct{ config SolutionVerifierConfig }

func NewQualityVerifier(config SolutionVerifierConfig) (*QualityVerifier, error) {
	if config.Sandbox == nil || config.Publisher == nil || config.Blobs == nil {
		return nil, errors.New("quality requires sandbox and artifact capabilities")
	}
	raw, err := config.Lock.MarshalIndent()
	if err != nil {
		return nil, err
	}
	config.Lock, err = toolchain.LoadLock(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return &QualityVerifier{config}, nil
}

// Quality compiles the fixed checker and evaluates its contract and every
// judged case. Upstream gates are summarized only after their committed proof
// has been reconstructed by QualityExecutor.
func (s *QualityVerifier) Verify(ctx context.Context, runID domain.RunID, input QualityInput) (QualityVerificationResult, error) {
	config := s.config
	var empty QualityVerificationResult
	if err := input.Validate(); err != nil {
		return empty, err
	}
	if err := runID.Validate(); err != nil {
		return empty, err
	}
	if config.Sandbox == nil || config.Publisher == nil || config.Blobs == nil || config.Publisher.identity.RunID != runID {
		return empty, errors.New("quality requires capabilities for its exact run")
	}
	lockDigest, err := config.Lock.Digest()
	if err != nil || lockDigest != input.JudgeReport.ToolchainLockDigest {
		return empty, errors.New("quality changed the frozen toolchain")
	}
	_, filename, media, compiler, err := solutionCompiler("cpp", config.Lock)
	if err != nil {
		return empty, err
	}
	digest, err := stableValueDigest(input.JudgeReport)
	if err != nil {
		return empty, err
	}
	i := input.JudgeInput
	report := QualityReport{SchemaVersion: qualitySchema, RunID: runID, InputDigest: digest, ProblemSpecDigest: i.Data.ProblemSpecDigest, SimilarityDecisionDigest: i.DataInput.SolutionInput.SimilarityDecisionDigest, SolutionVerificationDigest: i.DataInput.SolutionVerificationDigest, DataVerificationDigest: input.JudgeReport.InputDigest, ToolchainLockDigest: lockDigest, PolicyDigest: qualityPolicyDigest(lockDigest), Coverage: qualityCoverage(input), Canaries: []QualityCheckerEvidence{}, Cases: []QualityCheckerEvidence{}}
	var files []domain.PendingArtifact
	publish := func(path string, role domain.ArtifactRole, media string, raw []byte) (domain.PendingArtifact, error) {
		p, err := config.Publisher.Publish(ctx, port.ArtifactDeclaration{LogicalPath: domain.SafeRelPath(path), Role: role, MediaType: media, MaxBytes: int64(max(1, len(raw))), Provenance: domain.ProvenanceCandidate{SchemaVersion: qualitySchema, Producer: "quality-verifier", InputDigest: &digest}}, raw)
		if err == nil {
			files = append(files, p)
		}
		return p, err
	}
	finish := func() (QualityVerificationResult, error) {
		if err := report.ValidateFor(runID, input); err != nil {
			return empty, err
		}
		raw, err := json.Marshal(report)
		if err != nil {
			return empty, err
		}
		if len(raw) > 1<<20 {
			return empty, errors.New("quality report exceeds byte bound")
		}
		artifact, err := publish("quality/report.json", domain.ArtifactOutput, "application/vnd.cpgen.quality+json", raw)
		if err != nil {
			return empty, err
		}
		result := QualityVerificationResult{Report: report, ReportArtifact: artifact}
		seen := make(map[domain.ArtifactWriterTokenID]bool)
		for _, p := range append(files, config.Sandbox.Artifacts()...) {
			if !seen[p.WriterTokenID] {
				pending := p
				result.Occurrences = append(result.Occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending})
				seen[p.WriterTokenID] = true
			}
		}
		return result, nil
	}
	source, err := publish("quality/checker/"+filename, domain.ArtifactSource, media, judge.ExactTokenCheckerSource())
	if err != nil {
		return empty, err
	}
	bundle := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: domain.SafeRelPath(filename), Files: []port.SourceFile{{Path: domain.SafeRelPath(filename), Blob: source.Blob}}}
	bundle.Digest, err = port.ComputeSourceBundleDigest(bundle)
	if err != nil {
		return empty, err
	}
	compiled, err := config.Sandbox.Compile(ctx, port.CompileRequest{Language: port.LanguageCPP20, Role: port.RoleChecker, SourceBundle: bundle, Toolchain: compiler.ID, Limits: solutionCompileLimits(), ExpectedOutput: compiler.OutputPath})
	if err != nil {
		return empty, err
	}
	if err := compiled.Validate(); err != nil {
		return empty, err
	}
	if compiled.Failure != nil || !compiled.CallTrace.Equal(compiled.Value.CallTrace) {
		return empty, errors.New("quality checker compile lacks its matching outcome")
	}
	if err := compiled.Value.Validate(); err != nil {
		return empty, err
	}
	report.Checker = SolutionCompileEvidence{Role: port.RoleChecker, Source: source.Blob, SourceBundleDigest: bundle.Digest, Result: *compiled.Value}
	if compiled.Value.Outcome == domain.CompileInfraError {
		return empty, errors.New("quality checker compile infrastructure failure")
	}
	if compiled.Value.Outcome != domain.CompileOK {
		report.Reason = "checker.compile." + string(compiled.Value.Outcome)
		return finish()
	}
	run := func(item QualityCheckerEvidence) (QualityCheckerEvidence, error) {
		outcome, err := config.Sandbox.Run(ctx, qualityCheckerRequest(compiled.Value.Program.Blob, item.Input, item.Candidate, item.Answer))
		if err != nil {
			return item, err
		}
		if err := outcome.Validate(); err != nil {
			return item, err
		}
		if outcome.Failure != nil || !outcome.CallTrace.Equal(outcome.Value.CallTrace) {
			return item, errors.New("quality checker run lacks its matching outcome")
		}
		item.Result = *outcome.Value
		return item, nil
	}
	for _, canary := range qualityCanaries() {
		refs := make([]domain.BlobRef, 3)
		for index, raw := range []string{canary.Input, canary.Candidate, canary.Answer} {
			p, err := publish(fmt.Sprintf("quality/canaries/%s/%s", canary.Name, []string{"input.txt", "output.txt", "answer.txt"}[index]), domain.ArtifactInput, "text/plain", []byte(raw))
			if err != nil {
				return empty, err
			}
			refs[index] = p.Blob
		}
		check, err := run(QualityCheckerEvidence{Name: canary.Name, Input: refs[0], Candidate: refs[1], Answer: refs[2], Expected: canary.Expected})
		if err != nil {
			return empty, err
		}
		report.Canaries = append(report.Canaries, check)
		failure, err := qualityCheckerFailure(check)
		if err != nil {
			return empty, err
		}
		if failure != "" {
			report.Reason = failure
			return finish()
		}
	}
	for _, item := range input.JudgeReport.Cases {
		check, err := run(qualityCase(item))
		if err != nil {
			return empty, err
		}
		report.Cases = append(report.Cases, check)
		failure, err := qualityCheckerFailure(check)
		if err != nil {
			return empty, err
		}
		if failure != "" {
			report.Reason = failure
			return finish()
		}
	}
	report.Passed = true
	return finish()
}
