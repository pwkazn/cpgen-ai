package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/packageprobe"
	"cpgen/internal/port"
)

// Explicit, problem-specific validator cases are independent of the model's
// generated positive corpus. This test only consumes an already exported ZIP.
func TestLiveExportValidatorCases(t *testing.T) {
	casePath := os.Getenv("CPGEN_LIVE_VALIDATOR_CASES")
	if casePath == "" {
		t.Skip("explicit private validator cases are required")
	}
	root := os.Getenv("CPGEN_LIVE_ROOT")
	if !filepath.IsAbs(root) || !filepath.IsAbs(casePath) {
		t.Fatal("validator acceptance requires absolute private paths")
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(casePath)
	must(err)
	var cases []struct {
		Name     string `json:"name"`
		Input    string `json:"input"`
		ExitCode int    `json:"exit_code"`
	}
	must(domain.DecodeStrictJSON(raw, &cases))
	if len(cases) == 0 || len(cases) > 32 {
		t.Fatal("require 1-32 validator cases")
	}
	for _, c := range cases {
		if c.Name == "" || len(c.Input) > 1<<20 || (c.ExitCode != 0 && c.ExitCode != 3) {
			t.Fatal("invalid validator case")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	base := newDockerSandboxTestConfig(t, ctx)
	raw, err = os.ReadFile(filepath.Join(root, "problem.zip"))
	must(err)
	p, err := packageprobe.ReadArchive(ctx, raw)
	must(err)
	if p.Manifest.Problem.SolutionLanguage != "cpp" {
		t.Fatal("validator probe requires C++")
	}
	var source []byte
	for _, f := range p.Files {
		if f.Entry.Path == "judge/validator.cpp" {
			source = f.Bytes
		}
	}
	if len(source) == 0 {
		t.Fatal("export omitted validator source")
	}
	f := newCoordinatorFixtureAt(t, "ef", domain.BudgetLimits{MaxArtifactBytes: 64 << 20, MaxSandboxCreates: int64(8 + 2*len(cases)), MaxActiveTimeMilliseconds: 300000}, []domain.StageName{"prepare", "exercise"}, p.Manifest.PackageID, time.Now().UTC())
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "validator-blobs"))
	must(err)
	identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000ef", LogicalOperationID: "export-validator", Kind: domain.CallSandboxCompile, ScopeDigest: p.Manifest.PackageID, ExpectedRunVersion: 2}
	publisher, err := sandboxexec.NewArtifactSink(f.store, blobs, clock.Real{}, identity)
	must(err)
	var pending []domain.PendingArtifact
	publish := func(path domain.SafeRelPath, data []byte) domain.BlobRef {
		a, err := publisher.Publish(ctx, port.ArtifactDeclaration{LogicalPath: path, Role: domain.ArtifactInput, MediaType: "application/octet-stream", MaxBytes: int64(max(1, len(data))), Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.export-validator/v1", Producer: "fixture", InputDigest: &p.Manifest.PackageID}}, data)
		must(err)
		pending = append(pending, a)
		return a.Blob
	}
	base.Store, base.Blobs, base.Clock, base.Identity = f.store, blobs, clock.Real{}, identity
	worker, err := sandboxexec.NewSession(base)
	must(err)
	bundle := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: "main.cpp", Files: []port.SourceFile{{Path: "main.cpp", Blob: publish("validator.cpp", source)}}}
	bundle.Digest, err = port.ComputeSourceBundleDigest(bundle)
	must(err)
	compiled, err := worker.Compile(ctx, port.CompileRequest{Language: port.LanguageCPP20, Role: port.RoleValidator, SourceBundle: bundle, Toolchain: "cpp20-gcc-bookworm-v1", Limits: port.CompileLimits{Time: 45 * time.Second, MemoryBytes: 512 << 20, PIDs: 64, OutputBytes: 8 << 20}, ExpectedOutput: "result/files/main"})
	must(err)
	if compiled.Value == nil || compiled.Value.Outcome != domain.CompileOK || compiled.Value.Program == nil {
		t.Fatal("exported validator did not compile")
	}
	var results []map[string]any
	for i, c := range cases {
		input := publish(domain.SafeRelPath(fmt.Sprintf("cases/%03d.in", i+1)), []byte(c.Input))
		r, err := worker.Run(ctx, port.RunRequest{Role: port.RoleValidator, Program: compiled.Value.Program.Blob, Stdin: &input, Limits: port.RunLimits{Time: 2 * time.Second, MemoryBytes: 256 << 20, PIDs: 64, StdoutBytes: 4096, StderrBytes: 8192}})
		must(err)
		if r.Value == nil {
			t.Fatal("validator execution has no result")
		}
		passed := r.Value.Outcome == domain.ProcessExited && r.Value.ExitCode != nil && *r.Value.ExitCode == c.ExitCode
		results = append(results, map[string]any{"name": c.Name, "expected_exit_code": c.ExitCode, "result": r.Value, "passed": passed})
		if !passed {
			var exitCode any
			if r.Value.ExitCode != nil {
				exitCode = *r.Value.ExitCode
			}
			t.Errorf("validator case %s: outcome=%s exit=%v want=%d", c.Name, r.Value.Outcome, exitCode, c.ExitCode)
		}
	}
	var occurrences []domain.PendingOccurrence
	seen := map[domain.ArtifactWriterTokenID]bool{}
	for _, a := range append(pending, worker.Artifacts()...) {
		if !seen[a.WriterTokenID] {
			item := a
			occurrences = append(occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &item})
			seen[a.WriterTokenID] = true
		}
	}
	run, err := f.store.GetRun(ctx, f.runID)
	must(err)
	digest := p.Manifest.PackageID
	_, err = f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: run.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &digest, NextStage: "exercise", NextInputDigest: &digest, Occurrences: occurrences, IdempotencyKey: coordinatorID("finish", "export-validator"), At: time.Now().UTC()})
	must(err)
	raw, err = json.MarshalIndent(map[string]any{"package_id": digest, "passed": !t.Failed(), "cases": results}, "", "  ")
	must(err)
	must(os.WriteFile(filepath.Join(root, "validator-acceptance.json"), raw, 0600))
}
