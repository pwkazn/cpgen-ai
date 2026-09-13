package domain

import (
	"testing"
	"time"
)

func TestMutationClaimRejectsOverflowAndInvalidIdentity(t *testing.T) {
	base := MutationClaimRequest{RunID: RunID("run_00000000000000000000000000000001"), StageName: "generate", ScopeDigest: SumBytes([]byte("scope")), SourceBatchDigest: SumBytes([]byte("sources")), Ordinal: 1, Limit: 1, LimitSnapshot: 1, Kind: MutationContent, IntentDigest: SumBytes([]byte("intent")), At: testCanonicalTime()}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	base.Ordinal = 0
	if err := base.Validate(); err == nil {
		t.Fatal("zero mutation ordinal was accepted")
	}
	base = MutationClaimRequest{RunID: RunID("run_00000000000000000000000000000001"), StageName: "generate", ScopeDigest: SumBytes([]byte("scope")), SourceBatchDigest: SumBytes([]byte("sources")), Ordinal: 1, Limit: 1, LimitSnapshot: 1, Kind: MutationContent, IntentDigest: SumBytes([]byte("intent")), At: testCanonicalTime()}
	base.Limit = 1<<63 - 2
	base.LimitSnapshot = base.Limit
	base.Ordinal = 1<<63 - 1
	if err := base.Validate(); err == nil {
		t.Fatal("overflow-prone mutation claim was accepted")
	}
}

func testCanonicalTime() (value time.Time) { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }
