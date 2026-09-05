package domain

import (
	"testing"
	"time"
)

func TestCacheLookupRejectsMismatchedSourceDigest(t *testing.T) {
	key := SumBytes([]byte("cache-key"))
	policy := SumBytes([]byte("policy"))
	input := SumBytes([]byte("input"))
	lookup := CacheLookup{RunID: RunID("run_00000000000000000000000000000001"), Key: CacheKey{Digest: key, Kind: "statement"}, SchemaVersion: "cpgen.cache/v1", PolicyDigest: policy, InputDigest: input, At: time.Now().UTC()}
	if err := lookup.Validate(); err != nil {
		t.Fatal(err)
	}
	candidate := CacheCandidate{Entry: CacheEntry{Key: lookup.Key, Kind: "statement", SchemaVersion: lookup.SchemaVersion, PolicyDigest: policy, InputDigest: SumBytes([]byte("other")), State: CacheEntryValid, CreatedAt: time.Now().UTC(), Source: CacheSource{RunID: lookup.RunID, CallRecordID: CallRecordID("callrec_00000000000000000000000000000001"), OccurrenceID: ArtifactOccurrenceID("occurrence_00000000000000000000000000000001"), Digest: key}, Blobs: []CacheBlob{{Blob: BlobRef{Digest: key, Size: 1}, Role: ArtifactOutput, MediaType: "text/plain", LogicalPath: "output", Provenance: ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}}}}, SourceCallRecordID: CallRecordID("callrec_00000000000000000000000000000001"), SourceOccurrenceIDs: []ArtifactOccurrenceID{"occurrence_00000000000000000000000000000001"}}
	if err := candidate.ValidateFor(lookup); err == nil {
		t.Fatal("candidate with mismatched input digest was accepted")
	}
}

func TestCacheEntryRejectsExpiredOrInvalidatedEntry(t *testing.T) {
	key := CacheKey{Digest: SumBytes([]byte("cache-key")), Kind: "statement"}
	entry := CacheEntry{Key: key, Kind: key.Kind, SchemaVersion: "cpgen.cache/v1", PolicyDigest: SumBytes([]byte("policy")), InputDigest: SumBytes([]byte("input")), State: CacheEntryValid, ExpiresAt: timePtr(time.Now().UTC().Add(-time.Minute)), CreatedAt: time.Now().UTC().Add(-2 * time.Minute), Source: CacheSource{RunID: RunID("run_00000000000000000000000000000001"), CallRecordID: CallRecordID("callrec_00000000000000000000000000000001"), OccurrenceID: ArtifactOccurrenceID("occurrence_00000000000000000000000000000001"), Digest: SumBytes([]byte("cache-key"))}, Blobs: []CacheBlob{{Blob: BlobRef{Digest: key.Digest, Size: 1}, Role: ArtifactOutput, MediaType: "text/plain", LogicalPath: "output", Provenance: ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}}}}
	lookup := CacheLookup{RunID: RunID("run_00000000000000000000000000000001"), Key: key, SchemaVersion: entry.SchemaVersion, PolicyDigest: entry.PolicyDigest, InputDigest: entry.InputDigest, At: time.Now().UTC()}
	candidate := CacheCandidate{Entry: entry, SourceCallRecordID: entry.Source.CallRecordID, SourceOccurrenceIDs: []ArtifactOccurrenceID{entry.Source.OccurrenceID}}
	if err := candidate.ValidateFor(lookup); err == nil {
		t.Fatal("expired cache entry was accepted")
	}
	entry.State = CacheEntryInvalidated
	entry.ExpiresAt = timePtr(time.Now().UTC().Add(time.Minute))
	candidate = CacheCandidate{Entry: entry, SourceCallRecordID: CallRecordID("callrec_00000000000000000000000000000001"), SourceOccurrenceIDs: []ArtifactOccurrenceID{"occurrence_00000000000000000000000000000001"}}
	lookup = CacheLookup{RunID: RunID("run_00000000000000000000000000000001"), Key: key, SchemaVersion: "cpgen.cache/v1", PolicyDigest: entry.PolicyDigest, InputDigest: entry.InputDigest, At: time.Now().UTC()}
	if err := candidate.ValidateFor(lookup); err == nil {
		t.Fatal("invalidated cache entry was accepted as usable")
	}
}

func timePtr(value time.Time) *time.Time { return &value }
