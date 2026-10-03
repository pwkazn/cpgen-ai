package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/port"
)

type QualityReader struct {
	data       *DataReader
	store      SandboxEvidenceReadStore
	blobs      port.VerifiedBlobReader
	sandbox    sandboxexec.ReadPolicy
	similarity CommittedSimilarityReader
}

func (s *QualityReader) ReadInput(ctx context.Context, runID domain.RunID) (QualityInput, error) {
	var empty QualityInput
	input, err := s.data.ReadJudgeInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	report, err := s.data.ReadJudgeVerification(ctx, runID)
	if err != nil {
		return empty, err
	}
	value := QualityInput{JudgeInput: input, JudgeReport: report}
	if err := value.Validate(); err != nil {
		return empty, err
	}
	return value, nil
}

func (s *QualityReader) ReadReport(ctx context.Context, runID domain.RunID) (QualityReport, error) {
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return QualityReport{}, err
	}
	return s.readReportForInput(ctx, runID, input)
}

// readReportForInput reuses the current input already verified by ReadInput.
func (s *QualityReader) readReportForInput(ctx context.Context, runID domain.RunID, input QualityInput) (QualityReport, error) {
	var empty QualityReport
	store := s.store
	digest, err := stableValueDigest(input.JudgeReport)
	if err != nil {
		return empty, err
	}
	stage, err := store.ReadCommittedSandboxStage(ctx, runID, "quality")
	if err != nil {
		return empty, err
	}
	attempt := stage.Attempt
	if attempt.Validate() != nil || attempt.RunID != runID || attempt.StageName != "quality" || attempt.State != domain.StageAttemptSucceeded || attempt.InputDigest != digest || attempt.OutputDigest == nil {
		return empty, errors.New("quality stage differs from current Judge evidence")
	}
	reader, err := newSandboxStageEvidence(ctx, store, s.blobs, stage)
	if err != nil {
		return empty, err
	}
	metadata := func(path domain.SafeRelPath, media string) (port.CommittedPrivateStageArtifact, error) {
		item, found := reader.items[path]
		if !found || item.Blob.MediaType != media || item.Blob.Provenance.SchemaVersion != qualitySchema || item.Blob.Provenance.Producer != "quality-verifier" || item.Blob.Provenance.InputDigest == nil || *item.Blob.Provenance.InputDigest != digest {
			return item, errors.New("quality artifact lost its Judge binding")
		}
		return item, nil
	}
	local := func(path domain.SafeRelPath, raw []byte, role domain.ArtifactRole, media string) error {
		if _, err := metadata(path, media); err != nil {
			return err
		}
		_, err := reader.read(path, domain.BlobRef{Digest: domain.SumBytes(raw), Size: int64(len(raw))}, role, 1<<20)
		return err
	}
	item, err := metadata("quality/report.json", "application/vnd.cpgen.quality+json")
	if err != nil || item.Blob.Blob.Digest != *attempt.OutputDigest {
		return empty, errors.New("quality output differs from its report")
	}
	raw, err := reader.read(item.Blob.LogicalPath, item.Blob.Blob, domain.ArtifactOutput, 1<<20)
	if err != nil {
		return empty, err
	}
	var report QualityReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return empty, err
	}
	canonical, err := json.Marshal(report)
	if err != nil || !bytes.Equal(raw, canonical) {
		return empty, errors.New("quality report is not canonical")
	}
	if err := report.ValidateFor(runID, input); err != nil {
		return empty, err
	}
	config := s.sandbox
	config.Identity = qualityVerificationIdentity(attempt, 1)
	_, filename, media, compiler, err := solutionCompiler("cpp", config.Lock)
	if err != nil {
		return empty, err
	}
	if err := local(domain.SafeRelPath("quality/checker/"+filename), judge.ExactTokenCheckerSource(), domain.ArtifactSource, media); err != nil {
		return empty, err
	}
	bundle := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: domain.SafeRelPath(filename), Files: []port.SourceFile{{Path: domain.SafeRelPath(filename), Blob: report.Checker.Source}}, Digest: report.Checker.SourceBundleDigest}
	compileRequest := port.CompileRequest{Language: port.LanguageCPP20, Role: port.RoleChecker, SourceBundle: bundle, Toolchain: compiler.ID, Limits: solutionCompileLimits(), ExpectedOutput: compiler.OutputPath}
	if err := reader.result(config, compileRequest, report.Checker.Result); err != nil {
		return empty, err
	}
	for index, canary := range report.Canaries {
		spec := qualityCanaries()[index]
		for i, raw := range []string{spec.Input, spec.Candidate, spec.Answer} {
			path := domain.SafeRelPath(fmt.Sprintf("quality/canaries/%s/%s", spec.Name, []string{"input.txt", "output.txt", "answer.txt"}[i]))
			if err := local(path, []byte(raw), domain.ArtifactInput, "text/plain"); err != nil {
				return empty, err
			}
		}
		request := qualityCheckerRequest(report.Checker.Result.Program.Blob, canary.Input, canary.Candidate, canary.Answer)
		if err := reader.result(config, request, canary.Result); err != nil {
			return empty, err
		}
	}
	for _, item := range report.Cases {
		request := qualityCheckerRequest(report.Checker.Result.Program.Blob, item.Input, item.Candidate, item.Answer)
		if err := reader.result(config, request, item.Result); err != nil {
			return empty, err
		}
	}
	return report, reader.complete()
}
