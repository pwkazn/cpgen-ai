package domain

import (
	"bytes"
	"testing"
)

func TestProgramContextPreservesIntegerPrecision(t *testing.T) {
	// Only answer/explanation fields may disappear. JSON projection must not
	// silently round root seeds or integer constraints through float64.
	raw := []byte(`{"snapshot":{"effective_seed":9007199254740993},"problem":{"limit":9223372036854775807,"samples":[{"input":"1\n","output":"old","explanation":"stale"}]}}`)
	projected, err := programContext(raw, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, integer := range []string{"9007199254740993", "9223372036854775807"} {
		if !bytes.Contains(projected, []byte(integer)) {
			t.Fatalf("projection rounded integer %s: %s", integer, projected)
		}
	}
	if bytes.Contains(projected, []byte("old")) || bytes.Contains(projected, []byte("stale")) || !bytes.Contains(projected, []byte(SumBytes(raw))) {
		t.Fatal("projection lost answer isolation or source identity")
	}
}
