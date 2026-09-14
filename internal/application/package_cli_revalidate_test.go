package application_test

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/packageprobe"
	"cpgen/internal/port"
)

// Fresh compilation/execution consumes only the exported archive. No original
// run, compiled program, expected-output provider or source blob is reused.
func revalidateExportedPackageInDocker(t *testing.T, ctx context.Context, base sandboxexec.Config, raw []byte) {
	t.Helper()
	p, err := packageprobe.ReadArchive(ctx, raw)
	if err != nil {
		t.Fatal(err)
	}
	if p.Manifest.Problem.SolutionLanguage != "cpp" {
		t.Fatal("this execution fixture requires C++")
	}
	content := make(map[domain.SafeRelPath][]byte)
	for _, file := range p.Files {
		content[file.Entry.Path] = file.Bytes
	}
	var index packageprobe.GenerationTestsDocument
	if err := domain.DecodeStrictJSON(content["data/tests.json"], &index); err != nil {
		t.Fatal(err)
	}
	// A live model may produce more than the six cases in the fixed fixture.
	// Bound independent verification by the validated index, allowing both
	// programs on every case plus compilation and transfer helpers.
	f := newCoordinatorFixtureAt(t, "de", domain.BudgetLimits{MaxArtifactBytes: 64 << 20, MaxSandboxCreates: int64(16 + 4*len(index.Tests)), MaxActiveTimeMilliseconds: 300000}, []domain.StageName{"prepare", "exercise"}, p.Manifest.PackageID, time.Now().UTC())
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "imported-blobs"))
	if err != nil {
		t.Fatal(err)
	}
	identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000de", LogicalOperationID: "export-revalidation", Kind: domain.CallSandboxCompile, ScopeDigest: p.Manifest.PackageID, ExpectedRunVersion: 2}
	publisher, err := sandboxexec.NewArtifactSink(f.store, blobs, clock.Real{}, identity)
	if err != nil {
		t.Fatal(err)
	}
	var pending []domain.PendingArtifact
	publish := func(path domain.SafeRelPath, data []byte) domain.BlobRef {
		artifact, err := publisher.Publish(ctx, port.ArtifactDeclaration{LogicalPath: path, Role: domain.ArtifactInput, MediaType: "application/octet-stream", MaxBytes: int64(max(1, len(data))), Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.export-revalidation/v1", Producer: "fixture", InputDigest: &p.Manifest.PackageID}}, data)
		if err != nil {
			t.Fatal(err)
		}
		pending = append(pending, artifact)
		return artifact.Blob
	}
	base.Store, base.Blobs, base.Clock, base.Identity = f.store, blobs, clock.Real{}, identity
	worker, err := sandboxexec.NewSession(base)
	if err != nil {
		t.Fatal(err)
	}
	programs := make(map[port.ProgramRole]domain.BlobRef)
	for role, path := range map[port.ProgramRole]domain.SafeRelPath{port.RoleSolution: "solution/reference.cpp", port.RoleBrute: "solution/brute.cpp"} {
		source := publish(path, content[path])
		bundle := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: "main.cpp", Files: []port.SourceFile{{Path: "main.cpp", Blob: source}}}
		bundle.Digest, err = port.ComputeSourceBundleDigest(bundle)
		if err != nil {
			t.Fatal(err)
		}
		compiled, err := worker.Compile(ctx, port.CompileRequest{Language: port.LanguageCPP20, Role: role, SourceBundle: bundle, Toolchain: "cpp20-gcc-bookworm-v1", Limits: port.CompileLimits{Time: 45 * time.Second, MemoryBytes: 512 << 20, PIDs: 64, OutputBytes: 8 << 20}, ExpectedOutput: "result/files/main"})
		if err != nil || compiled.Value == nil || compiled.Value.Outcome != domain.CompileOK || compiled.Value.Program == nil {
			t.Fatalf("exported %s compile: %+v %v", role, compiled, err)
		}
		programs[role] = compiled.Value.Program.Blob
	}
	for _, item := range index.Tests {
		stdin := publish(domain.SafeRelPath("tests/"+item.ID+".in"), content[domain.SafeRelPath("tests/"+item.ID+".in")])
		answer := content[domain.SafeRelPath("tests/"+item.ID+".ans")]
		roles := []port.ProgramRole{port.RoleSolution}
		if item.Kind == domain.DataCaseSmall {
			roles = append(roles, port.RoleBrute)
		}
		for _, role := range roles {
			result, err := worker.Run(ctx, port.RunRequest{Role: role, Program: programs[role], Stdin: &stdin, Limits: port.RunLimits{Time: time.Duration(p.Manifest.Limits.TimeMS) * time.Millisecond, MemoryBytes: p.Manifest.Limits.MemoryMB << 20, PIDs: 64, StdoutBytes: 1 << 20, StderrBytes: 1 << 20}})
			if err != nil || result.Value == nil {
				t.Fatalf("exported %s/%s execution: %+v %v", item.ID, role, result, err)
			}
			verdict, err := judge.AdaptSolution(*result.Value)
			if err != nil || verdict.Outcome == nil || *verdict.Outcome != domain.SolutionOK || result.Value.Stdout == nil {
				t.Fatalf("exported %s/%s verdict: %+v %v", item.ID, role, verdict, err)
			}
			r, err := blobs.OpenVerified(ctx, result.Value.Stdout.Blob)
			if err != nil {
				t.Fatal(err)
			}
			actual, readErr := io.ReadAll(io.LimitReader(r, (1<<20)+1))
			closeErr := r.Close()
			if readErr != nil || closeErr != nil || judge.ExactTokenDigest(actual) != judge.ExactTokenDigest(answer) {
				t.Fatalf("exported %s/%s answer differs: %v %v", item.ID, role, readErr, closeErr)
			}
		}
	}
	var occurrences []domain.PendingOccurrence
	seen := make(map[domain.ArtifactWriterTokenID]bool)
	for _, artifact := range append(pending, worker.Artifacts()...) {
		if !seen[artifact.WriterTokenID] {
			item := artifact
			occurrences = append(occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &item})
			seen[artifact.WriterTokenID] = true
		}
	}
	run, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	output := p.Manifest.PackageID
	if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: run.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, Occurrences: occurrences, IdempotencyKey: coordinatorID("finish", "export-revalidation"), At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
}
