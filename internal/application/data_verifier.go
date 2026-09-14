package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	artifact "cpgen/internal/artifact"
	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

const dataVerificationSchema = "cpgen.data-verification/v1"

type DataVerifierConfig struct {
	Sandbox, RepeatSandbox SolutionSandbox
	Publisher              StageArtifactPublisher
	Blobs                  port.VerifiedBlobReader
	Lock                   toolchain.Lock
}

type DataVerifier struct {
	config                   DataVerifierConfig
	lockDigest, policyDigest domain.Digest
}

type DataSampleEvidence struct {
	Sample    int            `json:"sample"`
	Input     domain.BlobRef `json:"input"`
	Validator port.RunResult `json:"validator"`
}

type DataGeneratedEvidence struct {
	Case      domain.DataCase  `json:"case"`
	Runs      []port.RunResult `json:"runs"`
	Input     *domain.BlobRef  `json:"input,omitempty"`
	Validator *port.RunResult  `json:"validator,omitempty"`
}

type DataVerificationReport struct {
	SchemaVersion       string                    `json:"schema_version"`
	InputDigest         domain.Digest             `json:"input_digest"`
	ContentDigest       domain.Digest             `json:"content_digest"`
	ToolchainLockDigest domain.Digest             `json:"toolchain_lock_digest"`
	PolicyDigest        domain.Digest             `json:"policy_digest"`
	Compiles            []SolutionCompileEvidence `json:"compiles"`
	Samples             []DataSampleEvidence      `json:"samples"`
	Generated           []DataGeneratedEvidence   `json:"generated"`
	Passed              bool                      `json:"passed"`
	Reason              string                    `json:"reason,omitempty"`
}

type DatasetInput struct {
	Path    domain.SafeRelPath  `json:"path"`
	Origin  string              `json:"origin"`
	Ordinal int                 `json:"ordinal"`
	Kind    domain.DataCaseKind `json:"kind,omitempty"`
	Seed    *uint64             `json:"seed,omitempty"`
	Input   domain.BlobRef      `json:"input"`
}

// DatasetManifest contains validated inputs. Reference answers and Judge
// verdicts must be added by the next stage before this can become a package.
type DatasetManifest struct {
	SchemaVersion            string         `json:"schema_version"`
	DataContentDigest        domain.Digest  `json:"data_content_digest"`
	SolutionContentDigest    domain.Digest  `json:"solution_content_digest"`
	VerificationReportDigest domain.Digest  `json:"verification_report_digest"`
	Inputs                   []DatasetInput `json:"inputs"`
}

type DataVerificationResult struct {
	Report          DataVerificationReport
	ReportArtifact  domain.PendingArtifact
	DatasetArtifact *domain.PendingArtifact
	Occurrences     []domain.PendingOccurrence
}

