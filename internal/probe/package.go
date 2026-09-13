//go:build cpgen_slice0_probe

package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/fixture/ab"
	"cpgen/internal/packageprobe"
)

const StructuralPackageReportKind = "PROBE_STRUCTURAL_ONLY"

type StructuralPackageReport struct {
	Kind           string                   `json:"kind"`
	Status         string                   `json:"status"`
	PackageID      domain.Digest            `json:"package_id"`
	ManifestDigest domain.Digest            `json:"manifest_digest"`
	FileCount      int                      `json:"file_count"`
	ArtifactCallID domain.AttemptCallID     `json:"artifact_call_id"`
	Artifacts      []domain.PendingArtifact `json:"artifacts"`
}

// Artifact returns the immutable evidence entry for one package path. Keeping
// this lookup on the report makes crash/restart canaries inspect persisted
// package evidence without reaching into the package builder or its storage
// implementation.
func (r StructuralPackageReport) Artifact(logicalPath string) (domain.PendingArtifact, bool) {
	for _, artifact := range r.Artifacts {
		if string(artifact.LogicalPath) == logicalPath {
			return artifact, true
		}
	}
	return domain.PendingArtifact{}, false
}

func (r StructuralPackageReport) Validate() error {
	if r.Kind != StructuralPackageReportKind || r.Status != "PASSED" || r.FileCount <= 0 {
		return fmt.Errorf("invalid structural-only package report")
	}
	if err := r.PackageID.Validate(); err != nil {
		return err
	}
	if err := r.ManifestDigest.Validate(); err != nil {
		return err
	}
	if err := r.ArtifactCallID.Validate(); err != nil {
		return err
	}
	if len(r.Artifacts) != r.FileCount+1 {
		return fmt.Errorf("structural package artifact count is incomplete")
	}
	manifestFound := false
	seenPaths := make(map[domain.SafeRelPath]struct{}, len(r.Artifacts))
	for _, artifact := range r.Artifacts {
		if err := artifact.Validate(); err != nil {
			return err
		}
		if _, exists := seenPaths[artifact.LogicalPath]; exists {
			return fmt.Errorf("structural package contains duplicate artifact path %q", artifact.LogicalPath)
		}
		seenPaths[artifact.LogicalPath] = struct{}{}
		if artifact.CallID != r.ArtifactCallID {
			return fmt.Errorf("structural package artifact call identity mismatch")
		}
		if artifact.LogicalPath == "manifest.json" {
			manifestFound = artifact.Blob.Digest == r.ManifestDigest
		}
	}
	if !manifestFound {
		return fmt.Errorf("structural package manifest artifact is missing")
	}
	return nil
}

func (h *Harness) RunStructuralPackage(ctx context.Context, root *os.Root, vertical VerticalReport) (StructuralPackageReport, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if root == nil {
		return StructuralPackageReport{}, fmt.Errorf("structural package root is required")
	}
	if err := vertical.Validate(); err != nil || vertical.Status != VerticalPassed {
		return StructuralPackageReport{}, fmt.Errorf("structural package requires a passed vertical report: %w", err)
	}
	problem, err := h.syntheticPackageProblem(vertical)
	if err != nil {
		return StructuralPackageReport{}, err
	}
	manifest, err := packageprobe.Build(ctx, root, problem)
	if err != nil {
		return StructuralPackageReport{}, err
	}
	limits := packageprobe.ReadLimits{MaxFiles: 128, MaxPathBytes: 240, MaxFileBytes: 16 << 20, MaxTotalBytes: 64 << 20, MaxManifestBytes: 1 << 20}
	inspected, err := packageprobe.StructuralGate(ctx, root, limits)
	if err != nil {
		return StructuralPackageReport{}, err
	}
	if inspected.Manifest.PackageID != manifest.PackageID {
		return StructuralPackageReport{}, fmt.Errorf("built and inspected package identities differ")
	}
	callID, err := newAttemptCallID()
	if err != nil {
		return StructuralPackageReport{}, err
	}
	if err := h.artifacts.configureProbeArtifactCall(callID); err != nil {
		return StructuralPackageReport{}, err
	}
	imported, artifacts, err := packageprobe.Import(ctx, root, limits, h.artifacts)
	if err != nil {
		return StructuralPackageReport{}, err
	}
	if imported.Manifest.PackageID != manifest.PackageID {
		return StructuralPackageReport{}, fmt.Errorf("imported package identity differs from the built package")
	}
	manifestDigest := domain.Digest("")
	for _, artifact := range artifacts {
		if artifact.LogicalPath == "manifest.json" {
			manifestDigest = artifact.Blob.Digest
		}
	}
	report := StructuralPackageReport{
		Kind: StructuralPackageReportKind, Status: "PASSED", PackageID: manifest.PackageID,
		ManifestDigest: manifestDigest, FileCount: len(manifest.Files), ArtifactCallID: callID, Artifacts: artifacts,
	}
	if err := report.Validate(); err != nil {
		return StructuralPackageReport{}, err
	}
	return report, nil
}

