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
