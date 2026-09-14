package application_test

import (
	"context"
	"path/filepath"
	"testing"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestSandboxArtifactPublicationReplaysAndAttachesWithoutDoubleCharge(t *testing.T) {
	ctx := context.Background()
	f := newCoordinatorFixtureWithStages(t, "f4", domain.BudgetLimits{MaxArtifactBytes: 4096, MaxActiveTimeMilliseconds: 100000}, []domain.StageName{"prepare", "exercise"})
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000f4", LogicalOperationID: "solution-source", Kind: domain.CallSandboxCompile, ScopeDigest: domain.SumBytes([]byte("accepted solution source")), ExpectedRunVersion: 2}
	newSink := func() *sandboxexec.ArtifactSink {
		sink, err := sandboxexec.NewArtifactSink(f.store, blobs, f.clock, identity)
		if err != nil {
			t.Fatal(err)
		}
		return sink
	}
	digest := domain.SumBytes([]byte("solution content"))
	decl := port.ArtifactDeclaration{MediaType: "text/x-c++src", Role: domain.ArtifactSource, LogicalPath: "solution/reference/main.cpp", MaxBytes: 1024, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.solution-source/v1", Producer: "solution", InputDigest: &digest}}
	raw := []byte("int main() { return 0; }\r\n")
	first, err := newSink().Publish(ctx, decl, raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	second, err := newSink().Publish(ctx, decl, raw)
	if err != nil || first.Blob != second.Blob || first.CallID != second.CallID || first.WriterTokenID != second.WriterTokenID {
		t.Fatalf("publication replay changed identity: %+v %+v %v", first, second, err)
	}
	if _, err := newSink().Publish(ctx, decl, []byte("different source")); err == nil {
		t.Fatal("same publication identity accepted changed bytes")
	}
	changed := decl
	changed.MediaType = "application/octet-stream"
	if _, _, err := newSink().Read(ctx, changed); err == nil {
		t.Fatal("read accepted substituted metadata")
	}
	run, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	output := domain.SumBytes([]byte("source stage"))
	if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: run.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &first}}, IdempotencyKey: coordinatorID("finish", "sandbox source"), At: f.clock.Now()}); err != nil {
		t.Fatalf("attach settled artifact: %v", err)
	}
	budget, err := f.store.BudgetSnapshot(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if budget.Remaining[domain.BudgetArtifactPhysicalNewBytes] != 4096-int64(len(raw)) {
		t.Fatalf("artifact publication charged twice: %+v", budget)
	}
}

func TestSandboxArtifactAbortReleasesUnusedBytes(t *testing.T) {
	ctx := context.Background()
	f := newCoordinatorFixture(t, "f5", domain.BudgetLimits{MaxArtifactBytes: 64, MaxActiveTimeMilliseconds: 100000})
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000f5", LogicalOperationID: "compile", Kind: domain.CallSandboxCompile, ScopeDigest: domain.SumBytes([]byte("compile")), ExpectedRunVersion: 2}
	sink, err := sandboxexec.NewArtifactSink(f.store, blobs, f.clock, identity)
	if err != nil {
		t.Fatal(err)
	}
	decl := port.ArtifactDeclaration{MediaType: "text/plain", Role: domain.ArtifactStderr, LogicalPath: "compile/stderr", MaxBytes: 64, Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "docker"}}
	writer, err := sink.Prepare(ctx, decl)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("unpublished diagnostic")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Abort(ctx); err != nil {
		t.Fatal(err)
	}
	if err := writer.Abort(ctx); err != nil {
		t.Fatalf("repeat abort: %v", err)
	}
	decl.LogicalPath = "compile/stdout"
	if _, err := sink.Publish(ctx, decl, []byte("ok")); err != nil {
		t.Fatalf("aborted output held budget: %v", err)
	}
	decl.LogicalPath = "compile/too-large"
	decl.MaxBytes = 65
	if _, err := sink.Prepare(ctx, decl); err == nil {
		t.Fatal("artifact reservation exceeded frozen budget")
	}
}