func NewDataVerifier(config DataVerifierConfig) (*DataVerifier, error) {
	if config.Sandbox == nil || config.RepeatSandbox == nil || config.Publisher == nil || config.Blobs == nil {
		return nil, errors.New("data verification requires two execution scopes and artifact capabilities")
	}
	raw, err := config.Lock.MarshalIndent()
	if err != nil {
		return nil, err
	}
	config.Lock, err = toolchain.LoadLock(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	digest, err := config.Lock.Digest()
	if err != nil {
		return nil, err
	}
	return &DataVerifier{config, digest, dataVerificationPolicyDigest(digest)}, nil
}

func dataRunLimits(role port.ProgramRole) port.RunLimits {
	stdout := int64(4096)
	if role == port.RoleGenerator {
		stdout = 1 << 20
	}
	return port.RunLimits{Time: 2 * time.Second, MemoryBytes: 256 << 20, PIDs: 64, StdoutBytes: stdout, StderrBytes: 8192}
}

func dataVerificationPolicyDigest(lock domain.Digest) domain.Digest {
	raw, _ := json.Marshal(struct {
		Schema               string
		Lock                 domain.Digest
		Compile              port.CompileLimits
		Generator, Validator port.RunLimits
		Reproduction         string
	}{dataVerificationSchema, lock, solutionCompileLimits(), dataRunLimits(port.RoleGenerator), dataRunLimits(port.RoleValidator), "two-independent-executions-exact-bytes-v1"})
	return domain.SumBytes(raw)
}

func dataGeneratorRequest(program domain.BlobRef, item domain.DataCase) port.RunRequest {
	seed := item.Seed
	return port.RunRequest{Role: port.RoleGenerator, Program: program, Seed: &seed, Args: port.RoleArgs{GeneratorCase: &port.GeneratorCaseArgs{Ordinal: item.Ordinal, Kind: item.Kind}}, Limits: dataRunLimits(port.RoleGenerator)}
}

func dataValidatorRequest(program, input domain.BlobRef) port.RunRequest {
	return port.RunRequest{Role: port.RoleValidator, Program: program, Stdin: &input, Limits: dataRunLimits(port.RoleValidator)}
}

// dataRunFailure returns only a deterministic business failure. Missing or
// infrastructure evidence is an error, never a passing or failed content claim.
func dataRunFailure(result port.RunResult, role port.ProgramRole) (string, error) {
	if err := result.Validate(); err != nil {
		return "", err
	}
	if result.Outcome == domain.ProcessInfraError {
		return "", errors.New("data execution infrastructure failure")
	}
	if role == port.RoleGenerator {
		adapted, err := judge.AdaptSolution(result)
		if err != nil || adapted.Outcome == nil {
			return "", errors.New("generator has no process verdict")
		}
		if *adapted.Outcome != domain.SolutionOK {
			return string(*adapted.Outcome), nil
		}
	} else {
		adapted, err := judge.AdaptValidator(result, judge.DefaultTestlibV1)
		if err != nil || adapted.Outcome == nil {
			return "", errors.New("validator has no process verdict")
		}
		if *adapted.Outcome != domain.ValidatorValid {
			return string(*adapted.Outcome), nil
		}
	}
	if result.Stdout == nil || result.Stdout.Blob.Size > dataRunLimits(role).StdoutBytes {
		return "", errors.New("successful data execution lacks bounded stdout")
	}
	if role == port.RoleValidator && result.Stdout.Blob.Size != 0 {
		return "validator_stdout_not_empty", nil
	}
	return "", nil
}

func independentDataRuns(first, second domain.CallTrace) bool {
	if first.DispatchKind != domain.DispatchDispatched || second.DispatchKind != domain.DispatchDispatched || first.LogicalOperationID == second.LogicalOperationID || first.ResultAttemptCallID == nil || second.ResultAttemptCallID == nil || *first.ResultAttemptCallID == *second.ResultAttemptCallID {
		return false
	}
	seen := make(map[domain.AttemptCallID]bool)
	for _, id := range first.PhysicalAttemptCallIDs {
		seen[id] = true
	}
	for _, id := range second.PhysicalAttemptCallIDs {
		if seen[id] {
			return false
		}
	}
	return true
}

func (s *DataVerifier) Verify(ctx context.Context, input domain.DataDraftInputV1, content domain.DataContent) (DataVerificationResult, error) {
	var empty DataVerificationResult
	if err := content.ValidateInput(input); err != nil {
		return empty, err
	}
	inputDigest, err := input.Digest()
	if err != nil {
		return empty, err
	}
	language, filename, media, compiler, err := solutionCompiler(content.Language, s.config.Lock)
	if err != nil {
		return empty, err
	}
	report := DataVerificationReport{SchemaVersion: dataVerificationSchema, InputDigest: inputDigest, ContentDigest: content.ContentDigest, ToolchainLockDigest: s.lockDigest, PolicyDigest: s.policyDigest, Compiles: []SolutionCompileEvidence{}, Samples: []DataSampleEvidence{}, Generated: []DataGeneratedEvidence{}}
	var files []domain.PendingArtifact
	publish := func(path string, role domain.ArtifactRole, media string, raw []byte) (domain.PendingArtifact, error) {
		pending, err := s.config.Publisher.Publish(ctx, port.ArtifactDeclaration{LogicalPath: domain.SafeRelPath(path), Role: role, MediaType: media, MaxBytes: int64(max(1, len(raw))), Provenance: domain.ProvenanceCandidate{SchemaVersion: dataVerificationSchema, Producer: "data-verifier", InputDigest: &content.ContentDigest}}, raw)
		if err == nil {
			files = append(files, pending)
		}
		return pending, err
	}
	finish := func() (DataVerificationResult, error) {
		if err := report.ValidateFor(input, content); err != nil {
			return empty, err
		}
		raw, err := json.Marshal(report)
		if err != nil || len(raw) > 1<<20 {
			return empty, errors.New("data verification report exceeds its serialization bound")
		}
		artifact, err := publish("data/verification.json", domain.ArtifactOutput, "application/vnd.cpgen.data-verification+json", raw)
		if err != nil {
			return empty, err
		}
		result := DataVerificationResult{Report: report, ReportArtifact: artifact}
		if report.Passed {
			manifest, err := report.Dataset(input, content)
			if err != nil {
				return empty, err
			}
			raw, err := json.Marshal(manifest)
			if err != nil {
				return empty, err
			}
			pending, err := publish("data/dataset.json", domain.ArtifactOutput, "application/vnd.cpgen.dataset+json", raw)
			if err != nil {
				return empty, err
			}
			result.DatasetArtifact = &pending
		}
		all := append(files, s.config.Sandbox.Artifacts()...)
		all = append(all, s.config.RepeatSandbox.Artifacts()...)
		seen := make(map[domain.ArtifactWriterTokenID]bool)
		for _, item := range all {
			if !seen[item.WriterTokenID] {
				pending := item
				result.Occurrences = append(result.Occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending})
				seen[item.WriterTokenID] = true
			}
		}
		return result, nil
	}
	plan, err := content.Plan.CanonicalJSON()
	if err != nil {
		return empty, err
	}
	if _, err := publish("data/plan.json", domain.ArtifactOutput, "application/vnd.cpgen.test-plan+json", plan); err != nil {
		return empty, err
	}
	programs := make(map[port.ProgramRole]domain.BlobRef)
	for i, role := range []port.ProgramRole{port.RoleGenerator, port.RoleValidator} {
		name, code := "generator", content.GeneratorCode
		if i == 1 {
			name, code = "validator", content.ValidatorCode
		}
		source, err := publish("data/"+name+"/"+filename, domain.ArtifactSource, media, []byte(code))
		if err != nil {
			return empty, err
		}
		bundle := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: domain.SafeRelPath(filename), Files: []port.SourceFile{{Path: domain.SafeRelPath(filename), Blob: source.Blob}}}
		bundle.Digest, err = port.ComputeSourceBundleDigest(bundle)
		if err != nil {
			return empty, err
		}
		compiled, err := s.config.Sandbox.Compile(ctx, port.CompileRequest{Language: language, Role: role, SourceBundle: bundle, Toolchain: compiler.ID, Limits: solutionCompileLimits(), ExpectedOutput: compiler.OutputPath})
		if err != nil {
			return empty, err
		}
		if err := compiled.Validate(); err != nil {
			return empty, err
		}
		if compiled.Failure != nil || compiled.Value.Validate() != nil || !compiled.CallTrace.Equal(compiled.Value.CallTrace) || compiled.Value.Outcome == domain.CompileInfraError {
			return empty, errors.New("data compilation lacks complete execution evidence")
		}
		report.Compiles = append(report.Compiles, SolutionCompileEvidence{Role: role, Source: source.Blob, SourceBundleDigest: bundle.Digest, Result: *compiled.Value})
		if compiled.Value.Outcome != domain.CompileOK {
			report.Reason = fmt.Sprintf("compile.%s.%s", role, compiled.Value.Outcome)
			return finish()
		}
		programs[role] = compiled.Value.Program.Blob
	}
	run := func(sandbox SolutionSandbox, request port.RunRequest) (port.RunResult, error) {
		result, err := sandbox.Run(ctx, request)
		if err != nil {
			return port.RunResult{}, err
		}
		if err := result.Validate(); err != nil {
			return port.RunResult{}, err
		}
		if result.Failure != nil || !result.CallTrace.Equal(result.Value.CallTrace) {
			return port.RunResult{}, errors.New("data run lacks complete execution evidence")
		}
		return *result.Value, nil
	}
	for i, sample := range input.SolutionInput.Problem.Samples {
		stdin, err := publish(fmt.Sprintf("data/samples/%03d.in", i+1), domain.ArtifactInput, "text/plain", []byte(sample.Input))
		if err != nil {
			return empty, err
		}
		result, err := run(s.config.Sandbox, dataValidatorRequest(programs[port.RoleValidator], stdin.Blob))
		if err != nil {
			return empty, err
		}
		report.Samples = append(report.Samples, DataSampleEvidence{i + 1, stdin.Blob, result})
		failure, err := dataRunFailure(result, port.RoleValidator)
		if err != nil {
			return empty, err
		}
		if failure != "" {
			report.Reason = fmt.Sprintf("sample.%d.%s", i+1, failure)
			return finish()
		}
	}
	for _, item := range content.Plan.Cases {
		report.Generated = append(report.Generated, DataGeneratedEvidence{Case: item, Runs: []port.RunResult{}})
		check := &report.Generated[len(report.Generated)-1]
		var outputs [][]byte
		for i, sandbox := range []SolutionSandbox{s.config.Sandbox, s.config.RepeatSandbox} {
			result, err := run(sandbox, dataGeneratorRequest(programs[port.RoleGenerator], item))
			if err != nil {
				return empty, err
			}
			check.Runs = append(check.Runs, result)
			if i == 1 && !independentDataRuns(check.Runs[0].CallTrace, result.CallTrace) {
				return empty, errors.New("reproduction requires independent physical executions")
			}
			failure, err := dataRunFailure(result, port.RoleGenerator)
			if err != nil {
				return empty, err
			}
			if failure != "" {
				report.Reason = fmt.Sprintf("generated.%d.run.%d.%s", item.Ordinal, i+1, failure)
				return finish()
			}
			output, err := artifact.ReadVerified(ctx, s.config.Blobs, result.Stdout.Blob, 1<<20)
			if err != nil {
				return empty, err
			}
			outputs = append(outputs, output)
		}
		if !bytes.Equal(outputs[0], outputs[1]) {
			report.Reason = fmt.Sprintf("generated.%d.nondeterministic", item.Ordinal)
			return finish()
		}
		stdin, err := publish(fmt.Sprintf("data/generated/%03d.in", item.Ordinal), domain.ArtifactInput, "text/plain", outputs[0])
		if err != nil {
			return empty, err
		}
		check.Input = &stdin.Blob
		validation, err := run(s.config.Sandbox, dataValidatorRequest(programs[port.RoleValidator], stdin.Blob))
		if err != nil {
			return empty, err
		}
		check.Validator = &validation
		failure, err := dataRunFailure(validation, port.RoleValidator)
		if err != nil {
			return empty, err
		}
		if failure != "" {
			report.Reason = fmt.Sprintf("generated.%d.%s", item.Ordinal, failure)
			return finish()
		}
	}
	report.Passed = true
	return finish()
}

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
