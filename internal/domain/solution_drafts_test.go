package domain

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func solutionDraftInputFixture(t *testing.T) SolutionDraftInputV1 {
	t.Helper()
	snapshot, _, _, _, problem := testContentChain(t)
	input, err := NewSolutionDraftInput(snapshot, problem, SumBytes([]byte("similarity input")), SumBytes([]byte("accepted evidence")), SumBytes([]byte("accepted decision")))
	if err != nil {
		t.Fatal(err)
	}
	return input
}

func validSolutionDraft() SolutionDraftV1 {
	return SolutionDraftV1{SchemaVersion: SolutionDraftSchemaV1, ReferenceCode: "// café\r\nint main() { return 0; }\n", BruteCode: "int main() { return 0; }\n", Explanation: "Enumerate the candidates; the reference algorithm avoids repeated work."}
}

func TestSolutionDraftBindsExactInputAndPreservesSourceBytes(t *testing.T) {
	input := solutionDraftInputFixture(t)
	draft := validSolutionDraft()
	content, err := draft.Bind(input)
	if err != nil {
		t.Fatal(err)
	}
	if content.ReferenceCode != draft.ReferenceCode || content.BruteCode != draft.BruteCode || content.Language != input.Snapshot.Request.SolutionLanguage || content.ProblemSpecDigest != input.Problem.SpecDigest {
		t.Fatalf("solution binding lost source or frozen fields: %+v", content)
	}
	first, err := content.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := draft.Bind(input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := replayed.CanonicalJSON()
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("non-deterministic binding: %v", err)
	}
	input.SimilarityEvidenceDigest = SumBytes([]byte("different evidence"))
	if err := content.ValidateInput(input); err == nil {
		t.Fatal("solution accepted a different similarity source")
	}
	content.ReferenceCode += "// changed"
	if err := content.Validate(); err == nil {
		t.Fatal("changed source retained a valid content digest")
	}
}

func TestSolutionDraftInputRejectsCrossedRequestAndClonesSamples(t *testing.T) {
	input := solutionDraftInputFixture(t)
	cloned, err := NewSolutionDraftInput(input.Snapshot, input.Problem, input.SimilarityInputDigest, input.SimilarityEvidenceDigest, input.SimilarityDecisionDigest)
	if err != nil {
		t.Fatal(err)
	}
	input.Problem.Samples[0].Input = "changed"
	if err := cloned.Validate(); err != nil {
		t.Fatalf("constructor aliased problem samples: %v", err)
	}
	cloned.Snapshot.Request.Brief += " different request"
	changed, err := NewGenerationRequestSnapshotV1(cloned.Snapshot.Request, cloned.Snapshot.EffectiveSeed)
	if err != nil {
		t.Fatal(err)
	}
	cloned.Snapshot = changed
	if err := cloned.Validate(); err == nil {
		t.Fatal("crossed snapshot and problem accepted")
	}
}

func TestSolutionDraftRejectsMalformedAndProviderOwnedIdentity(t *testing.T) {
	raw, err := json.Marshal(validSolutionDraft())
	if err != nil {
		t.Fatal(err)
	}
	for name, candidate := range map[string][]byte{
		"forged verdict": append([]byte(`{"verified":true,`), raw[1:]...),
		"forged digest":  append([]byte(`{"content_digest":"forged",`), raw[1:]...),
		"duplicate":      append([]byte(`{"schema_version":"cpgen.solution-draft/v1",`), raw[1:]...),
		"missing source": []byte(`{"schema_version":"cpgen.solution-draft/v1","reference_code":"x","explanation":"x"}`),
		"field alias":    []byte(strings.Replace(string(raw), `"reference_code"`, `"Reference_code"`, 1)),
	} {
		t.Run(name, func(t *testing.T) {
			var draft SolutionDraftV1
			if err := json.Unmarshal(candidate, &draft); err == nil {
				t.Fatal("malformed or forged draft accepted")
			}
		})
	}
	for _, source := range []string{"", " \n\t", "x\x00y", string([]byte{0xff}), strings.Repeat("x", 262145)} {
		draft := validSolutionDraft()
		draft.ReferenceCode = source
		if err := draft.Validate(); err == nil {
			t.Fatalf("invalid source accepted: %d bytes", len(source))
		}
	}
}
