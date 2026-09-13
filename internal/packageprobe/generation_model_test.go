package packageprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
)

func generationProblem(t *testing.T, language string) Problem {
	t.Helper()
	p := minimalProblem(t)
	p.SchemaVersion = GenerationPackageSchemaVersion
	p.Problem.Language = "en"
	p.Problem.SolutionLanguage = language
	p.Checker.Comparison = judge.ExactTokenComparisonV1
	p.Files = p.Files[:0]
	for _, file := range minimalProblem(t).Files {
		if file.Role == RoleTestInput || file.Role == RoleTestAnswer {
			continue
		}
		if file.Role == RoleStatement {
			file.Path = "statement/statement.md"
		}
		if file.Role == RoleSamples {
			file.Data = packageJSON(t, SamplesDocument{SchemaVersion: SamplesSchemaVersion, Samples: []Sample{{Input: "1 2\n", Output: "3\n"}}})
		}
		if file.Role == RoleChecker {
			file.Data = judge.ExactTokenCheckerSource()
		}
		if language == "go" && (file.Role == RoleReference || file.Role == RoleBrute || file.Role == RoleGenerator || file.Role == RoleValidator) {
			file.Path = domain.SafeRelPath(strings.TrimSuffix(string(file.Path), ".cpp") + ".go")
		}
		p.Files = append(p.Files, file)
	}
	digest := domain.SumBytes([]byte("fixture proof"))
	index := GenerationTestsDocument{SchemaVersion: "cpgen.package-tests/v1", DataContentDigest: digest, JudgeReportDigest: digest, Plan: domain.TestPlanV1{SchemaVersion: domain.TestPlanSchemaV1, EffectiveSeed: 42}}
	seeds := []uint64{2017657698631242862, 14428362291972001661, 3948874171292443791, 17952868614994154545}
	for i, kind := range []domain.DataCaseKind{domain.DataCaseSmall, domain.DataCaseSmall, domain.DataCaseBoundary, domain.DataCaseStress} {
		index.Plan.Cases = append(index.Plan.Cases, domain.DataCase{Ordinal: i + 1, Kind: kind, Purpose: fmt.Sprintf("case %d", i+1), Seed: seeds[i]})
	}
	for i := 0; i < 5; i++ {
		item := GenerationTest{ID: "sample-001", Origin: "sample", Ordinal: 1}
		if i > 0 {
			seed := seeds[i-1]
			item = GenerationTest{ID: fmt.Sprintf("generated-%03d", i), Origin: "generated", Ordinal: i, Kind: index.Plan.Cases[i-1].Kind, Seed: &seed}
		}
		in, out := []byte("1 2\n"), []byte("3\n")
		item.Input = domain.BlobRef{Digest: domain.SumBytes(in), Size: int64(len(in))}
		item.Answer = domain.BlobRef{Digest: domain.SumBytes(out), Size: int64(len(out))}
		index.Tests = append(index.Tests, item)
		p.Files = append(p.Files, ProblemFile{Path: domain.SafeRelPath("tests/" + item.ID + ".in"), Role: RoleTestInput, Data: in}, ProblemFile{Path: domain.SafeRelPath("tests/" + item.ID + ".ans"), Role: RoleTestAnswer, Data: out})
	}
	var ids []string
	for _, item := range index.Tests {
		ids = append(ids, item.ID)
	}
	p.TestGroups = []TestGroup{{Name: "main", Tests: ids, CoverageTags: []string{"sample", "small", "boundary", "stress"}}}
	indexRaw := packageJSON(t, index)
	p.Files = append(p.Files, ProblemFile{Path: "data/tests.json", Role: RoleTestPlan, Data: indexRaw})
	quality := GenerationQualityDocument{SchemaVersion: "cpgen.prepackage-report/v2", RunID: p.RunID, Profile: "mvp", ProblemSpecRevision: 1, Status: "PASSED", QualityReportDigest: digest, JudgeReportDigest: digest, DataVerificationDigest: digest, SolutionVerificationDigest: digest, ProblemSpecDigest: digest, SimilarityDecisionDigest: domain.Digest("sha256:" + strings.Repeat("b", 64)), PolicyDigest: digest, CheckerSourceDigest: domain.SumBytes(judge.ExactTokenCheckerSource()), TestsDigest: domain.SumBytes(indexRaw)}
	provenance := GenerationProvenance{SchemaVersion: "cpgen.provenance/v2", RunID: p.RunID, Source: "mvp-generation", WorkflowRevision: "mvp-fixture", WorkflowDigest: domain.SumBytes([]byte("mvp-fixture")), RequestDigest: digest, ConfigDigest: digest, ProblemSpecDigest: digest, QualityReportDigest: digest, ToolchainLockDigest: p.ToolchainManifestDigest}
	for i := range p.Files {
		switch p.Files[i].Role {
		case RolePrePackageReport:
			p.Files[i].Data = packageJSON(t, quality)
		case RoleProvenance:
			p.Files[i].Data = packageJSON(t, provenance)
		}
	}
	return p
}

func packageJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestGenerationPackageBuildsAndReadsBothSourceLanguages(t *testing.T) {
	for _, language := range []string{"cpp", "go"} {
		for _, statementLanguage := range []string{"en", "zh-CN"} {
			t.Run(language+"/"+statementLanguage, func(t *testing.T) {
				p := generationProblem(t, language)
				p.Problem.Language = statementLanguage
				root := emptyRoot(t)
				manifest, err := Build(context.Background(), root, p)
				if err != nil {
					t.Fatal(err)
				}
				verified, err := StructuralGate(context.Background(), root, testReadLimits())
				if err != nil || verified.Manifest.PackageID != manifest.PackageID || manifest.SchemaVersion != GenerationPackageSchemaVersion {
					t.Fatalf("generation package read: %v", err)
				}
				other := emptyRoot(t)
				again, err := Build(context.Background(), other, p)
				if err != nil || again.PackageID != manifest.PackageID {
					t.Fatalf("generation package changed identity: %v", err)
				}
				encoded, err := EncodeManifest(manifest)
				if err != nil {
					t.Fatal(err)
				}
				for _, bad := range [][]byte{append([]byte(`{"schema_version":"cpgen.package/v2",`), encoded[1:]...), []byte(strings.Replace(string(encoded), `"run_id"`, `"Run_id"`, 1)), []byte(strings.Replace(string(encoded), `"title": "A+B"`, `"title": "\ud800"`, 1))} {
					if _, err := DecodeManifest(bad); err == nil {
						t.Fatal("package accepted duplicate, aliased or malformed Unicode JSON")
					}
				}
			})
		}
	}
}

func TestGenerationPackageRejectsSubstitutedBindings(t *testing.T) {
	for _, change := range []func(*Problem){
		func(p *Problem) { p.Problem.SolutionLanguage = "go" },
		func(p *Problem) { p.Problem.Language = "en\n" },
		func(p *Problem) { p.Checker.Comparison = "other" },
		func(p *Problem) { p.ToolchainManifestDigest = domain.SumBytes([]byte("other lock")) },
		func(p *Problem) {
			for i := range p.Files {
				if p.Files[i].Role == RoleChecker {
					p.Files[i].Data = []byte("int main(){return 0;}")
				}
			}
		},
		func(p *Problem) {
			for i := range p.Files {
				if p.Files[i].Path == "tests/generated-001.ans" {
					p.Files[i].Data = []byte("wrong\n")
				}
			}
		},
		func(p *Problem) {
			var replacement []byte
			for i := range p.Files {
				if p.Files[i].Role == RoleTestPlan {
					var index GenerationTestsDocument
					if err := json.Unmarshal(p.Files[i].Data, &index); err != nil {
						t.Fatal(err)
					}
					index.Plan.Cases[0].Seed++
					replacement = packageJSON(t, index)
					p.Files[i].Data = replacement
				}
			}
			for i := range p.Files {
				if p.Files[i].Role == RolePrePackageReport {
					var quality GenerationQualityDocument
					if err := json.Unmarshal(p.Files[i].Data, &quality); err != nil {
						t.Fatal(err)
					}
					quality.TestsDigest = domain.SumBytes(replacement)
					p.Files[i].Data = packageJSON(t, quality)
				}
			}
		},
	} {
		p := generationProblem(t, "cpp")
		change(&p)
		if p.Validate(240) == nil {
			t.Fatal("generation package accepted a substituted language, checker, test or proof")
		}
	}
}
