package domain_test

import (
	"encoding/json"
	"strings"
	"testing"

	"cpgen/internal/domain"
)

func TestDigestRoundTrip(t *testing.T) {
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
}

func TestDigestRejectsUppercase(t *testing.T) {
	t.Parallel()
	value := "sha256:" + strings.Repeat("A", 64)
	if _, err := domain.ParseDigest(value); err == nil {
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
}

// TestWorkflowIDsRejectMalformedJSON catches a future lifecycle record gaining
// an identifier that can bypass the common strict lowercase-prefixed ID rule.
func TestWorkflowIDsRejectMalformedJSON(t *testing.T) {
	t.Parallel()
	for _, target := range []any{
		new(domain.ReviewDecisionID), new(domain.ControlRequestID), new(domain.CallRecordID),
		new(domain.ArtifactDeclarationID), new(domain.ArtifactOccurrenceID), new(domain.CacheReuseRecordID),
	} {
		if err := json.Unmarshal([]byte(`"not-an-id"`), target); err == nil {
			t.Fatalf("%T accepted malformed JSON ID", target)
		}
	}
}
