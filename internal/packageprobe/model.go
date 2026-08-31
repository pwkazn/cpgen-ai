package packageprobe

import (
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"cpgen/internal/domain"
)

const (
	PackageSchemaVersion          domain.SchemaVersion = "cpgen.package/v1"
	SamplesSchemaVersion          domain.SchemaVersion = "cpgen.samples/v1"
	SimilarityReportSchemaVersion domain.SchemaVersion = "cpgen.similarity-report/v1"
	PrePackageReportSchemaVersion domain.SchemaVersion = "cpgen.prepackage-report/v1"
	ProvenanceSchemaVersion       domain.SchemaVersion = "cpgen.provenance/v1"
)

type FileRole string

const (
	RoleManifest         FileRole = "manifest"
	RoleStatement        FileRole = "statement"
	RoleSamples          FileRole = "samples"
	RoleEditorial        FileRole = "editorial"
	RoleReference        FileRole = "reference"
	RoleBrute            FileRole = "brute"
	RoleValidator        FileRole = "validator"
	RoleChecker          FileRole = "checker"
	RoleGenerator        FileRole = "generator"
	RoleTestInput        FileRole = "test_input"
	RoleTestAnswer       FileRole = "test_answer"
	RoleSimilarityReport FileRole = "similarity_report"
	RolePrePackageReport FileRole = "prepackage_report"
	RoleProvenance       FileRole = "provenance"
)

func (r FileRole) Valid() bool {
	switch r {
	case RoleStatement, RoleSamples, RoleEditorial, RoleReference, RoleBrute, RoleValidator, RoleChecker,
		RoleGenerator, RoleTestInput, RoleTestAnswer, RoleSimilarityReport, RolePrePackageReport, RoleProvenance:
		return true
	default:
		return false
	}
}

type ProblemInfo struct {
	Slug                string `json:"slug"`
	Title               string `json:"title"`
	Language            string `json:"language"`
	ProblemSpecRevision int64  `json:"problem_spec_revision"`
}

type ProblemLimits struct {
	TimeMS      int64 `json:"time_ms"`
	MemoryMB    int64 `json:"memory_mb"`
	OutputBytes int64 `json:"output_bytes"`
}

type CheckerConfig struct {
	Kind         string             `json:"kind"`
	Protocol     string             `json:"protocol"`
	ArtifactPath domain.SafeRelPath `json:"artifact_path"`
}

type TestGroup struct {
	Name         string   `json:"name"`
	Tests        []string `json:"tests"`
	CoverageTags []string `json:"coverage_tags"`
}

type Verification struct {
	Profile              string             `json:"profile"`
	PrePackageReportPath domain.SafeRelPath `json:"prepackage_report_path"`
	EnvironmentDigest    domain.Digest      `json:"environment_digest"`
}

type ProblemFile struct {
	Path domain.SafeRelPath
	Role FileRole
	Data []byte
}

type Problem struct {
	RunID                   domain.RunID
	Problem                 ProblemInfo
	Limits                  ProblemLimits
	Checker                 CheckerConfig
	ToolchainManifestDigest domain.Digest
	TestGroups              []TestGroup
	Verification            Verification
	ProvenancePath          domain.SafeRelPath
	Files                   []ProblemFile
}

type FileEntry struct {
	Path   domain.SafeRelPath `json:"path"`
	SHA256 domain.Digest      `json:"sha256"`
	Size   int64              `json:"size"`
	Role   FileRole           `json:"role"`
}

type Manifest struct {
	SchemaVersion           domain.SchemaVersion `json:"schema_version"`
	PackageID               domain.Digest        `json:"package_id"`
	RunID                   domain.RunID         `json:"run_id"`
	Problem                 ProblemInfo          `json:"problem"`
	Limits                  ProblemLimits        `json:"limits"`
	Checker                 CheckerConfig        `json:"checker"`
	ToolchainManifestDigest domain.Digest        `json:"toolchain_manifest_digest"`
	TestGroups              []TestGroup          `json:"test_groups"`
	Files                   []FileEntry          `json:"files"`
	Verification            Verification         `json:"verification"`
	ProvenancePath          domain.SafeRelPath   `json:"provenance_path"`
}

