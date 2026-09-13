package application

import (
	"encoding/json"
	"errors"
	"fmt"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func (r DataVerificationReport) ValidateFor(input domain.DataDraftInputV1, content domain.DataContent) error {
	if err := content.ValidateInput(input); err != nil {
		return err
	}
	digest, err := input.Digest()
	if err != nil {
		return err
	}
	if r.SchemaVersion != dataVerificationSchema || r.InputDigest != digest || r.ContentDigest != content.ContentDigest || r.ToolchainLockDigest.Validate() != nil || r.PolicyDigest != dataVerificationPolicyDigest(r.ToolchainLockDigest) {
		return errors.New("data verification report binding differs")
	}
	if len(r.Compiles) < 1 || len(r.Compiles) > 2 {
		return errors.New("data report requires ordered compile evidence")
	}
	failure := ""
	roles := []port.ProgramRole{port.RoleGenerator, port.RoleValidator}
	codes := []string{content.GeneratorCode, content.ValidatorCode}
	filename := domain.SafeRelPath("main.cpp")
	if content.Language == "go" {
		filename = "main.go"
	}
	for i, compiled := range r.Compiles {
		if failure != "" || compiled.Role != roles[i] || compiled.Source != (domain.BlobRef{Digest: domain.SumBytes([]byte(codes[i])), Size: int64(len(codes[i]))}) {
			return errors.New("data compiler differs from source or order")
		}
		bundle, err := port.ComputeSourceBundleDigest(port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: filename, Files: []port.SourceFile{{Path: filename, Blob: compiled.Source}}})
		if err != nil || bundle != compiled.SourceBundleDigest {
			return errors.New("data source bundle differs")
		}
		if err := compiled.Result.Validate(); err != nil {
			return err
		}
		if compiled.Result.Outcome == domain.CompileInfraError {
			return errors.New("data report contains an infrastructure failure")
		}
		if compiled.Result.Outcome != domain.CompileOK {
			failure = fmt.Sprintf("compile.%s.%s", compiled.Role, compiled.Result.Outcome)
		}
	}
	samples := input.SolutionInput.Problem.Samples
	if len(r.Samples) > len(samples) || (len(r.Samples) > 0 && (failure != "" || len(r.Compiles) != 2)) {
		return errors.New("data samples lack completed compilation")
	}
	for i, sample := range r.Samples {
		if failure != "" || sample.Sample != i+1 || sample.Input != (domain.BlobRef{Digest: domain.SumBytes([]byte(samples[i].Input)), Size: int64(len(samples[i].Input))}) {
			return errors.New("data sample differs from the problem or check order")
		}
		failure, err = dataRunFailure(sample.Validator, port.RoleValidator)
		if err != nil {
			return err
		}
		if failure != "" {
			failure = fmt.Sprintf("sample.%d.%s", sample.Sample, failure)
		}
	}
	if len(r.Generated) > len(content.Plan.Cases) || (len(r.Generated) > 0 && (failure != "" || len(r.Compiles) != 2 || len(r.Samples) != len(samples))) {
		return errors.New("generated data lacks completed sample validation")
	}
	for i, generated := range r.Generated {
		if failure != "" || generated.Case != content.Plan.Cases[i] || len(generated.Runs) < 1 || len(generated.Runs) > 2 {
			return errors.New("generated evidence differs from the plan or first-failure order")
		}
		for j, result := range generated.Runs {
			if failure != "" {
				return errors.New("generator continued after failure")
			}
			if j == 1 && !independentDataRuns(generated.Runs[0].CallTrace, result.CallTrace) {
				return errors.New("reproduction shares an original execution")
			}
			failure, err = dataRunFailure(result, port.RoleGenerator)
			if err != nil {
				return err
			}
			if failure != "" {
				failure = fmt.Sprintf("generated.%d.run.%d.%s", generated.Case.Ordinal, j+1, failure)
			}
		}
		if failure == "" {
			if len(generated.Runs) != 2 {
				return errors.New("data case lacks its independent reproduction")
			}
			if generated.Runs[0].Stdout.Blob != generated.Runs[1].Stdout.Blob {
				failure = fmt.Sprintf("generated.%d.nondeterministic", generated.Case.Ordinal)
			}
		}
		if failure != "" {
			if generated.Input != nil || generated.Validator != nil {
				return errors.New("failed generation was promoted to validated input")
			}
			continue
		}
		if generated.Input == nil || *generated.Input != generated.Runs[0].Stdout.Blob || generated.Validator == nil {
			return errors.New("generated input lacks exact bytes or validator evidence")
		}
		failure, err = dataRunFailure(*generated.Validator, port.RoleValidator)
		if err != nil {
			return err
		}
		if failure != "" {
			failure = fmt.Sprintf("generated.%d.%s", generated.Case.Ordinal, failure)
		}
	}
	if r.Passed {
		if failure != "" || r.Reason != "" || len(r.Compiles) != 2 || len(r.Samples) != len(samples) || len(r.Generated) != len(content.Plan.Cases) {
			return errors.New("passing data verification omits a required check")
		}
	} else if failure == "" || r.Reason != failure {
		return errors.New("failed data verification lacks its exact first failure")
	}
	return nil
}

func (r DataVerificationReport) Dataset(input domain.DataDraftInputV1, content domain.DataContent) (DatasetManifest, error) {
	var empty DatasetManifest
	if err := r.ValidateFor(input, content); err != nil {
		return empty, err
	}
	if !r.Passed {
		return empty, errors.New("dataset requires passing data verification")
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return empty, err
	}
	manifest := DatasetManifest{SchemaVersion: "cpgen.dataset/v1", DataContentDigest: content.ContentDigest, SolutionContentDigest: content.SolutionContentDigest, VerificationReportDigest: domain.SumBytes(raw), Inputs: []DatasetInput{}}
	for _, sample := range r.Samples {
		manifest.Inputs = append(manifest.Inputs, DatasetInput{Path: domain.SafeRelPath(fmt.Sprintf("samples/%03d.in", sample.Sample)), Origin: "sample", Ordinal: sample.Sample, Input: sample.Input})
	}
	for _, generated := range r.Generated {
		seed := generated.Case.Seed
		manifest.Inputs = append(manifest.Inputs, DatasetInput{Path: domain.SafeRelPath(fmt.Sprintf("generated/%03d.in", generated.Case.Ordinal)), Origin: "generated", Ordinal: generated.Case.Ordinal, Kind: generated.Case.Kind, Seed: &seed, Input: *generated.Input})
	}
	return manifest, nil
}
