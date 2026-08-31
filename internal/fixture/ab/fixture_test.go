//go:build cpgen_slice0_probe

package ab_test

import (
	"slices"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/fixture/ab"
)

func TestABFixtureBundlesAreCanonicalAndComplete(t *testing.T) {
	store := &fixtureBlobs{refs: map[domain.Digest][]byte{}}
	definitions := ab.Definitions()
	if len(definitions) != 5 {
		t.Fatalf("definitions = %d, want 5", len(definitions))
	}
	wantNames := []string{"brute", "checker", "generator", "reference", "validator"}
	gotNames := make([]string, len(definitions))
	for index, definition := range definitions {
		gotNames[index] = definition.Name
		bundle, err := ab.Bundle(store, definition.Name)
		if err != nil {
			t.Fatal(err)
		}
		if err := bundle.Validate(); err != nil {
			t.Fatal(err)
		}
		if len(bundle.Files) != 1 || bundle.Files[0].Path != "main.cpp" || bundle.EntryPoint != "main.cpp" {
			t.Fatalf("bundle %s = %#v", definition.Name, bundle)
		}
	}
	if !slices.Equal(gotNames, wantNames) {
		t.Fatalf("definition order = %#v", gotNames)
	}
}

func TestABGeneratorCasesAreSeededBoundedAndStable(t *testing.T) {
	first := ab.GeneratedCases(ab.FixedSeed)
	second := ab.GeneratedCases(ab.FixedSeed)
	if !slices.Equal(first, second) || len(first) != 6 {
		t.Fatalf("generated cases = %#v", first)
	}
	for _, test := range first {
		if test.A < -1_000_000_000 || test.A > 1_000_000_000 || test.B < -1_000_000_000 || test.B > 1_000_000_000 {
			t.Fatalf("case is out of bounds: %#v", test)
		}
	}
	if slices.Equal(first, ab.GeneratedCases(ab.FixedSeed+1)) {
		t.Fatal("changing the seed did not change random cases")
	}
	if got := ab.RenderCases(first); got != "-1000000000 -1000000000\n1000000000 1000000000\n-1000000000 1000000000\n0 0\n179638503 754650125\n220213037 755577128\n" {
		t.Fatalf("seeded fixture bytes changed:\n%s", got)
	}
}

type fixtureBlobs struct{ refs map[domain.Digest][]byte }

func (s *fixtureBlobs) PutBlob(data []byte) domain.BlobRef {
	digest := domain.SumBytes(data)
	s.refs[digest] = slices.Clone(data)
	return domain.BlobRef{Digest: digest, Size: int64(len(data))}
}