type VerifiedFile struct {
	Entry FileEntry
	Bytes []byte
}

type VerifiedProblem struct {
	Manifest Manifest
	Files    []VerifiedFile
}

type ReadLimits struct {
	MaxFiles         int
	MaxPathBytes     int
	MaxFileBytes     int64
	MaxTotalBytes    int64
	MaxManifestBytes int64
}

func (l ReadLimits) Validate() error {
	if l.MaxFiles <= 0 || l.MaxPathBytes <= 0 || l.MaxFileBytes <= 0 || l.MaxTotalBytes <= 0 || l.MaxManifestBytes <= 0 {
		return fmt.Errorf("all package read limits must be positive")
	}
	if l.MaxManifestBytes > l.MaxTotalBytes {
		return fmt.Errorf("manifest limit exceeds total package limit")
	}
	return nil
}

func (p Problem) Validate(maxPathBytes int) error {
	if err := validateManifestMetadata(p.RunID, p.Problem, p.Limits, p.Checker, p.ToolchainManifestDigest, p.TestGroups, p.Verification, p.ProvenancePath); err != nil {
		return err
	}
	paths := make([]domain.SafeRelPath, len(p.Files))
	entries := make([]FileEntry, len(p.Files))
	bytesByPath := make(map[domain.SafeRelPath][]byte, len(p.Files))
	for index, file := range p.Files {
		paths[index] = file.Path
		entries[index] = FileEntry{Path: file.Path, SHA256: domain.SumBytes(file.Data), Size: int64(len(file.Data)), Role: file.Role}
		bytesByPath[file.Path] = file.Data
	}
	if err := validatePackagePaths(paths, maxPathBytes); err != nil {
		return err
	}
	if err := validateFileEntries(entries, p.TestGroups, p.Checker, p.Verification, p.ProvenancePath); err != nil {
		return err
	}
	return validateEmbeddedDTOs(bytesByPath, p.RunID, p.Problem.ProblemSpecRevision, p.Verification.Profile)
}

func (m Manifest) Validate(maxPathBytes int) error {
	if m.SchemaVersion != PackageSchemaVersion {
		return fmt.Errorf("package schema must be %q", PackageSchemaVersion)
	}
	if err := m.PackageID.Validate(); err != nil {
		return fmt.Errorf("package ID: %w", err)
	}
	if err := validateManifestMetadata(m.RunID, m.Problem, m.Limits, m.Checker, m.ToolchainManifestDigest, m.TestGroups, m.Verification, m.ProvenancePath); err != nil {
		return err
	}
	paths := make([]domain.SafeRelPath, len(m.Files))
	for index, entry := range m.Files {
		paths[index] = entry.Path
		if entry.Path == "manifest.json" || entry.Role == RoleManifest {
			return fmt.Errorf("manifest.json cannot be a manifest file entry")
		}
		if err := entry.SHA256.Validate(); err != nil || entry.Size < 0 || !entry.Role.Valid() {
			return fmt.Errorf("invalid manifest file entry %q", entry.Path)
		}
		if index > 0 && m.Files[index-1].Path >= entry.Path {
			return fmt.Errorf("manifest file entries are not in canonical path order")
		}
	}
	if err := validatePackagePaths(paths, maxPathBytes); err != nil {
		return err
	}
	if err := validateFileEntries(m.Files, m.TestGroups, m.Checker, m.Verification, m.ProvenancePath); err != nil {
		return err
	}
	computed, err := ComputePackageID(m)
	if err != nil {
		return err
	}
	if computed != m.PackageID {
		return fmt.Errorf("package ID mismatch: got %s, computed %s", m.PackageID, computed)
	}
	return nil
}

func (p VerifiedProblem) Validate() error {
	if err := p.Manifest.Validate(1 << 20); err != nil {
		return err
	}
	if len(p.Files) != len(p.Manifest.Files) {
		return fmt.Errorf("verified problem file count mismatch")
	}
	for index, file := range p.Files {
		if file.Entry != p.Manifest.Files[index] || int64(len(file.Bytes)) != file.Entry.Size || domain.SumBytes(file.Bytes) != file.Entry.SHA256 {
			return fmt.Errorf("verified problem file %d does not match its manifest entry", index)
		}
	}
	bytesByPath := make(map[domain.SafeRelPath][]byte, len(p.Files))
	for _, file := range p.Files {
		bytesByPath[file.Entry.Path] = file.Bytes
	}
	return validateEmbeddedDTOs(bytesByPath, p.Manifest.RunID, p.Manifest.Problem.ProblemSpecRevision, p.Manifest.Verification.Profile)
}