func (h *Harness) syntheticPackageProblem(vertical VerticalReport) (packageprobe.Problem, error) {
	const packageRunID domain.RunID = "run_00000000000000000000000000000009"
	verticalProjection := struct {
		Programs             map[string]domain.BlobRef
		GeneratedCasesDigest domain.Digest
		ValidatorLegal       domain.ValidatorOutcome
		ValidatorIllegal     domain.ValidatorOutcome
		Cases                []VerticalCaseReport
		CheckerAttacks       map[string]domain.CheckerOutcome
	}{
		Programs: vertical.Programs, GeneratedCasesDigest: vertical.GeneratedCasesDigest,
		ValidatorLegal: vertical.ValidatorLegal, ValidatorIllegal: vertical.ValidatorIllegal,
		Cases: vertical.Cases, CheckerAttacks: vertical.CheckerAttacks,
	}
	projectionBytes, err := json.Marshal(verticalProjection)
	if err != nil {
		return packageprobe.Problem{}, err
	}
	verticalDigest := domain.SumBytes(projectionBytes)
	lockBytes, err := json.Marshal(h.lock)
	if err != nil {
		return packageprobe.Problem{}, err
	}
	samplesBytes, err := json.Marshal(packageprobe.SamplesDocument{
		SchemaVersion: packageprobe.SamplesSchemaVersion,
		Samples:       []packageprobe.Sample{{Input: "1 2\n", Output: "3\n"}},
	})
	if err != nil {
		return packageprobe.Problem{}, err
	}
	similarityBytes, err := json.Marshal(packageprobe.SimilarityReport{
		SchemaVersion:  packageprobe.SimilarityReportSchemaVersion,
		EvidenceDigest: domain.SumBytes([]byte("slice0-similarity-evidence-v1")),
		DecisionDigest: domain.SumBytes([]byte("slice0-similarity-decision-v1")), Matches: []packageprobe.SimilarityMatch{},
	})
	if err != nil {
		return packageprobe.Problem{}, err
	}
	prepackageBytes, err := json.Marshal(packageprobe.PrePackageQualityReport{
		SchemaVersion: packageprobe.PrePackageReportSchemaVersion, RunID: packageRunID,
		Profile: "mvp", ProblemSpecRevision: 1, Status: "PASSED",
	})
	if err != nil {
		return packageprobe.Problem{}, err
	}
	provenanceBytes, err := json.Marshal(packageprobe.Provenance{
		SchemaVersion: packageprobe.ProvenanceSchemaVersion, RunID: packageRunID,
		Source: "slice0-probe", VerticalDigest: verticalDigest,
	})
	if err != nil {
		return packageprobe.Problem{}, err
	}
	files := []packageprobe.ProblemFile{
		{Path: "statement/zh-CN.md", Role: packageprobe.RoleStatement, Data: []byte("# A+B\n\n给定两个整数，输出它们的和。\n")},
		{Path: "statement/samples.json", Role: packageprobe.RoleSamples, Data: samplesBytes},
		{Path: "solution/editorial.md", Role: packageprobe.RoleEditorial, Data: []byte("Read two signed 64-bit integers and print their sum.\n")},
		{Path: "reports/similarity.json", Role: packageprobe.RoleSimilarityReport, Data: similarityBytes},
		{Path: "reports/prepackage-quality.json", Role: packageprobe.RolePrePackageReport, Data: prepackageBytes},
		{Path: "reports/provenance.json", Role: packageprobe.RoleProvenance, Data: provenanceBytes},
	}
	fixtureRoles := map[string]struct {
		path domain.SafeRelPath
		role packageprobe.FileRole
	}{
		"reference": {path: "solution/reference.cpp", role: packageprobe.RoleReference},
		"brute":     {path: "solution/brute.cpp", role: packageprobe.RoleBrute},
		"validator": {path: "judge/validator.cpp", role: packageprobe.RoleValidator},
		"checker":   {path: "judge/checker.cpp", role: packageprobe.RoleChecker},
		"generator": {path: "judge/generator.cpp", role: packageprobe.RoleGenerator},
	}
	for _, definition := range ab.Definitions() {
		target := fixtureRoles[definition.Name]
		files = append(files, packageprobe.ProblemFile{Path: target.path, Role: target.role, Data: definition.Source})
	}
	groups := packageprobe.TestGroup{Name: "main", CoverageTags: []string{"minimum", "boundaries", "seeded"}}
	for index, test := range ab.GeneratedCases(ab.FixedSeed) {
		id := fmt.Sprintf("%03d", index+1)
		input := strconv.FormatInt(test.A, 10) + " " + strconv.FormatInt(test.B, 10) + "\n"
		answer := strconv.FormatInt(test.A+test.B, 10) + "\n"
		files = append(files,
			packageprobe.ProblemFile{Path: domain.SafeRelPath("tests/" + id + ".in"), Role: packageprobe.RoleTestInput, Data: []byte(input)},
			packageprobe.ProblemFile{Path: domain.SafeRelPath("tests/" + id + ".ans"), Role: packageprobe.RoleTestAnswer, Data: []byte(answer)},
		)
		groups.Tests = append(groups.Tests, id)
	}
	return packageprobe.Problem{
		RunID:                   packageRunID,
		Problem:                 packageprobe.ProblemInfo{Slug: "a-plus-b", Title: "A+B", Language: "zh-CN", ProblemSpecRevision: 1},
		Limits:                  packageprobe.ProblemLimits{TimeMS: int64((2 * time.Second) / time.Millisecond), MemoryMB: 256, OutputBytes: 1 << 20},
		Checker:                 packageprobe.CheckerConfig{Kind: "token", Protocol: "testlib_v1", ArtifactPath: "judge/checker.cpp"},
		ToolchainManifestDigest: domain.SumBytes(lockBytes), TestGroups: []packageprobe.TestGroup{groups},
		Verification: packageprobe.Verification{
			Profile: "mvp", PrePackageReportPath: "reports/prepackage-quality.json",
			EnvironmentDigest: domain.SumBytes([]byte("slice0-synthetic-environment-v1")),
		},
		ProvenancePath: "reports/provenance.json", Files: files,
	}, nil
}
