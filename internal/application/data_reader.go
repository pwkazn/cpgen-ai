package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	sandboxexec "cpgen/internal/adapter/sandbox"
	artifact "cpgen/internal/artifact"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

type DataReader struct {
	solution   *SolutionReader
	sandbox    sandboxexec.ReadPolicy
	generation *GenerationReader
	calls      durable.CommittedDraftReader
	store      SandboxEvidenceReadStore
	blobs      port.VerifiedBlobReader
}

func (s *DataReader) ReadInput(ctx context.Context, runID domain.RunID) (domain.AgentResult[domain.DataDraftInputV1], error) {
	var empty domain.AgentResult[domain.DataDraftInputV1]
	input, err := s.solution.ReadInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("data requires current committed acceptance")
	}
	content, err := s.solution.ReadDraft(ctx, runID)
	if err != nil {
		return empty, err
	}
	report, err := s.solution.ReadVerification(ctx, runID, s.sandbox)
	if err != nil {
		return empty, err
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return empty, err
	}
	digest := domain.SumBytes(raw)
	if !report.Passed {
		return domain.Review[domain.DataDraftInputV1](domain.ReviewRequest{EvidenceDigest: digest, PolicyDigest: report.PolicyDigest, Reason: "solution_requires_review:" + report.Reason}), nil
	}
	bound, err := domain.NewDataDraftInput(*input.Value, content, digest)
	if err != nil {
		return empty, err
	}
	return domain.Success(bound), nil
}

func (s *DataReader) ReadDraft(ctx context.Context, runID domain.RunID) (domain.DataContent, error) {
	var empty domain.DataContent
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("data draft has no passing current Solution")
	}
	variables, err := dataDraftVariables(s.solution.revision, *input.Value)
	if err != nil {
		return empty, err
	}
	digest, err := input.Value.Digest()
	if err != nil {
		return empty, err
	}
	calls := s.calls
	raw, expected, err := s.generation.readDraft(ctx, runID, "data", digest, variables, calls)
	if err != nil {
		return empty, err
	}
	var draft domain.DataDraftV1
	if err := json.Unmarshal(raw, &draft); err != nil {
		return empty, err
	}
	content, err := draft.Bind(*input.Value)
	if err != nil {
		return empty, err
	}
	if content.ContentDigest != expected {
		return empty, errors.New("committed data digest differs from reconstructed content")
	}
	return content, nil
}

func dataDraftVariables(revision string, input domain.DataDraftInputV1) ([]byte, error) {
	if revision == workflow.ExecutedSamplesRevision {
		return input.ProgramContextJSON()
	}
	return input.CanonicalJSON()
}

func (s *DataReader) ReadJudgeInput(ctx context.Context, runID domain.RunID) (JudgeInput, error) {
	var empty JudgeInput
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("Judge requires a current passing Solution")
	}
	content, err := s.ReadDraft(ctx, runID)
	if err != nil {
		return empty, err
	}
	report, err := s.ReadVerification(ctx, runID)
	if err != nil {
		return empty, err
	}
	solution, err := s.solution.ReadVerification(ctx, runID, s.sandbox)
	if err != nil {
		return empty, err
	}
	value := JudgeInput{WorkflowRevision: s.solution.revision, DataInput: *input.Value, Data: content, DataReport: report, SolutionReport: solution}
	if _, err := value.dataset(); err != nil {
		return empty, err
	}
	return value, nil
}

