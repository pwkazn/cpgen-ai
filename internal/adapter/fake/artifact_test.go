package fake_test

import (
	"context"
	"errors"
	"io"
	"testing"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestArtifactSinkFinalizesDeduplicatesAndVerifies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sink := fake.NewArtifactSink()
	declaration := port.ArtifactDeclaration{
		MediaType: "text/plain", Role: domain.ArtifactEvidence, LogicalPath: "evidence/result.txt", MaxBytes: 32,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "test"},
	}

	first := writeArtifact(t, ctx, sink, declaration, []byte("hello"))
	if first.PhysicalNewBytes != 5 {
		t.Fatalf("first physical bytes = %d, want 5", first.PhysicalNewBytes)
	}
	second := writeArtifact(t, ctx, sink, declaration, []byte("hello"))
	if second.PhysicalNewBytes != 0 || second.Blob != first.Blob {
		t.Fatalf("deduplicated artifact = %+v, first = %+v", second, first)
	}

	reader, err := sink.OpenVerified(ctx, first.Blob)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("read %q, want hello", data)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	pinned, err := sink.PinExisting(ctx, first.Blob, declaration)
	if err != nil {
		t.Fatal(err)
	}
	if pinned.PhysicalNewBytes != 0 || pinned.Blob != first.Blob {
		t.Fatalf("pin returned %+v", pinned)
	}
}

func TestArtifactSinkEnforcesDeclarationLimit(t *testing.T) {
	t.Parallel()
	sink := fake.NewArtifactSink()
	declaration := port.ArtifactDeclaration{
		MediaType: "text/plain", Role: domain.ArtifactEvidence, LogicalPath: "evidence/result.txt", MaxBytes: 4,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "test"},
	}
	writer, err := sink.Prepare(context.Background(), declaration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("hello")); !errors.Is(err, fake.ErrArtifactTooLarge) {
		t.Fatalf("write error = %v, want ErrArtifactTooLarge", err)
	}
}

func writeArtifact(t *testing.T, ctx context.Context, sink *fake.ArtifactSink, declaration port.ArtifactDeclaration, data []byte) domain.PendingArtifact {
	t.Helper()
	writer, err := sink.Prepare(ctx, declaration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	artifact, err := writer.Finalize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}
