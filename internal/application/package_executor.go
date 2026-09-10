package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/packageprobe"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

const generationPackageProducer = "mvp-package"

type PackageStageResult struct {
	Binding     domain.VerifiedPackageBinding
	Occurrences []domain.PendingOccurrence
}

func packagePublicationIdentity(attempt domain.StageAttempt, version int64) port.SandboxAuthorizationIdentity {
	const operation = "package-publication"
	return port.SandboxAuthorizationIdentity{RunID: attempt.RunID, StageName: attempt.StageName, AttemptID: attempt.AttemptID, SandboxExecutionID: domain.SandboxExecutionID(coordinatorMutationID("sandbox", operation, attempt.RunID, attempt.AttemptID, attempt.InputDigest)), LogicalOperationID: operation, Kind: domain.CallSandboxCompile, ScopeDigest: attempt.InputDigest, ExpectedRunVersion: version}
}

func (s *PackageExecutor) Run(ctx context.Context, view domain.RunView) (PackageStageResult, error) {
	var empty PackageStageResult
	problem, quality, err := s.Assemble(ctx, view)
	if err != nil {
		return empty, err
	}
	digest, err := stableValueDigest(quality)
	if err != nil {
		return empty, err
	}
	attempt, err := s.quality.data.generation.admit(ctx, view, "package", digest)
	if err != nil {
		return empty, err
	}
	raw, manifest, err := packageprobe.BuildArchive(ctx, problem)
	if err != nil {
		return empty, err
	}
	if int64(len(raw)) > view.Budget().Limits.MaxPackageBytes {
		return empty, errors.New("package exceeds the run package byte limit")
	}
	verified, err := packageprobe.ReadArchive(ctx, raw)
	if err != nil || verified.Manifest.PackageID != manifest.PackageID {
		return empty, errors.Join(err, errors.New("package archive failed round-trip verification"))
	}
	manifestRaw, err := packageprobe.EncodeManifest(manifest)
	if err != nil {
		return empty, err
	}
	config := s.quality.data.generation.config
	publisher, err := NewSandboxArtifactSink(config.Store, config.Blobs, config.Clock, packagePublicationIdentity(attempt, view.Version()))
	if err != nil {
		return empty, err
	}
	pending, err := publisher.Publish(ctx, port.ArtifactDeclaration{LogicalPath: "package/problem.zip", Role: domain.ArtifactOutput, MediaType: "application/zip", MaxBytes: int64(len(raw)), Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.package/v2", Producer: generationPackageProducer, InputDigest: &digest}}, raw)
	if err != nil {
		return empty, err
	}
	return PackageStageResult{Binding: domain.VerifiedPackageBinding{PackageID: manifest.PackageID, ManifestDigest: domain.SumBytes(manifestRaw), QualityDigest: digest, Archive: pending.Blob}, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending}}}, nil
}

type PackageExecutor struct{ quality *QualityExecutor }

func NewPackageExecutor(quality *QualityExecutor) (*PackageExecutor, error) {
	if quality == nil {
		return nil, errors.New("package requires the current Quality executor")
	}
	return &PackageExecutor{quality}, nil
}

