package domain

import (
	"bytes"
	"encoding/json"
	"testing"
)

func testGenerationRequest() GenerationRequestV1 {
	return GenerationRequestV1{SchemaVersion: 1, TitleHint: "Graphs", TopicTags: []string{"graphs", "shortest-path"}, DifficultyLower: 1800, DifficultyUpper: 2200, TimeLimitMS: 2000, MemoryLimitMB: 512, Languages: []string{"cpp"}, SimilarityRequired: true, SimilarityThreshold: 0.82, OutputFormat: "internal"}
}

func TestGenerationRequestV1CanonicalAndStrictValidation(t *testing.T) {
	r := testGenerationRequest()
	b, err := r.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, mustJSON(t, r)) {
		t.Fatal("canonical JSON differs from encoding/json")
	}
	d, err := r.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if d != SumBytes(b) {
		t.Fatal("digest is not bound to canonical JSON")
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"schema_version":1,"bogus":1}`, `{"schema_version":1,"title_hint":"\ud800"}`, `{"schema_version":1,"difficulty_lower":3,"difficulty_upper":2}`, `{"schema_version":1,"similarity_threshold":1.1}`} {
		var got GenerationRequestV1
		if err := strictJSON(raw, &got); err == nil {
			t.Fatalf("accepted invalid request %s", raw)
		}
	}
}

func TestContentContractsSelectionAndDigestChain(t *testing.T) {
	r := testGenerationRequest()
	snap, err := NewGenerationRequestSnapshotV1(r, 42)
	if err != nil {
		t.Fatal(err)
	}
	rd, _ := r.Digest()
	candidates := []IdeaCandidate{{CandidateOrdinal: 1, AbstractTask: "B", IntendedAlgorithm: "heap", FeasibilityStatus: "REJECTED", FeasibilityReasons: []string{"conflict"}}, {CandidateOrdinal: 0, AbstractTask: "A", IntendedAlgorithm: "bfs", FeasibilityStatus: "FEASIBLE"}}
	batch, err := NewIdeaBatch(snap, 2, "policy/v1", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Candidates[0].CandidateOrdinal != 0 {
		t.Fatal("batch did not order by ordinal")
	}
	if len(batch.FeasibleCandidateIDs()) != 1 {
		t.Fatal("wrong feasible set")
	}
	if _, err := NewIdeaSelection(rd, batch, batch.Candidates[1].IdeaID, "selection/v1", []string{"reason"}, nil); err == nil {
		t.Fatal("rejected candidate was selectable")
	}
	sel, err := NewIdeaSelection(rd, batch, batch.Candidates[0].IdeaID, "selection/v1", []string{"reason"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	in := StatementInput{RequestSnapshotDigest: snap.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: sel.SelectionDigest, SelectedIdeaID: sel.SelectedIdeaID}
	if err := in.ValidateChain(snap, batch, sel); err != nil {
		t.Fatal(err)
	}
	in.SelectedIdeaID = "wrong"
	if err := in.ValidateChain(snap, batch, sel); err == nil {
		t.Fatal("accepted broken chain")
	}
	// defensive copies
	ids := batch.FeasibleCandidateIDs()
	ids[0] = "mutated"
	if batch.Candidates[0].IdeaID == "mutated" {
		t.Fatal("returned slice aliases batch")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
