//go:build cpgen_slice0_probe

package probe

import (
	"context"
	"io"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestCapabilityMemoryArtifactStoreEnforcesLimitsAndVerifiedReads(t *testing.T) {
	store := NewMemoryArtifactStore()
	callOrdinal := 0
	plan, err := port.NewContainerPlan(domain.SumBytes([]byte("engine")), []port.PlannedResource{{
		Ordinal: 0, Kind: port.ResourceContainer, Role: port.ResourceTarget, DeterministicName: "target",
		ExpectedLabelsDigest: domain.SumBytes([]byte("labels")), CreateCallOrdinal: &callOrdinal,
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	callID := domain.AttemptCallID("call_00000000000000000000000000000001")
	if err := store.configureCalls(plan, []domain.AttemptCallID{callID}); err != nil {
		t.Fatal(err)
	}
	declaration := port.ArtifactDeclaration{
		MediaType: "text/plain", Role: domain.ArtifactStdout, LogicalPath: "process/stdout", MaxBytes: 3,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "docker-direct-v2"},
	}
	overflow, err := store.Prepare(context.Background(), declaration)
	if err != nil {
		t.Fatal(err)
	}
	if written, err := overflow.Write([]byte("four")); err == nil || written != 0 {
		t.Fatalf("overflow write = %d, %v", written, err)
	}
	if err := overflow.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}

	writer, err := store.Prepare(context.Background(), declaration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	artifact, err := writer.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if artifact.CallID != callID || artifact.Blob.Size != 3 {
		t.Fatalf("artifact = %#v", artifact)
	}
	reader, err := store.OpenVerified(context.Background(), artifact.Blob)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil || string(data) != "abc" || reader.BlobRef() != artifact.Blob {
		t.Fatalf("verified read = %q, %v, %v", data, readErr, closeErr)
	}
}