// Assemble deliberately reconstructs every committed upstream proof. Package
// DTOs and a caller-supplied PASSED value cannot grant publication authority.
func (s *PackageExecutor) Assemble(ctx context.Context, view domain.RunView) (packageprobe.Problem, QualityReport, error) {
	var empty packageprobe.Problem
	if view.WorkflowRevision() != workflow.MVPWorkflowRevision {
		return empty, QualityReport{}, errors.New("package requires the MVP workflow")
	}
	input, err := s.quality.ReadInput(ctx, view.RunID())
	if err != nil {
		return empty, QualityReport{}, err
	}
	quality, err := s.quality.ReadReport(ctx, view.RunID())
	if err != nil || !quality.Passed {
		return empty, quality, errors.Join(err, errors.New("package requires current passing Quality"))
	}
	current, err := s.quality.data.solution.similarity.ReadCommitted(ctx, view.RunID())
	if err != nil {
		return empty, quality, err
	}
	i := input.JudgeInput
	spec, solution := i.DataInput.SolutionInput.Problem, i.DataInput.Solution
	qualityDigest, err := stableValueDigest(quality)
	if err != nil {
		return empty, quality, err
	}
	p := packageprobe.Problem{
		SchemaVersion: packageprobe.GenerationPackageSchemaVersion, RunID: view.RunID(),
		Problem:                 packageprobe.ProblemInfo{Slug: "problem-" + strings.TrimPrefix(string(spec.SpecDigest), "sha256:")[:16], Title: spec.Title, Language: spec.Language, ProblemSpecRevision: spec.Revision, SolutionLanguage: solution.Language},
		Limits:                  packageprobe.ProblemLimits{TimeMS: spec.TimeLimitMS, MemoryMB: spec.MemoryLimitMB, OutputBytes: 1 << 20},
		Checker:                 packageprobe.CheckerConfig{Kind: "token", Protocol: "testlib_v1", ArtifactPath: "judge/checker.cpp", Comparison: judge.ExactTokenComparisonV1},
		ToolchainManifestDigest: quality.ToolchainLockDigest,
		Verification:            packageprobe.Verification{Profile: "mvp", PrePackageReportPath: "reports/prepackage-quality.json", EnvironmentDigest: quality.ToolchainLockDigest},
		ProvenancePath:          "reports/provenance.json",
	}
	add := func(path string, role packageprobe.FileRole, raw []byte) {
		p.Files = append(p.Files, packageprobe.ProblemFile{Path: domain.SafeRelPath(path), Role: role, Data: raw})
	}
	addJSON := func(path string, role packageprobe.FileRole, value any) error {
		raw, err := json.Marshal(value)
		if err == nil {
			add(path, role, raw)
		}
		return err
	}
	add("statement/statement.md", packageprobe.RoleStatement, []byte(renderPackageStatement(spec)))
	samples := packageprobe.SamplesDocument{SchemaVersion: packageprobe.SamplesSchemaVersion, Samples: []packageprobe.Sample{}}
	for _, sample := range spec.Samples {
		samples.Samples = append(samples.Samples, packageprobe.Sample{Input: sample.Input, Output: sample.Output})
	}
	if err := addJSON("statement/samples.json", packageprobe.RoleSamples, samples); err != nil {
		return empty, quality, err
	}
	ext := solution.Language
	add("solution/editorial.md", packageprobe.RoleEditorial, []byte(solution.Explanation))
	add("solution/reference."+ext, packageprobe.RoleReference, []byte(solution.ReferenceCode))
	add("solution/brute."+ext, packageprobe.RoleBrute, []byte(solution.BruteCode))
	add("judge/generator."+ext, packageprobe.RoleGenerator, []byte(i.Data.GeneratorCode))
	add("judge/validator."+ext, packageprobe.RoleValidator, []byte(i.Data.ValidatorCode))
	add("judge/checker.cpp", packageprobe.RoleChecker, judge.ExactTokenCheckerSource())
	index := packageprobe.GenerationTestsDocument{SchemaVersion: "cpgen.package-tests/v1", DataContentDigest: i.Data.ContentDigest, JudgeReportDigest: quality.InputDigest, Plan: i.Data.Plan}
	group := packageprobe.TestGroup{Name: "main", Tests: []string{}, CoverageTags: []string{"sample", "small", "boundary", "stress"}}
	for _, item := range input.JudgeReport.Cases {
		origin := item.Input.Origin
		if origin == "samples" {
			origin = "sample"
		}
		id := fmt.Sprintf("%s-%03d", origin, item.Input.Ordinal)
		if item.Answer == nil {
			return empty, quality, errors.New("package case has no verified answer")
		}
		for _, file := range []struct {
			suffix string
			role   packageprobe.FileRole
			ref    domain.BlobRef
		}{{".in", packageprobe.RoleTestInput, item.Input.Input}, {".ans", packageprobe.RoleTestAnswer, *item.Answer}} {
			raw, err := readPackageBlob(ctx, s.quality.data.generation.config.Blobs, file.ref)
			if err != nil {
				return empty, quality, err
			}
			add("tests/"+id+file.suffix, file.role, raw)
		}
		index.Tests = append(index.Tests, packageprobe.GenerationTest{ID: id, Origin: origin, Ordinal: item.Input.Ordinal, Kind: item.Input.Kind, Seed: item.Input.Seed, Input: item.Input.Input, Answer: *item.Answer})
		group.Tests = append(group.Tests, id)
	}
	p.TestGroups = []packageprobe.TestGroup{group}
	indexRaw, err := json.Marshal(index)
	if err != nil {
		return empty, quality, err
	}
	add("data/tests.json", packageprobe.RoleTestPlan, indexRaw)
	similarity := packageprobe.SimilarityReport{SchemaVersion: packageprobe.SimilarityReportSchemaVersion, EvidenceDigest: current.Evidence.EvidenceDigest, DecisionDigest: quality.SimilarityDecisionDigest, Matches: []packageprobe.SimilarityMatch{}}
	for _, hit := range current.Evidence.Hits {
		similarity.Matches = append(similarity.Matches, packageprobe.SimilarityMatch{ID: hit.ExternalID, Source: hit.Source, URL: hit.CanonicalURL, Score: hit.Score})
	}
	doc := packageprobe.GenerationQualityDocument{SchemaVersion: "cpgen.prepackage-report/v2", RunID: view.RunID(), Profile: "mvp", ProblemSpecRevision: spec.Revision, Status: "PASSED", QualityReportDigest: qualityDigest, JudgeReportDigest: quality.InputDigest, DataVerificationDigest: quality.DataVerificationDigest, SolutionVerificationDigest: quality.SolutionVerificationDigest, ProblemSpecDigest: quality.ProblemSpecDigest, SimilarityDecisionDigest: quality.SimilarityDecisionDigest, PolicyDigest: quality.PolicyDigest, CheckerSourceDigest: domain.SumBytes(judge.ExactTokenCheckerSource()), TestsDigest: domain.SumBytes(indexRaw)}
	provenance := packageprobe.GenerationProvenance{SchemaVersion: "cpgen.provenance/v2", RunID: view.RunID(), Source: "mvp-generation", WorkflowRevision: view.WorkflowRevision(), WorkflowDigest: view.WorkflowDigest(), RequestDigest: view.RequestDigest(), ConfigDigest: view.ConfigDigest(), ProblemSpecDigest: spec.SpecDigest, QualityReportDigest: qualityDigest, ToolchainLockDigest: quality.ToolchainLockDigest}
	for _, file := range []struct {
		path  string
		role  packageprobe.FileRole
		value any
	}{{"reports/similarity.json", packageprobe.RoleSimilarityReport, similarity}, {"reports/prepackage-quality.json", packageprobe.RolePrePackageReport, doc}, {"reports/provenance.json", packageprobe.RoleProvenance, provenance}} {
		if err := addJSON(file.path, file.role, file.value); err != nil {
			return empty, quality, err
		}
	}
	return p, quality, p.Validate(240)
}