var slugPattern = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
var testPathPattern = regexp.MustCompile(`^tests/([A-Za-z0-9_-]+)\.(in|ans)$`)

func validateManifestMetadata(runID domain.RunID, problem ProblemInfo, limits ProblemLimits, checker CheckerConfig, toolchain domain.Digest, groups []TestGroup, verification Verification, provenance domain.SafeRelPath) error {
	if err := runID.Validate(); err != nil {
		return err
	}
	if !slugPattern.MatchString(problem.Slug) || strings.TrimSpace(problem.Title) == "" || problem.Language != "zh-CN" || problem.ProblemSpecRevision <= 0 {
		return fmt.Errorf("invalid package problem metadata")
	}
	if limits.TimeMS <= 0 || limits.MemoryMB <= 0 || limits.OutputBytes <= 0 {
		return fmt.Errorf("package limits must be positive")
	}
	if checker.Kind != "token" || checker.Protocol != "testlib_v1" || checker.ArtifactPath != "judge/checker.cpp" {
		return fmt.Errorf("unsupported checker configuration")
	}
	if err := toolchain.Validate(); err != nil {
		return err
	}
	if len(groups) == 0 {
		return fmt.Errorf("at least one test group is required")
	}
	if verification.Profile != "mvp" || verification.PrePackageReportPath != "reports/prepackage-quality.json" {
		return fmt.Errorf("invalid verification metadata")
	}
	if err := verification.EnvironmentDigest.Validate(); err != nil {
		return err
	}
	if provenance != "reports/provenance.json" {
		return fmt.Errorf("invalid provenance path")
	}
	return nil
}

func validateFileEntries(entries []FileEntry, groups []TestGroup, checker CheckerConfig, verification Verification, provenance domain.SafeRelPath) error {
	required := map[domain.SafeRelPath]FileRole{
		"statement/zh-CN.md": RoleStatement, "statement/samples.json": RoleSamples,
		"solution/editorial.md": RoleEditorial, "solution/reference.cpp": RoleReference, "solution/brute.cpp": RoleBrute,
		"judge/validator.cpp": RoleValidator, checker.ArtifactPath: RoleChecker, "judge/generator.cpp": RoleGenerator,
		"reports/similarity.json": RoleSimilarityReport, verification.PrePackageReportPath: RolePrePackageReport, provenance: RoleProvenance,
	}
	tests := map[string]map[string]bool{}
	seen := map[domain.SafeRelPath]struct{}{}
	for _, entry := range entries {
		if _, exists := seen[entry.Path]; exists {
			return fmt.Errorf("duplicate package path %q", entry.Path)
		}
		seen[entry.Path] = struct{}{}
		if role, ok := required[entry.Path]; ok {
			if entry.Role != role {
				return fmt.Errorf("path %q has role %q, want %q", entry.Path, entry.Role, role)
			}
			delete(required, entry.Path)
			continue
		}
		match := testPathPattern.FindStringSubmatch(string(entry.Path))
		if len(match) != 3 {
			return fmt.Errorf("undeclared package path %q", entry.Path)
		}
		wantRole := RoleTestInput
		if match[2] == "ans" {
			wantRole = RoleTestAnswer
		}
		if entry.Role != wantRole {
			return fmt.Errorf("test path %q has the wrong role", entry.Path)
		}
		if tests[match[1]] == nil {
			tests[match[1]] = map[string]bool{}
		}
		tests[match[1]][match[2]] = true
	}
	if len(required) != 0 || len(tests) == 0 {
		return fmt.Errorf("package is missing required structural files")
	}
	for id, pair := range tests {
		if !pair["in"] || !pair["ans"] {
			return fmt.Errorf("test %q does not have an input/answer pair", id)
		}
	}
	groupIDs := map[string]struct{}{}
	for _, group := range groups {
		if group.Name == "" || len(group.Tests) == 0 || len(group.CoverageTags) == 0 {
			return fmt.Errorf("invalid test group")
		}
		for _, id := range group.Tests {
			if _, duplicate := groupIDs[id]; duplicate {
				return fmt.Errorf("duplicate test ID %q", id)
			}
			if tests[id] == nil {
				return fmt.Errorf("test group references missing test %q", id)
			}
			groupIDs[id] = struct{}{}
		}
	}
	if len(groupIDs) != len(tests) {
		return fmt.Errorf("not every test pair belongs to exactly one group")
	}
	return nil
}

