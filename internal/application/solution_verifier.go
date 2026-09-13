package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

const solutionVerificationSchema = "cpgen.solution-verification/v1"
const solutionSampleComparison = judge.ExactTokenComparisonV1

type SolutionSandbox interface {
	port.MeteredSandbox
	Artifacts() []domain.PendingArtifact
}

type SolutionVerifierConfig struct {
	Sandbox   SolutionSandbox
	Publisher StageArtifactPublisher
	Blobs     port.VerifiedBlobReader
	Lock      toolchain.Lock
}

type SolutionVerifier struct {
	config                   SolutionVerifierConfig
	lockDigest, policyDigest domain.Digest
}

type SolutionCompileEvidence struct {
	Role               port.ProgramRole   `json:"role"`
	Source             domain.BlobRef     `json:"source"`
	SourceBundleDigest domain.Digest      `json:"source_bundle_digest"`
	Result             port.CompileResult `json:"result"`
}

type SolutionSampleEvidence struct {
	Sample            int              `json:"sample"`
	Role              port.ProgramRole `json:"role"`
	Input             domain.BlobRef   `json:"input"`
	Expected          domain.BlobRef   `json:"expected"`
	Result            port.RunResult   `json:"result"`
	ActualTokenDigest domain.Digest    `json:"actual_token_digest,omitempty"`
	Matches           bool             `json:"matches"`
}

// A completed verification report may fail. It is committed as evidence before
// the workflow selects human review or the next business stage. Passing samples
// is not a complete Judge/Quality verdict for the later generated test suite.
type SolutionVerificationReport struct {
	SchemaVersion       string                    `json:"schema_version"`
	InputDigest         domain.Digest             `json:"input_digest"`
	ContentDigest       domain.Digest             `json:"content_digest"`
	ProblemSpecDigest   domain.Digest             `json:"problem_spec_digest"`
	PolicyDigest        domain.Digest             `json:"policy_digest"`
	ToolchainLockDigest domain.Digest             `json:"toolchain_lock_digest"`
	Comparison          string                    `json:"comparison"`
	Compiles            []SolutionCompileEvidence `json:"compiles"`
	Samples             []SolutionSampleEvidence  `json:"samples"`
	Passed              bool                      `json:"passed"`
	Reason              string                    `json:"reason,omitempty"`
}

type SolutionVerificationResult struct {
	Report         SolutionVerificationReport
	ReportArtifact domain.PendingArtifact
	Occurrences    []domain.PendingOccurrence
}