func readPackageBlob(ctx context.Context, blobs port.VerifiedBlobReader, ref domain.BlobRef) ([]byte, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if ref.Size > 1<<20 {
		return nil, errors.New("package test exceeds byte bound")
	}
	r, err := blobs.OpenVerified(ctx, ref)
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(r, ref.Size+1))
	if err := errors.Join(err, r.Close()); err != nil {
		return nil, err
	}
	if int64(len(raw)) != ref.Size || domain.SumBytes(raw) != ref.Digest {
		return nil, errors.New("package blob differs")
	}
	return raw, nil
}

func renderPackageStatement(spec domain.ProblemSpec) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n%s\n\n## Input\n\n%s\n", spec.Title, spec.Description, spec.Input.Description)
	for _, field := range spec.Input.Fields {
		fmt.Fprintf(&out, "\n- %s\n", field)
	}
	fmt.Fprintf(&out, "\n## Output\n\n%s\n", spec.Output.Description)
	for _, field := range spec.Output.Fields {
		fmt.Fprintf(&out, "\n- %s\n", field)
	}
	fmt.Fprintf(&out, "\n## Limits\n\n%d ms; %d MB\n", spec.TimeLimitMS, spec.MemoryLimitMB)
	for i, sample := range spec.Samples {
		fmt.Fprintf(&out, "\n## Sample %d\n\nInput:\n\n%s\n\nOutput:\n\n%s\n", i+1, fencedSample(sample.Input), fencedSample(sample.Output))
		if sample.Explanation != "" {
			fmt.Fprintf(&out, "\n%s\n", sample.Explanation)
		}
	}
	return out.String()
}

func fencedSample(raw string) string {
	fence := "```"
	for strings.Contains(raw, fence) {
		fence += "`"
	}
	return fence + "text\n" + raw + "\n" + fence
}
