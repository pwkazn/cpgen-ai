package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// ReadJudgeVerification proves the retained programs, validated inputs, exact
// execution requests, cleaned resources and answer bytes without dispatching.
func (s *DataReader) ReadJudgeVerification(ctx context.Context, runID domain.RunID) (JudgeVerificationReport, error) {
	var empty JudgeVerificationReport
	store := s.store
	input, err := s.ReadJudgeInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	digest, err := stableValueDigest(input.DataReport)
	if err != nil {
		return empty, err
	}
	stage, err := store.ReadCommittedSandboxStage(ctx, runID, "judge")
	if err != nil {
		return empty, err
	}
	attempt := stage.Attempt
	if attempt.Validate() != nil || attempt.RunID != runID || attempt.StageName != "judge" || attempt.State != domain.StageAttemptSucceeded || attempt.InputDigest != digest || attempt.OutputDigest == nil {
		return empty, errors.New("Judge stage differs from current verified data")
	}
	reader, err := newSandboxStageEvidence(ctx, store, s.blobs, stage)
	if err != nil {
		return empty, err
	}
	metadata := func(path domain.SafeRelPath, media string) (port.CommittedPrivateStageArtifact, error) {
		item, found := reader.items[path]
		if !found || item.Blob.MediaType != media || item.Blob.Provenance.SchemaVersion != judgeVerificationSchema || item.Blob.Provenance.Producer != "judge-verifier" || item.Blob.Provenance.InputDigest == nil || *item.Blob.Provenance.InputDigest != digest {
			return item, errors.New("Judge artifact lost its committed data binding")
		}
		return item, nil
	}
	local := func(path domain.SafeRelPath, raw []byte, media string) error {
		if _, err := metadata(path, media); err != nil {
			return err
		}
		_, err := reader.read(path, domain.BlobRef{Digest: domain.SumBytes(raw), Size: int64(len(raw))}, domain.ArtifactOutput, 1<<20)
		return err
	}
	item, err := metadata("judge/verification.json", "application/vnd.cpgen.judge-verification+json")
	if err != nil || item.Blob.Blob.Digest != *attempt.OutputDigest {
		return empty, errors.New("Judge output differs from its committed report")
	}
	raw, err := reader.read(item.Blob.LogicalPath, item.Blob.Blob, domain.ArtifactOutput, 1<<20)
	if err != nil {
		return empty, err
	}
	var report JudgeVerificationReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return empty, err
	}
	canonical, err := json.Marshal(report)
	if err != nil || !bytes.Equal(raw, canonical) {
		return empty, errors.New("Judge report is not canonical")
	}
	if err := report.ValidateFor(input); err != nil {
		return empty, err
	}
	config := s.sandbox
	config.Identity = judgeVerificationIdentity(attempt, 1)
	for _, check := range report.Cases {
		validate := func(role port.ProgramRole, result port.RunResult, token domain.Digest) error {
			if err := reader.result(config, judgeRunRequest(input, check.Input.Input, role), result); err != nil {
				return err
			}
			if token != "" {
				raw, err := readSolutionVerificationBlob(ctx, s.blobs, result.Stdout.Blob, 1<<20)
				if err != nil {
					return err
				}
				if solutionTokenDigest(raw) != token {
					return errors.New("Judge token comparison differs from actual output")
				}
			}
			return nil
		}
		if err := validate(port.RoleSolution, check.Reference, check.ReferenceTokenDigest); err != nil {
			return empty, err
		}
		if check.Brute != nil {
			if err := validate(port.RoleBrute, *check.Brute, check.BruteTokenDigest); err != nil {
				return empty, err
			}
		}
		if check.Answer != nil {
			raw, err := readSolutionVerificationBlob(ctx, s.blobs, *check.Answer, 1<<20)
			if err != nil {
				return empty, err
			}
			if err := local(domain.SafeRelPath("judge/"+string(judgeAnswerPath(check.Input))), raw, "text/plain"); err != nil {
				return empty, err
			}
		}
	}
	if report.Passed {
		manifest, err := report.Dataset(input)
		if err != nil {
			return empty, err
		}
		raw, err := json.Marshal(manifest)
		if err != nil {
			return empty, err
		}
		if err := local("judge/dataset.json", raw, "application/vnd.cpgen.judged-dataset+json"); err != nil {
			return empty, err
		}
	}
	return report, reader.complete()
}