func NewSolutionVerifier(config SolutionVerifierConfig) (*SolutionVerifier, error) {
	if config.Sandbox == nil || config.Publisher == nil || config.Blobs == nil {
		return nil, errors.New("solution verifier requires sandbox and artifact capabilities")
	}
	encoded, err := config.Lock.MarshalIndent()
	if err != nil {
		return nil, err
	}
	config.Lock, err = toolchain.LoadLock(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	lockDigest, err := config.Lock.Digest()
	if err != nil {
		return nil, err
	}
	return &SolutionVerifier{config: config, lockDigest: lockDigest, policyDigest: solutionVerificationPolicyDigest(lockDigest)}, nil
}

func solutionVerificationPolicyDigest(lockDigest domain.Digest) domain.Digest {
	policy, _ := json.Marshal(struct {
		Schema, Comparison   string
		Lock                 domain.Digest
		Compile              port.CompileLimits
		RunPIDs, StreamBytes int64
	}{solutionVerificationSchema, solutionSampleComparison, lockDigest, solutionCompileLimits(), 64, 1 << 20})
	return domain.SumBytes(policy)
}

func solutionCompileLimits() port.CompileLimits {
	return port.CompileLimits{Time: 45 * time.Second, MemoryBytes: 512 << 20, PIDs: 64, OutputBytes: 8 << 20}
}

func solutionCompiler(name string, lock toolchain.Lock) (port.Language, string, string, toolchain.Toolchain, error) {
	language, filename, media := port.LanguageCPP20, "main.cpp", "text/x-c++src"
	switch name {
	case "cpp":
	case "go":
		language, filename, media = port.LanguageGo, "main.go", "text/x-go"
	default:
		return "", "", "", toolchain.Toolchain{}, errors.New("solution verification supports cpp and go")
	}
	for _, compiler := range lock.Toolchains {
		if compiler.Language == language {
			return language, filename, media, compiler, nil
		}
	}
	return "", "", "", toolchain.Toolchain{}, errors.New("solution language has no locked compiler")
}

func (s *SolutionVerifier) Verify(ctx context.Context, input domain.SolutionDraftInputV1, content domain.SolutionContent) (SolutionVerificationResult, error) {
	var empty SolutionVerificationResult
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
	report := SolutionVerificationReport{SchemaVersion: solutionVerificationSchema, InputDigest: inputDigest, ContentDigest: content.ContentDigest, ProblemSpecDigest: input.Problem.SpecDigest, PolicyDigest: s.policyDigest, ToolchainLockDigest: s.lockDigest, Comparison: solutionSampleComparison, Compiles: []SolutionCompileEvidence{}, Samples: []SolutionSampleEvidence{}}
	var files []domain.PendingArtifact
	publish := func(path string, role domain.ArtifactRole, media string, data []byte) (domain.PendingArtifact, error) {
		pending, err := s.config.Publisher.Publish(ctx, port.ArtifactDeclaration{MediaType: media, Role: role, LogicalPath: domain.SafeRelPath(path), MaxBytes: int64(max(len(data), 1)), Provenance: domain.ProvenanceCandidate{SchemaVersion: solutionVerificationSchema, Producer: "solution-verifier", InputDigest: &content.ContentDigest}}, data)
		if err == nil {
			files = append(files, pending)
		}
		return pending, err
	}
	finish := func() (SolutionVerificationResult, error) {
		if err := report.ValidateFor(input, content); err != nil {
			return empty, err
		}
		raw, err := json.Marshal(report)
		if err != nil {
			return empty, err
		}
		if len(raw) > 1<<20 {
			return empty, errors.New("solution verification report exceeds byte bound")
		}
		artifact, err := publish("solution/verification.json", domain.ArtifactOutput, "application/vnd.cpgen.solution-verification+json", raw)
		if err != nil {
			return empty, err
		}
		all := append(files, s.config.Sandbox.Artifacts()...)
		occurrences := make([]domain.PendingOccurrence, 0, len(all))
		seen := make(map[domain.ArtifactWriterTokenID]bool)
		for index := range all {
			if !seen[all[index].WriterTokenID] {
				pending := all[index]
				occurrences = append(occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending})
				seen[pending.WriterTokenID] = true
			}
		}
		return SolutionVerificationResult{Report: report, ReportArtifact: artifact, Occurrences: occurrences}, nil
	}
	roles := []port.ProgramRole{port.RoleSolution, port.RoleBrute}
	programs := make(map[port.ProgramRole]domain.BlobRef)
	for index, role := range roles {
		name, code := "reference", content.ReferenceCode
		if index == 1 {
			name, code = "brute", content.BruteCode
		}
		source, err := publish("solution/"+name+"/"+filename, domain.ArtifactSource, media, []byte(code))
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
		if compiled.Failure != nil {
			return empty, fmt.Errorf("solution compilation unavailable: %s/%s", compiled.Failure.Code, compiled.Failure.Class)
		}
		if err := compiled.Value.Validate(); err != nil {
			return empty, err
		}
		if !compiled.CallTrace.Equal(compiled.Value.CallTrace) {
			return empty, errors.New("compile outcome trace differs from value")
		}
		if compiled.Value.Outcome == domain.CompileInfraError {
			return empty, errors.New("solution compiler infrastructure failure")
		}
		report.Compiles = append(report.Compiles, SolutionCompileEvidence{Role: role, Source: source.Blob, SourceBundleDigest: bundle.Digest, Result: *compiled.Value})
		if compiled.Value.Outcome != domain.CompileOK {
			report.Reason = fmt.Sprintf("compile.%s.%s", role, compiled.Value.Outcome)
			return finish()
		}
		programs[role] = compiled.Value.Program.Blob
	}
	for index, sample := range input.Problem.Samples {
		stdin, err := publish(fmt.Sprintf("solution/samples/%03d.in", index+1), domain.ArtifactInput, "text/plain", []byte(sample.Input))
		if err != nil {
			return empty, err
		}
		expected, err := publish(fmt.Sprintf("solution/samples/%03d.out", index+1), domain.ArtifactOutput, "text/plain", []byte(sample.Output))
		if err != nil {
			return empty, err
		}
		for _, role := range roles {
			run, err := s.config.Sandbox.Run(ctx, port.RunRequest{Role: role, Program: programs[role], Stdin: &stdin.Blob, Limits: port.RunLimits{Time: time.Duration(input.Problem.TimeLimitMS) * time.Millisecond, MemoryBytes: input.Problem.MemoryLimitMB << 20, PIDs: 64, StdoutBytes: 1 << 20, StderrBytes: 1 << 20}})
			if err != nil {
				return empty, err
			}
			if err := run.Validate(); err != nil {
				return empty, err
			}
			if run.Failure != nil {
				return empty, fmt.Errorf("solution sample unavailable: %s/%s", run.Failure.Code, run.Failure.Class)
			}
			if !run.CallTrace.Equal(run.Value.CallTrace) {
				return empty, errors.New("sample outcome trace differs from value")
			}
			adapted, err := judge.AdaptSolution(*run.Value)
			if err != nil {
				return empty, err
			}
			if adapted.InfrastructureFailure || adapted.Outcome == nil {
				return empty, errors.New("solution sample infrastructure failure")
			}
			check := SolutionSampleEvidence{Sample: index + 1, Role: role, Input: stdin.Blob, Expected: expected.Blob, Result: *run.Value}
			verdict := string(*adapted.Outcome)
			if *adapted.Outcome == domain.SolutionOK {
				if run.Value.Stdout == nil || run.Value.Stdout.Blob.Size > 1<<20 {
					return empty, errors.New("successful sample has no bounded stdout")
				}
				reader, err := s.config.Blobs.OpenVerified(ctx, run.Value.Stdout.Blob)
				if err != nil {
					return empty, err
				}
				actual, readErr := io.ReadAll(io.LimitReader(reader, (1<<20)+1))
				if err := errors.Join(readErr, reader.Close()); err != nil {
					return empty, err
				}
				if len(actual) > 1<<20 {
					return empty, errors.New("sample stdout exceeds policy")
				}
				check.ActualTokenDigest = solutionTokenDigest(actual)
				check.Matches = check.ActualTokenDigest == solutionTokenDigest([]byte(sample.Output))
				if !check.Matches {
					verdict = "WA"
				}
			}
			report.Samples = append(report.Samples, check)
			if !check.Matches {
				report.Reason = fmt.Sprintf("sample.%d.%s.%s", index+1, role, verdict)
				return finish()
			}
		}
	}
	report.Passed = true
	return finish()
}

