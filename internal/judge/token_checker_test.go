package judge_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/judge"
)

func TestExactTokenDigestRetainsExistingComparisonBytes(t *testing.T) {
	for _, raw := range [][]byte{nil, {}, []byte("a\u0085b\u3000c"), []byte("a\xff\x00b")} {
		encoded, err := json.Marshal(bytes.Fields(raw))
		if err != nil {
			t.Fatal(err)
		}
		if judge.ExactTokenDigest(raw) != domain.SumBytes(encoded) {
			t.Fatal("token digest changed existing report semantics")
		}
	}
	source := judge.ExactTokenCheckerSource()
	source[0] = '!'
	if judge.ExactTokenCheckerSource()[0] == '!' {
		t.Fatal("caller mutated compiled checker source")
	}
}

func TestExactTokenCheckerSourcePreservesHistoricalPolicyBytes(t *testing.T) {
	// Quality policy and exported packages bind these embedded bytes. A Windows
	// CRLF checkout must not change the fixed checker's historical identity.
	source := judge.ExactTokenCheckerSource()
	const want = domain.Digest("sha256:a9408e8d000f85718717d8ff2690395c93c8403fd73bf3a0fca962ccf8d63fb9")
	if got := domain.SumBytes(source); got != want || bytes.Contains(source, []byte("\r")) {
		t.Fatalf("embedded checker identity changed: got %s, want %s", got, want)
	}
}
