package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"cpgen/internal/domain"
)

func TestDigestValidation(t *testing.T) {
	t.Parallel()
	digest := domain.SumBytes([]byte("cpgen"))
	if err := digest.Validate(); err != nil {
		t.Fatalf("generated digest is invalid: %v", err)
	}
	encoded, err := json.Marshal(digest)
	if err != nil {
		t.Fatal(err)
	}
	var decoded domain.Digest
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != digest {
		t.Fatalf("round trip changed digest: %q != %q", decoded, digest)
	}
	if _, err := domain.ParseDigest("sha256:" + strings.Repeat("A", 64)); err == nil {
		t.Fatal("uppercase digest was accepted")
	}
}

func TestSafeRelPathRejectsTraversalAndPlatformPaths(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"../secret", "/absolute", `C:\secret`, `a\b`, "a//b", "."} {
		if _, err := domain.ParseSafeRelPath(value); err == nil {
			t.Errorf("unsafe path %q was accepted", value)
		}
	}
	for _, value := range []string{"main.cpp", "tests/001.in", "报告/输入.txt"} {
		if _, err := domain.ParseSafeRelPath(value); err != nil {
			t.Errorf("safe path %q was rejected: %v", value, err)
		}
	}
}

func TestSchemaVersionAndIDValidation(t *testing.T) {
	t.Parallel()
	if _, err := domain.ParseSchemaVersion("cpgen.package/v1"); err != nil {
		t.Fatalf("valid schema version rejected: %v", err)
	}
	if _, err := domain.ParseSchemaVersion("v1"); err == nil {
		t.Fatal("unscoped schema version accepted")
	}
	value, err := domain.NewID("run")
	if err != nil {
		t.Fatal(err)
	}
	if err := domain.RunID(value).Validate(); err != nil {
		t.Fatalf("generated id is invalid: %v", err)
	}
	for _, target := range []any{
		new(domain.ReviewDecisionID), new(domain.ControlRequestID), new(domain.CallRecordID),
		new(domain.ArtifactDeclarationID), new(domain.ArtifactOccurrenceID), new(domain.CacheReuseRecordID),
	} {
		if err := json.Unmarshal([]byte(`"not-an-id"`), target); err == nil {
			t.Fatalf("%T accepted malformed JSON ID", target)
		}
	}
}

func TestPendingOccurrenceTaggedUnion(t *testing.T) {
	t.Parallel()
	digest := domain.SumBytes([]byte("artifact"))
	base := domain.PendingArtifact{Blob: domain.BlobRef{Digest: digest, Size: 8}, MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: "out.txt",
		CallID: "call_00000000000000000000000000000001", ReservationID: "res_00000000000000000000000000000001", WriterTokenID: "writer_00000000000000000000000000000001", PinID: "pin_00000000000000000000000000000001", PhysicalNewBytes: 8,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}}
	validNew := domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &base}
	if err := validNew.Validate(); err != nil {
		t.Fatalf("valid NEW_WRITE rejected: %v", err)
	}
	for name, value := range map[string]domain.PendingOccurrence{
		"empty":   {Kind: domain.PendingOccurrenceNewWrite},
		"both":    {Kind: domain.PendingOccurrenceNewWrite, NewWrite: &base, CacheReuse: &domain.PendingCacheReuse{}},
		"unknown": {Kind: "OTHER", NewWrite: &base},
	} {
		if err := value.Validate(); err == nil {
			t.Errorf("%s occurrence accepted", name)
		}
	}
	reuse := domain.PendingCacheReuse{CacheReuseRecordID: "reuse_00000000000000000000000000000001", SourceOccurrenceID: domain.ArtifactOccurrenceID("occurrence_" + strings.Repeat("0", 32)), SourceCallRecordID: "call_00000000000000000000000000000002", CurrentCallRecordID: "call_00000000000000000000000000000003", Blob: base.Blob, MediaType: base.MediaType, Role: base.Role, LogicalPath: base.LogicalPath, Provenance: base.Provenance}
	if err := (domain.PendingOccurrence{Kind: domain.PendingOccurrenceCacheReuse, CacheReuse: &reuse}).Validate(); err != nil {
		t.Fatalf("valid CACHE_REUSE rejected: %v", err)
	}
}