func solutionTokenDigest(raw []byte) domain.Digest {
	return judge.ExactTokenDigest(raw)
}

func (r SolutionVerificationReport) ValidateFor(input domain.SolutionDraftInputV1, content domain.SolutionContent) error {
	if err := content.ValidateInput(input); err != nil {
		return err
	}
	digest, err := input.Digest()
	if err != nil {
		return err
	}
	if r.SchemaVersion != solutionVerificationSchema || r.Comparison != solutionSampleComparison || r.InputDigest != digest || r.ContentDigest != content.ContentDigest || r.ProblemSpecDigest != input.Problem.SpecDigest || r.PolicyDigest.Validate() != nil || r.ToolchainLockDigest.Validate() != nil {
		return errors.New("solution verification report binding differs")
	}
	if len(r.Compiles) < 1 || len(r.Compiles) > 2 {
		return errors.New("solution report requires ordered compile evidence")
	}
	roles := []port.ProgramRole{port.RoleSolution, port.RoleBrute}
	code := []string{content.ReferenceCode, content.BruteCode}
	failure := ""
	for index, compiled := range r.Compiles {
		if failure != "" || compiled.Role != roles[index] || compiled.Source.Digest != domain.SumBytes([]byte(code[index])) || compiled.Source.Size != int64(len(code[index])) || compiled.SourceBundleDigest.Validate() != nil {
			return errors.New("compile evidence differs from the solution source")
		}
		filename := domain.SafeRelPath("main.cpp")
		if content.Language == "go" {
			filename = "main.go"
		}
		bundleDigest, err := port.ComputeSourceBundleDigest(port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: filename, Files: []port.SourceFile{{Path: filename, Blob: compiled.Source}}})
		if err != nil || bundleDigest != compiled.SourceBundleDigest {
			return errors.New("compile evidence changed its source bundle")
		}
		if err := compiled.Result.Validate(); err != nil {
			return err
		}
		if compiled.Result.Outcome == domain.CompileInfraError {
			return errors.New("infrastructure failure is not a content verdict")
		}
		if compiled.Result.Outcome != domain.CompileOK {
			failure = fmt.Sprintf("compile.%s.%s", compiled.Role, compiled.Result.Outcome)
		}
	}
	if len(r.Samples) > 2*len(input.Problem.Samples) || (len(r.Samples) > 0 && (len(r.Compiles) != 2 || failure != "")) {
		return errors.New("samples ran without successful compilation")
	}
	for index, sample := range r.Samples {
		if failure != "" || sample.Sample != index/2+1 || sample.Role != roles[index%2] {
			return errors.New("sample evidence is incomplete or out of order")
		}
		problemSample := input.Problem.Samples[index/2]
		if sample.Input != (domain.BlobRef{Digest: domain.SumBytes([]byte(problemSample.Input)), Size: int64(len(problemSample.Input))}) || sample.Expected != (domain.BlobRef{Digest: domain.SumBytes([]byte(problemSample.Output)), Size: int64(len(problemSample.Output))}) {
			return errors.New("sample evidence differs from the problem")
		}
		adapted, err := judge.AdaptSolution(sample.Result)
		if err != nil {
			return err
		}
		if adapted.InfrastructureFailure || adapted.Outcome == nil {
			return errors.New("sample infrastructure failure is not a content verdict")
		}
		verdict := string(*adapted.Outcome)
		if *adapted.Outcome == domain.SolutionOK {
			if sample.Result.Stdout == nil || sample.Result.Stdout.Blob.Size > 1<<20 || sample.ActualTokenDigest.Validate() != nil || sample.Matches != (sample.ActualTokenDigest == solutionTokenDigest([]byte(problemSample.Output))) {
				return errors.New("sample token comparison is inconsistent")
			}
			if !sample.Matches {
				verdict = "WA"
			}
		} else if sample.Matches || sample.ActualTokenDigest != "" {
			return errors.New("failed process claimed matching output")
		}
		if !sample.Matches {
			failure = fmt.Sprintf("sample.%d.%s.%s", sample.Sample, sample.Role, verdict)
		}
	}
	if r.Passed {
		if failure != "" || r.Reason != "" || len(r.Compiles) != 2 || len(r.Samples) != 2*len(input.Problem.Samples) {
			return errors.New("passing solution report omits required checks")
		}
	} else if failure == "" || r.Reason != failure {
		return errors.New("failed solution report lacks its exact first failure")
	}
	return nil
}
