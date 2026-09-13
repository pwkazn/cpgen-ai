package packageprobe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestStructuralGateBuildInspectAndImportRoundTrip(t *testing.T) {
	directory, root := buildPackage(t, minimalProblem(t))
	limits := testReadLimits()
	inspected, err := Inspect(context.Background(), root, limits)
	if err != nil {
		t.Fatal(err)
	}
	if err := inspected.Validate(); err != nil {
		t.Fatal(err)
	}
	root.Close()

	store := newPackageStore()
	store.wantPreparedBeforeWrite = len(inspected.Files) + 1
	root, err = os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	imported, artifacts, err := Import(context.Background(), root, limits, store)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Manifest.PackageID != inspected.Manifest.PackageID || len(artifacts) != len(inspected.Files)+1 {
		t.Fatalf("imported package/artifacts = %#v / %d", imported.Manifest, len(artifacts))
	}
	if store.openCount != len(artifacts) {
		t.Fatalf("reverse reader opened %d imported Blobs, want %d", store.openCount, len(artifacts))
	}
	for _, artifact := range artifacts {
		if err := artifact.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStructuralGateRejectsManifestAndTreeAttacks(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, Manifest)
	}{
		{name: "missing file", mutate: func(t *testing.T, dir string, manifest Manifest) {
			mustRemove(t, filepath.Join(dir, filepath.FromSlash(string(manifest.Files[0].Path))))
		}},
		{name: "extra file", mutate: func(t *testing.T, dir string, _ Manifest) {
			mustWrite(t, filepath.Join(dir, "extra.txt"), []byte("extra"))
		}},
		{name: "same-size hash tamper", mutate: func(t *testing.T, dir string, manifest Manifest) {
			path := filepath.Join(dir, filepath.FromSlash(string(manifest.Files[0].Path)))
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			tampered := bytes.Repeat([]byte{'x'}, len(original))
			mustWrite(t, path, tampered)
		}},
		{name: "declared size tamper", mutate: func(t *testing.T, dir string, manifest Manifest) {
			manifest.Files[0].Size++
			manifest.PackageID = ""
			manifest.PackageID, _ = ComputePackageID(manifest)
			writeManifest(t, dir, manifest)
		}},
		{name: "wrong package id", mutate: func(t *testing.T, dir string, manifest Manifest) {
			manifest.PackageID = domain.SumBytes([]byte("wrong"))
			writeManifest(t, dir, manifest)
		}},
		{name: "manifest self entry", mutate: func(t *testing.T, dir string, manifest Manifest) {
			manifest.Files = append(manifest.Files, FileEntry{Path: "manifest.json", SHA256: domain.SumBytes(nil), Size: 0, Role: RoleManifest})
			writeManifest(t, dir, manifest)
		}},
		{name: "unknown manifest field", mutate: func(t *testing.T, dir string, _ Manifest) {
			path := filepath.Join(dir, "manifest.json")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte(`"schema_version"`), []byte(`"unknown":true,"schema_version"`), 1)
			mustWrite(t, path, data)
		}},
		{name: "unknown report field", mutate: func(t *testing.T, dir string, manifest Manifest) {
			path := domain.SafeRelPath("reports/similarity.json")
			full := filepath.Join(dir, filepath.FromSlash(string(path)))
			data, err := os.ReadFile(full)
			if err != nil {
				t.Fatal(err)
			}
			data = bytes.Replace(data, []byte(`"matches"`), []byte(`"raw_response":"forbidden","matches"`), 1)
			mustWrite(t, full, data)
			for index := range manifest.Files {
				if manifest.Files[index].Path == path {
					manifest.Files[index].Size = int64(len(data))
					manifest.Files[index].SHA256 = domain.SumBytes(data)
				}
			}
			manifest.PackageID = ""
			manifest.PackageID, _ = ComputePackageID(manifest)
			writeManifest(t, dir, manifest)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory, root := buildPackage(t, minimalProblem(t))
			manifest := readManifestFile(t, directory)
			_ = root.Close()
			test.mutate(t, directory, manifest)
			root, err := os.OpenRoot(directory)
			if err != nil {
				t.Fatal(err)
			}
			defer root.Close()
			if _, err := Inspect(context.Background(), root, testReadLimits()); err == nil {
				t.Fatal("attacked package was accepted")
			}
		})
	}
}

