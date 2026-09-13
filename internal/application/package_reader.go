package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/packageprobe"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

type PackageReader struct {
	quality    *QualityReader
	similarity CommittedSimilarityReader
	store      PackageEvidenceReadStore
	blobs      port.VerifiedBlobReader
}

// Assemble reconstructs every committed upstream proof before granting publication authority.
func (s *PackageReader) Assemble(ctx context.Context, view domain.RunView) (packageprobe.Problem, QualityReport, error) {
	var empty packageprobe.Problem
	if view.WorkflowRevision() != workflow.GenerationRevision {
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
	current, err := s.similarity.ReadCommitted(ctx, view.RunID())
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
			raw, err := readPackageBlob(ctx, s.blobs, file.ref)
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

// ReadArchive is a read-only export boundary. A package ledger entry does not
// bypass current source proof: reconstruct the complete expected archive and
// require the committed bytes, manifest and final run binding to match it.
func (s *PackageReader) ReadArchive(ctx context.Context, runID domain.RunID) ([]byte, domain.VerifiedPackageRecord, error) {
	var empty domain.VerifiedPackageRecord
	store := s.store
	run, err := store.GetRun(ctx, runID)
	if err != nil {
		return nil, empty, err
	}
	if run.State != domain.RunReady || run.FinalPackageOccurrenceID == nil {
		return nil, empty, errors.New("package export requires READY")
	}
	record, err := store.ReadVerifiedPackage(ctx, runID)
	if err != nil {
		return nil, empty, err
	}
	if record.Validate() != nil || record.RunID != runID || record.OccurrenceID != *run.FinalPackageOccurrenceID {
		return nil, empty, errors.New("package record differs from final run binding")
	}
	budget, err := store.BudgetSnapshot(ctx, runID)
	if err != nil {
		return nil, empty, err
	}
	view, err := domain.NewRunViewFromSnapshot(run, budget, nil)
	if err != nil {
		return nil, empty, err
	}
	problem, quality, err := s.Assemble(ctx, view)
	if err != nil {
		return nil, empty, err
	}
	qualityDigest, err := stableValueDigest(quality)
	if err != nil || qualityDigest != record.Binding.QualityDigest {
		return nil, empty, errors.New("package lost its current Quality report")
	}
	expected, manifest, err := packageprobe.BuildArchive(ctx, problem)
	if err != nil {
		return nil, empty, err
	}
	manifestRaw, err := packageprobe.EncodeManifest(manifest)
	if err != nil {
		return nil, empty, err
	}
	if manifest.PackageID != record.Binding.PackageID || domain.SumBytes(manifestRaw) != record.Binding.ManifestDigest || domain.SumBytes(expected) != record.Binding.Archive.Digest || int64(len(expected)) != record.Binding.Archive.Size {
		return nil, empty, errors.New("package differs from reconstructed current artifacts")
	}
	stage, err := store.ReadCommittedSandboxStage(ctx, runID, "package")
	if err != nil {
		return nil, empty, err
	}
	if stage.Attempt.AttemptID != record.AttemptID || stage.Attempt.InputDigest != qualityDigest || stage.Attempt.OutputDigest == nil || *stage.Attempt.OutputDigest != record.Binding.Archive.Digest || len(stage.Artifacts) != 1 {
		return nil, empty, errors.New("package stage differs from verified record")
	}
	item := stage.Artifacts[0]
	if item.OccurrenceID != record.OccurrenceID || item.Blob.Blob != record.Binding.Archive || item.Blob.LogicalPath != "package/problem.zip" || item.Blob.Role != domain.ArtifactOutput || item.Blob.MediaType != "application/zip" || item.Blob.Provenance.SchemaVersion != "cpgen.package/v2" || item.Blob.Provenance.Producer != generationPackageProducer || item.Blob.Provenance.InputDigest == nil || *item.Blob.Provenance.InputDigest != qualityDigest {
		return nil, empty, errors.New("package archive provenance differs")
	}
	raw, err := readSolutionVerificationBlob(ctx, s.blobs, record.Binding.Archive, 65<<20)
	if err != nil {
		return nil, empty, err
	}
	if !bytes.Equal(raw, expected) {
		return nil, empty, errors.New("committed package bytes differ")
	}
	if _, err := packageprobe.ReadArchive(ctx, raw); err != nil {
		return nil, empty, err
	}
	return raw, record, nil
}