type SamplesDocument struct {
	SchemaVersion domain.SchemaVersion `json:"schema_version"`
	Samples       []Sample             `json:"samples"`
}

type Sample struct {
	Input  string `json:"input"`
	Output string `json:"output"`
}

type SimilarityReport struct {
	SchemaVersion  domain.SchemaVersion `json:"schema_version"`
	EvidenceDigest domain.Digest        `json:"evidence_digest"`
	DecisionDigest domain.Digest        `json:"decision_digest"`
	Matches        []SimilarityMatch    `json:"matches"`
}

type SimilarityMatch struct {
	ID     string  `json:"id"`
	Title  string  `json:"title"`
	Source string  `json:"source"`
	URL    string  `json:"url"`
	Score  float64 `json:"score"`
}

type PrePackageQualityReport struct {
	SchemaVersion       domain.SchemaVersion `json:"schema_version"`
	RunID               domain.RunID         `json:"run_id"`
	Profile             string               `json:"profile"`
	ProblemSpecRevision int64                `json:"problem_spec_revision"`
	Status              string               `json:"status"`
}

type Provenance struct {
	SchemaVersion  domain.SchemaVersion `json:"schema_version"`
	RunID          domain.RunID         `json:"run_id"`
	Source         string               `json:"source"`
	VerticalDigest domain.Digest        `json:"vertical_digest"`
}

func validateEmbeddedDTOs(files map[domain.SafeRelPath][]byte, runID domain.RunID, revision int64, profile string) error {
	var samples SamplesDocument
	if err := decodeStrict(files["statement/samples.json"], &samples); err != nil || samples.SchemaVersion != SamplesSchemaVersion || len(samples.Samples) == 0 {
		return fmt.Errorf("invalid samples document: %w", err)
	}
	var similarity SimilarityReport
	if err := decodeStrict(files["reports/similarity.json"], &similarity); err != nil || similarity.SchemaVersion != SimilarityReportSchemaVersion {
		return fmt.Errorf("invalid package-safe similarity report: %w", err)
	}
	if err := similarity.EvidenceDigest.Validate(); err != nil {
		return err
	}
	if err := similarity.DecisionDigest.Validate(); err != nil {
		return err
	}
	for _, match := range similarity.Matches {
		parsed, err := url.Parse(match.URL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || match.ID == "" || match.Title == "" || match.Source == "" || match.Score < 0 || match.Score > 1 {
			return fmt.Errorf("similarity report contains unsafe match metadata")
		}
	}
	var prepackage PrePackageQualityReport
	if err := decodeStrict(files["reports/prepackage-quality.json"], &prepackage); err != nil || prepackage.SchemaVersion != PrePackageReportSchemaVersion || prepackage.RunID != runID || prepackage.Profile != profile || prepackage.ProblemSpecRevision != revision || prepackage.Status != "PASSED" {
		return fmt.Errorf("invalid prepackage quality report: %w", err)
	}
	var provenance Provenance
	if err := decodeStrict(files["reports/provenance.json"], &provenance); err != nil || provenance.SchemaVersion != ProvenanceSchemaVersion || provenance.RunID != runID || provenance.Source != "slice0-probe" {
		return fmt.Errorf("invalid provenance report: %w", err)
	}
	return provenance.VerticalDigest.Validate()
}

func cloneGroups(input []TestGroup) []TestGroup {
	result := make([]TestGroup, len(input))
	for index, group := range input {
		result[index] = group
		result[index].Tests = slices.Clone(group.Tests)
		result[index].CoverageTags = slices.Clone(group.CoverageTags)
	}
	return result
}