func TestStructuralGateRejectsLinksDuplicatesMissingAnswersAndLimits(t *testing.T) {
	problem := minimalProblem(t)
	problem.TestGroups[0].Tests = append(problem.TestGroups[0].Tests, "001")
	if _, err := Build(context.Background(), emptyRoot(t), problem); err == nil {
		t.Fatal("duplicate test ID was accepted")
	}
	problem = minimalProblem(t)
	problem.Files = removeProblemFile(problem.Files, "tests/001.ans")
	if _, err := Build(context.Background(), emptyRoot(t), problem); err == nil {
		t.Fatal("input without answer was accepted")
	}

	directory, root := buildPackage(t, minimalProblem(t))
	_ = root.Close()
	statement := filepath.Join(directory, "statement", "zh-CN.md")
	backup := statement + ".real"
	if err := os.Rename(statement, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(backup, statement); err == nil {
		root, err = os.OpenRoot(directory)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(context.Background(), root, testReadLimits()); err == nil {
			t.Fatal("symlink package file was accepted")
		}
		_ = root.Close()
	} else {
		t.Logf("symlink unavailable: %v", err)
	}

	directory, root = buildPackage(t, minimalProblem(t))
	_ = root.Close()
	externalLink := filepath.Join(t.TempDir(), "statement-hardlink.md")
	if err := os.Link(filepath.Join(directory, "statement", "zh-CN.md"), externalLink); err == nil {
		root, err = os.OpenRoot(directory)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Inspect(context.Background(), root, testReadLimits()); err == nil {
			t.Fatal("multiply-linked package file was accepted")
		}
		_ = root.Close()
	} else {
		t.Logf("hardlink unavailable: %v", err)
	}

	_, root = buildPackage(t, minimalProblem(t))
	limits := testReadLimits()
	limits.MaxFiles = 2
	if _, err := Inspect(context.Background(), root, limits); err == nil {
		t.Fatal("file-count limit was ignored")
	}
	_ = root.Close()
	_, root = buildPackage(t, minimalProblem(t))
	limits = testReadLimits()
	limits.MaxTotalBytes = 8
	if _, err := Inspect(context.Background(), root, limits); err == nil {
		t.Fatal("total-byte limit was ignored")
	}
	_ = root.Close()
}

func TestStructuralGateReadsTheVerifiedOpenHandleAfterPathReplacement(t *testing.T) {
	directory, root := buildPackage(t, minimalProblem(t))
	opened, err := openPackage(context.Background(), root, testReadLimits())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "statement", "zh-CN.md")
	moved := path + ".moved"
	if err := os.Rename(path, moved); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, []byte("attacker replacement"))
	verified, err := inspectOpened(context.Background(), opened)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.close(); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	for _, file := range verified.Files {
		if file.Entry.Path == "statement/zh-CN.md" && string(file.Bytes) != "# A+B\n" {
			t.Fatalf("verified handle returned replacement bytes %q", file.Bytes)
		}
	}
}

func minimalProblem(t *testing.T) Problem {
	t.Helper()
	files := []ProblemFile{
		{Path: "statement/zh-CN.md", Role: RoleStatement, Data: []byte("# A+B\n")},
		{Path: "statement/samples.json", Role: RoleSamples, Data: []byte(`{"schema_version":"cpgen.samples/v1","samples":[{"input":"1 2\\n","output":"3\\n"}]}`)},
		{Path: "solution/editorial.md", Role: RoleEditorial, Data: []byte("Add the two integers.\n")},
		{Path: "solution/reference.cpp", Role: RoleReference, Data: []byte("reference")},
		{Path: "solution/brute.cpp", Role: RoleBrute, Data: []byte("brute")},
		{Path: "judge/validator.cpp", Role: RoleValidator, Data: []byte("validator")},
		{Path: "judge/checker.cpp", Role: RoleChecker, Data: []byte("checker")},
		{Path: "judge/generator.cpp", Role: RoleGenerator, Data: []byte("generator")},
		{Path: "tests/001.in", Role: RoleTestInput, Data: []byte("1 2\n")},
		{Path: "tests/001.ans", Role: RoleTestAnswer, Data: []byte("3\n")},
		{Path: "reports/similarity.json", Role: RoleSimilarityReport, Data: []byte(`{"schema_version":"cpgen.similarity-report/v1","evidence_digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","decision_digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","matches":[]}`)},
		{Path: "reports/prepackage-quality.json", Role: RolePrePackageReport, Data: []byte(`{"schema_version":"cpgen.prepackage-report/v1","run_id":"run_00000000000000000000000000000001","profile":"mvp","problem_spec_revision":1,"status":"PASSED"}`)},
		{Path: "reports/provenance.json", Role: RoleProvenance, Data: []byte(`{"schema_version":"cpgen.provenance/v1","run_id":"run_00000000000000000000000000000001","source":"slice0-probe","vertical_digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"}`)},
	}
	return Problem{
		RunID:                   "run_00000000000000000000000000000001",
		Problem:                 ProblemInfo{Slug: "a-plus-b", Title: "A+B", Language: "zh-CN", ProblemSpecRevision: 1},
		Limits:                  ProblemLimits{TimeMS: 2000, MemoryMB: 256, OutputBytes: 1 << 20},
		Checker:                 CheckerConfig{Kind: "token", Protocol: "testlib_v1", ArtifactPath: "judge/checker.cpp"},
		ToolchainManifestDigest: domain.SumBytes([]byte("toolchain")),
		TestGroups:              []TestGroup{{Name: "main", Tests: []string{"001"}, CoverageTags: []string{"minimum"}}},
		Verification:            Verification{Profile: "mvp", PrePackageReportPath: "reports/prepackage-quality.json", EnvironmentDigest: domain.SumBytes([]byte("environment"))},
		ProvenancePath:          "reports/provenance.json", Files: files,
	}
}

func buildPackage(t *testing.T, problem Problem) (string, *os.Root) {
	t.Helper()
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), root, problem); err != nil {
		root.Close()
		t.Fatal(err)
	}
	return directory, root
}

func emptyRoot(t *testing.T) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func testReadLimits() ReadLimits {
	return ReadLimits{MaxFiles: 64, MaxPathBytes: 240, MaxFileBytes: 1 << 20, MaxTotalBytes: 8 << 20, MaxManifestBytes: 1 << 20}
}

func readManifestFile(t *testing.T, directory string) Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(directory, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := DecodeManifest(data)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func writeManifest(t *testing.T, directory string, manifest Manifest) {
	t.Helper()
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	mustWrite(t, filepath.Join(directory, "manifest.json"), data)
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustRemove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func removeProblemFile(files []ProblemFile, path domain.SafeRelPath) []ProblemFile {
	result := make([]ProblemFile, 0, len(files)-1)
	for _, file := range files {
		if file.Path != path {
			result = append(result, file)
		}
	}
	return result
}

type packageStore struct {
	mu                      sync.Mutex
	blobs                   map[domain.Digest][]byte
	prepareCount            int
	wantPreparedBeforeWrite int
	openCount               int
}

func newPackageStore() *packageStore { return &packageStore{blobs: map[domain.Digest][]byte{}} }

func (s *packageStore) Prepare(_ context.Context, declaration port.ArtifactDeclaration) (port.ArtifactWriter, error) {
	if err := declaration.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.prepareCount++
	s.mu.Unlock()
	return &packageWriter{store: s, declaration: declaration}, nil
}

func (s *packageStore) PinExisting(context.Context, domain.BlobRef, port.ArtifactDeclaration) (domain.PendingArtifact, error) {
	return domain.PendingArtifact{}, fmt.Errorf("not implemented")
}

func (s *packageStore) OpenVerified(_ context.Context, ref domain.BlobRef) (port.VerifiedReadCloser, error) {
	s.mu.Lock()
	data, ok := s.blobs[ref.Digest]
	s.openCount++
	s.mu.Unlock()
	if !ok || int64(len(data)) != ref.Size {
		return nil, fmt.Errorf("blob not found")
	}
	return &packageReader{Reader: bytes.NewReader(bytes.Clone(data)), ref: ref}, nil
}

type packageWriter struct {
	store       *packageStore
	declaration port.ArtifactDeclaration
	buffer      bytes.Buffer
	terminal    bool
}

func (w *packageWriter) Write(data []byte) (int, error) {
	w.store.mu.Lock()
	prepared, want := w.store.prepareCount, w.store.wantPreparedBeforeWrite
	w.store.mu.Unlock()
	if want != 0 && prepared != want {
		return 0, fmt.Errorf("streaming began after %d of %d declarations", prepared, want)
	}
	if w.terminal || int64(w.buffer.Len()+len(data)) > w.declaration.MaxBytes {
		return 0, fmt.Errorf("writer limit or terminal state")
	}
	return w.buffer.Write(data)
}

func (w *packageWriter) Finalize(context.Context) (domain.PendingArtifact, error) {
	if w.terminal {
		return domain.PendingArtifact{}, fmt.Errorf("writer is terminal")
	}
	w.terminal = true
	data := bytes.Clone(w.buffer.Bytes())
	ref := domain.BlobRef{Digest: domain.SumBytes(data), Size: int64(len(data))}
	w.store.mu.Lock()
	_, existed := w.store.blobs[ref.Digest]
	w.store.blobs[ref.Digest] = data
	w.store.mu.Unlock()
	physical := ref.Size
	if existed {
		physical = 0
	}
	return domain.PendingArtifact{
		Blob: ref, MediaType: w.declaration.MediaType, Role: w.declaration.Role, LogicalPath: w.declaration.LogicalPath,
		CallID: "call_00000000000000000000000000000001", ReservationID: "res_00000000000000000000000000000001",
		WriterTokenID: "writer_00000000000000000000000000000001", PinID: "pin_00000000000000000000000000000001",
		PhysicalNewBytes: physical, Provenance: w.declaration.Provenance,
	}, nil
}

func (w *packageWriter) Abort(context.Context) error { w.terminal = true; return nil }

type packageReader struct {
	*bytes.Reader
	ref domain.BlobRef
}

func (r *packageReader) Close() error            { return nil }
func (r *packageReader) BlobRef() domain.BlobRef { return r.ref }

var _ ImportSink = (*packageStore)(nil)
var _ io.ReadCloser = (*packageReader)(nil)
