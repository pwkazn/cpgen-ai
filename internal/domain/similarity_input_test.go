package domain

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestSimilarityInputBindsContentAndEveryExecutionPolicy(t *testing.T) {
	_, _, _, _, problem := testContentChain(t)
	projection := SumBytes([]byte("package-safe projection"))
	input, err := NewSimilarityInputV1(problem, projection, SumBytes([]byte("decision")), SumBytes([]byte("provider")), SumBytes([]byte("execution")), 20)
	if err != nil || input.ValidateProblem(problem, projection) != nil {
		t.Fatalf("input=%+v err=%v", input, err)
	}
	digest, err := input.Digest()
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*SimilarityInputV1){
		func(i *SimilarityInputV1) { i.RequestSnapshotDigest = SumBytes([]byte("different snapshot")) },
		func(i *SimilarityInputV1) { i.ProblemSpecDigest = SumBytes([]byte("different problem")) },
		func(i *SimilarityInputV1) { i.CandidateProjectionDigest = SumBytes([]byte("different projection")) },
		func(i *SimilarityInputV1) { i.DecisionPolicyDigest = SumBytes([]byte("different decision")) },
		func(i *SimilarityInputV1) { i.ProviderPolicyDigest = SumBytes([]byte("different provider")) },
		func(i *SimilarityInputV1) { i.ExecutionPolicyDigest = SumBytes([]byte("different retry/cost")) },
		func(i *SimilarityInputV1) { i.Limit++ },
	} {
		altered := input
		change(&altered)
		actual, err := altered.Digest()
		if err != nil || actual == digest {
			t.Fatal("changed semantic input retained its identity")
		}
	}
	if input.ValidateProblem(problem, SumBytes([]byte("substituted projection"))) == nil {
		t.Fatal("foreign query projection was accepted")
	}
	problem.SpecDigest = SumBytes([]byte("substituted problem"))
	if input.ValidateProblem(problem, projection) == nil {
		t.Fatal("foreign problem was accepted")
	}
}

func TestSimilarityInputStrictJSONAndRoundTrip(t *testing.T) {
	_, _, _, _, problem := testContentChain(t)
	digest := SumBytes([]byte("fixture"))
	input, err := NewSimilarityInputV1(problem, digest, digest, digest, digest, 20)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := input.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded SimilarityInputV1
	if err := json.Unmarshal(raw, &decoded); err != nil || !reflect.DeepEqual(input, decoded) {
		t.Fatalf("round trip: %v", err)
	}
	for _, invalid := range [][]byte{
		bytes.Replace(raw, []byte(`"limit":20`), []byte(`"limit":0`), 1),
		bytes.Replace(raw, []byte(`"limit":20`), []byte(`"limit":10001`), 1),
		bytes.Replace(raw, []byte(`"limit":20`), []byte(`"limit":20,"limit":21`), 1),
		bytes.Replace(raw, []byte(`"limit":20`), []byte(`"limit":20,"private_query":"must not be here"`), 1),
		bytes.Replace(raw, []byte(SimilarityInputSchemaV1), []byte("unsupported/v2"), 1),
	} {
		if json.Unmarshal(invalid, &decoded) == nil {
			t.Fatalf("invalid semantic input admitted: %s", invalid)
		}
	}
}
