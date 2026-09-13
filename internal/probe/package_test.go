//go:build cpgen_slice0_probe

package probe

import (
	"testing"

	"cpgen/internal/domain"
)

func TestStructuralPackageReportArtifactLookupIsExact(t *testing.T) {
	ref := domain.BlobRef{Digest: domain.SumBytes([]byte("manifest")), Size: 8}
	artifact := domain.PendingArtifact{
		Blob: ref, MediaType: "application/json", Role: domain.ArtifactEvidence,
		LogicalPath: "manifest.json", CallID: "call_00000000000000000000000000000001",
		ReservationID: "res_00000000000000000000000000000001",
		WriterTokenID: "writer_00000000000000000000000000000001",
		PinID:         "pin_00000000000000000000000000000001", PhysicalNewBytes: 8,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "test"},
	}
	report := StructuralPackageReport{Artifacts: []domain.PendingArtifact{artifact}}
	got, ok := report.Artifact("manifest.json")
	if !ok || got.LogicalPath != artifact.LogicalPath || got.Blob != artifact.Blob {
		t.Fatalf("artifact lookup = %#v, %v", got, ok)
	}
	if _, ok := report.Artifact("MANIFEST.JSON"); ok {
		t.Fatal("artifact lookup unexpectedly normalized a path")
	}
}

func TestStructuralPackageReportRejectsDuplicatePaths(t *testing.T) {
	ref := domain.BlobRef{Digest: domain.SumBytes([]byte("x")), Size: 1}
	artifact := domain.PendingArtifact{
		Blob: ref, MediaType: "text/plain", Role: domain.ArtifactEvidence,
		LogicalPath: "manifest.json", CallID: "call_00000000000000000000000000000001",
		ReservationID: "res_00000000000000000000000000000001",
		WriterTokenID: "writer_00000000000000000000000000000001",
		PinID:         "pin_00000000000000000000000000000001", PhysicalNewBytes: 1,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "test"},
	}
	report := StructuralPackageReport{Kind: StructuralPackageReportKind, Status: "PASSED", PackageID: ref.Digest, ManifestDigest: ref.Digest, FileCount: 1, ArtifactCallID: artifact.CallID, Artifacts: []domain.PendingArtifact{artifact, artifact}}
	if err := report.Validate(); err == nil {
		t.Fatal("duplicate package paths were accepted")
	}
}
