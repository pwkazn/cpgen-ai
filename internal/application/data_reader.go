package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// ReadVerification reconstructs the committed data report and every exact
// generator/validator request. It performs no Docker execution or publication.
func (s *DataExecutor) ReadVerification(ctx context.Context, runID domain.RunID) (DataVerificationReport, error) {
	var empty DataVerificationReport
	store, ok := s.generation.config.Store.(interface {
		solutionVerificationReadStore
		sandboxEvidenceStore
	})
	if !ok {
		return empty, errors.New("data verification requires committed sandbox evidence reads")
	}
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
	reader, err := newSandboxStageEvidence(ctx, store, s.generation.config.Blobs, stage)
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
			raw, err := readSolutionVerificationBlob(ctx, s.generation.config.Blobs, *generated.Input, 1<<20)
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