// ReadVerification reconstructs the committed data report and every exact
// generator/validator request. It performs no Docker execution or publication.
func (s *DataReader) ReadVerification(ctx context.Context, runID domain.RunID) (DataVerificationReport, error) {
	var empty DataVerificationReport
	store := s.store
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("data verification lost its passing Solution")
	}
	content, err := s.ReadDraft(ctx, runID)
	if err != nil {
		return empty, err
	}
	stage, err := store.ReadCommittedSandboxStage(ctx, runID, "data_verify")
	if err != nil {
		return empty, err
	}
	attempt := stage.Attempt
	if attempt.Validate() != nil || attempt.RunID != runID || attempt.StageName != "data_verify" || attempt.State != domain.StageAttemptSucceeded || attempt.InputDigest != content.ContentDigest || attempt.OutputDigest == nil {
		return empty, errors.New("data verification stage differs from the current draft")
	}
	reader, err := newSandboxStageEvidence(ctx, store, s.blobs, stage)
	if err != nil {
		return empty, err
	}
	metadata := func(path domain.SafeRelPath, media string) (port.CommittedPrivateStageArtifact, error) {
		item, found := reader.items[path]
		if !found || item.Blob.MediaType != media || item.Blob.Provenance.SchemaVersion != dataVerificationSchema || item.Blob.Provenance.Producer != "data-verifier" || item.Blob.Provenance.InputDigest == nil || *item.Blob.Provenance.InputDigest != content.ContentDigest {
			return item, errors.New("data artifact lacks its committed content binding")
		}
		return item, nil
	}
	local := func(path domain.SafeRelPath, expected []byte, role domain.ArtifactRole, media string) error {
		if _, err := metadata(path, media); err != nil {
			return err
		}
		_, err := reader.read(path, domain.BlobRef{Digest: domain.SumBytes(expected), Size: int64(len(expected))}, role, 1<<20)
		return err
	}
	item, err := metadata("data/verification.json", "application/vnd.cpgen.data-verification+json")
	if err != nil || item.Blob.Blob.Digest != *attempt.OutputDigest {
		return empty, errors.New("data stage output differs from its report")
	}
	raw, err := reader.read(item.Blob.LogicalPath, item.Blob.Blob, domain.ArtifactOutput, 1<<20)
	if err != nil {
		return empty, err
	}
	var report DataVerificationReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return empty, err
	}
	canonical, err := json.Marshal(report)
	if err != nil || !bytes.Equal(raw, canonical) {
		return empty, errors.New("data report is not canonical")
	}
	if err := report.ValidateFor(*input.Value, content); err != nil {
		return empty, err
	}
	lockDigest, err := s.sandbox.Lock.Digest()
	if err != nil || report.ToolchainLockDigest != lockDigest {
		return empty, errors.New("data report changed the frozen toolchain")
	}
	plan, err := content.Plan.CanonicalJSON()
	if err != nil {
		return empty, err
	}
	if err := local("data/plan.json", plan, domain.ArtifactOutput, "application/vnd.cpgen.test-plan+json"); err != nil {
		return empty, err
	}
	main, repeat := s.sandbox, s.sandbox
	main.Identity = dataVerificationIdentity(attempt, 1, false)
	repeat.Identity = dataVerificationIdentity(attempt, 1, true)
	language, filename, media, compiler, err := solutionCompiler(content.Language, main.Lock)
	if err != nil {
		return empty, err
	}
	programs := make(map[port.ProgramRole]domain.BlobRef)
	for i, compiled := range report.Compiles {
		name, code := "generator", content.GeneratorCode
		if i == 1 {
			name, code = "validator", content.ValidatorCode
		}
		if err := local(domain.SafeRelPath("data/"+name+"/"+filename), []byte(code), domain.ArtifactSource, media); err != nil {
			return empty, err
		}
		bundle := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: domain.SafeRelPath(filename), Files: []port.SourceFile{{Path: domain.SafeRelPath(filename), Blob: compiled.Source}}, Digest: compiled.SourceBundleDigest}
		request := port.CompileRequest{Language: language, Role: compiled.Role, SourceBundle: bundle, Toolchain: compiler.ID, Limits: solutionCompileLimits(), ExpectedOutput: compiler.OutputPath}
		if err := reader.result(main, request, compiled.Result); err != nil {
			return empty, err
		}
		if compiled.Result.Program != nil {
			programs[compiled.Role] = compiled.Result.Program.Blob
		}
	}
	for _, sample := range report.Samples {
		expected := []byte(input.Value.SolutionInput.Problem.Samples[sample.Sample-1].Input)
		if err := local(domain.SafeRelPath(fmt.Sprintf("data/samples/%03d.in", sample.Sample)), expected, domain.ArtifactInput, "text/plain"); err != nil {
			return empty, err
		}
		if err := reader.result(main, dataValidatorRequest(programs[port.RoleValidator], sample.Input), sample.Validator); err != nil {
			return empty, err
		}
	}
	for _, generated := range report.Generated {
		request := dataGeneratorRequest(programs[port.RoleGenerator], generated.Case)
		for i, result := range generated.Runs {
			config := main
			if i == 1 {
				config = repeat
			}
			if err := reader.result(config, request, result); err != nil {
				return empty, err
			}
		}
		if generated.Input != nil {
			raw, err := artifact.ReadVerified(ctx, s.blobs, *generated.Input, 1<<20)
			if err != nil {
				return empty, err
			}
			if err := local(domain.SafeRelPath(fmt.Sprintf("data/generated/%03d.in", generated.Case.Ordinal)), raw, domain.ArtifactInput, "text/plain"); err != nil {
				return empty, err
			}
			if err := reader.result(main, dataValidatorRequest(programs[port.RoleValidator], *generated.Input), *generated.Validator); err != nil {
				return empty, err
			}
		}
	}
	if report.Passed {
		manifest, err := report.Dataset(*input.Value, content)
		if err != nil {
			return empty, err
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			return empty, err
		}
		if err := local("data/dataset.json", raw, domain.ArtifactOutput, "application/vnd.cpgen.dataset+json"); err != nil {
			return empty, err
		}
	}
	return report, reader.complete()
}
