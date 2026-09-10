package domain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func testGenerationRequest() GenerationRequestV1 {
	seed := int64(-42)
	return GenerationRequestV1{SchemaVersion: RequestSchemaV1, Mode: "manual", Brief: "Graphs", Tags: []string{"graphs"}, NormalizedTags: []string{"graphs"}, Language: "en", Difficulty: "hard", RequiredFeatures: []string{"connected"}, ForbiddenFeatures: []string{"interactive"}, TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", Seed: &seed, VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: BudgetLimits{MaxLLMCalls: 4}}
}

func testContentChain(t *testing.T) (GenerationRequestSnapshotV1, IdeaBatch, IdeaSelection, StatementInput, ProblemSpec) {
	t.Helper()
	snap, err := NewGenerationRequestSnapshotV1(testGenerationRequest(), -42)
	if err != nil {
		t.Fatal(err)
	}
	candidates := []IdeaCandidate{
		{CandidateOrdinal: 1, AbstractTask: "B", IntendedAlgorithm: "heap", TargetComplexity: "O(n log n)", FeasibilityStatus: "FEASIBLE"},
		{CandidateOrdinal: 0, AbstractTask: "A", IntendedAlgorithm: "bfs", TargetComplexity: "O(n)", FeasibilityStatus: "FEASIBLE"},
	}
	batch, err := NewIdeaBatch(snap, 2, GenerationPolicyV1, candidates, 3)
	if err != nil {
		t.Fatal(err)
	}
	sel, err := NewIdeaSelection(snap.RequestDigest, batch, batch.Candidates[0].IdeaID, SelectionOrdinalPolicyV1, []string{"stable"}, []Digest{SumBytes([]byte("evidence"))})
	if err != nil {
		t.Fatal(err)
	}
	in := StatementInput{SchemaVersion: StatementInputSchemaV1, RequestSnapshotDigest: snap.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: sel.SelectionDigest, SelectedIdeaID: sel.SelectedIdeaID}
	spec, err := NewProblemSpec(in, snap, batch, sel, ProblemSpec{Revision: 1, Title: "Graphs", Description: "Find distances.", Input: ProblemIO{Description: "A graph", Fields: []string{"n", "edges"}}, Output: ProblemIO{Description: "Distances", Fields: []string{"distances"}}, Samples: []ProblemSample{{Input: "1 0", Output: "0", Explanation: "One vertex"}}})
	if err != nil {
		t.Fatal(err)
	}
	return snap, batch, sel, in, spec
}

func TestGenerationRequestPreservesRunRequestAndCopies(t *testing.T) {
	r := testGenerationRequest()
	original := RunRequest(r)
	got, err := GenerationRequestFromRunRequest(original)
	if err != nil {
		t.Fatal(err)
	}
	back, err := got.ToRunRequest()
	if err != nil || !reflect.DeepEqual(original, back) {
		t.Fatalf("lossy conversion: %#v %v", back, err)
	}
	var decoded GenerationRequestV1
	if err := json.Unmarshal(mustJSON(t, original), &decoded); err != nil {
		t.Fatal(err)
	}
	r.Brief = " Cafe\u0301 "
	r.Tags = []string{" Trees ", "graphs", "Graphs"}
	r.NormalizedTags = []string{"graphs", "trees"}
	r.RequiredFeatures = []string{" connected ", "connected"}
	snap, err := NewGenerationRequestSnapshotV1(r, *r.Seed)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(snap.Request, r) {
		t.Fatalf("request changed: %#v", snap.Request)
	}
	*r.Seed = 88
	r.Tags[0] = "changed"
	r.RequiredFeatures[0] = "changed"
	r.ForbiddenFeatures[0] = "changed"
	r.ExportTargets[0] = "changed"
	if err := snap.Validate(); err != nil {
		t.Fatalf("aliased request: %v", err)
	}
	if _, err := NewGenerationRequestSnapshotV1(testGenerationRequest(), 99); err == nil {
		t.Fatal("ignored explicit seed")
	}
	for name, mutate := range map[string]func(*GenerationRequestV1){
		"version":  func(r *GenerationRequestV1) { r.SchemaVersion = "cpgen.request/v2" },
		"conflict": func(r *GenerationRequestV1) { r.ForbiddenFeatures = []string{"connected"} },
		"budget":   func(r *GenerationRequestV1) { r.BudgetLimits.MaxLLMCalls = -1 },
		"utf8":     func(r *GenerationRequestV1) { r.Brief = "\xff" },
		"limit":    func(r *GenerationRequestV1) { r.TimeLimitMilliseconds = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			r := testGenerationRequest()
			mutate(&r)
			if err := r.Validate(); err == nil {
				t.Fatal("accepted invalid request")
			}
		})
	}
}

func TestContentContractsStrictDecodingEveryBoundary(t *testing.T) {
	snap, batch, sel, in, spec := testContentChain(t)
	for _, v := range []any{testGenerationRequest(), snap, batch.Candidates[0], batch, sel, in, spec} {
		t.Run(reflect.TypeOf(v).Name(), func(t *testing.T) {
			raw := mustJSON(t, v)
			for name, data := range map[string][]byte{
				"valid":      raw,
				"unknown":    append([]byte(`{"unknown":1,`), raw[1:]...),
				"case alias": append([]byte(`{"SCHEMA_VERSION":"duplicate",`), raw[1:]...),
				"null":       []byte(`null`),
				"duplicate":  append([]byte(`{"schema_version":"duplicate",`), raw[1:]...),
				"trailing":   append(append([]byte{}, raw...), []byte(` {}`)...),
				"version":    bytes.Replace(raw, []byte(`/v1`), []byte(`/v9`), 1),
			} {
				t.Run(name, func(t *testing.T) {
					dst := reflect.New(reflect.TypeOf(v)).Interface()
					err := json.Unmarshal(data, dst)
					if (err == nil) != (name == "valid") {
						t.Fatalf("decode %s: %v", data, err)
					}
					direct := reflect.New(reflect.TypeOf(v)).Interface().(json.Unmarshaler)
					if err := direct.UnmarshalJSON(data); (err == nil) != (name == "valid") {
						t.Fatalf("direct UnmarshalJSON %s: %v", data, err)
					}
				})
			}
		})
	}
}

func TestStrictJSONSurrogateEscapes(t *testing.T) {
	for _, tt := range []struct {
		name, raw string
		valid     bool
	}{
		{"pair", `"\ud83d\ude00"`, true}, {"upper pair", `"\uD83D\uDE00"`, true},
		{"high", `"\ud800"`, false}, {"low", `"\udc00"`, false}, {"low low", `"\udc00\udc00"`, false},
		{"high high", `"\ud800\ud800"`, false}, {"pair then low", `"\ud800\udc00\udc00"`, false},
		{"pair then pair", `"\ud800\udc00\ud801\udc01"`, true},
		{"escaped slash", `"\\ud800"`, true}, {"escaped then high", `"\\\ud800"`, false},
		{"high escaped low", `"\ud800\\udc00"`, false}, {"normal", `"\u00e9"`, true},
		{"malformed hex", `"\udxx0"`, false}, {"truncated", `"\ud80"`, false},
		{"quote", `"\"\\ud800"`, true}, {"duplicate nested", `{"x":{"a":1,"\u0061":2}}`, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var got any
			err := strictJSON(tt.raw, &got)
			if (err == nil) != tt.valid {
				t.Fatalf("strictJSON(%s)=%v", tt.raw, err)
			}
		})
	}
}

func TestIdeaBatchNormalizationOrdinalPolicyAndLineage(t *testing.T) {
	snap, batch, _, _, _ := testContentChain(t)
	candidates := candidateDrafts(batch.Candidates)
	candidates[0].AbstractTask = " Cafe\u0301 "
	b, err := NewIdeaBatch(snap, 2, GenerationPolicyV1, candidates, 4)
	if err != nil {
		t.Fatal(err)
	}
	candidates[0].AbstractTask = "changed"
	if err := b.Validate(); err != nil {
		t.Fatal(err)
	}
	if b.BatchOrdinal != 4 || b.Candidates[0].AbstractTask != "Café" || len(b.Candidates[0].SeedAxes) == 0 {
		t.Fatal("not normalized")
	}
	other, err := NewIdeaBatch(snap, 2, GenerationPolicyV1, candidateDrafts(b.Candidates), 5)
	if err != nil {
		t.Fatal(err)
	}
	if b.Candidates[0].IdeaID == other.Candidates[0].IdeaID {
		t.Fatal("batch ordinal not bound")
	}
	for name, mutate := range map[string]func(*IdeaCandidate){
		"empty task": func(c *IdeaCandidate) { c.AbstractTask = "" }, "empty complexity": func(c *IdeaCandidate) { c.TargetComplexity = "" },
		"rejected no reason": func(c *IdeaCandidate) { c.FeasibilityStatus = "REJECTED"; c.FeasibilityReasons = nil },
		"parent alone":       func(c *IdeaCandidate) { c.ParentIdeaID = batch.Candidates[1].IdeaID },
		"ordinal alone":      func(c *IdeaCandidate) { c.MutationOrdinal = 1 },
		"self parent":        func(c *IdeaCandidate) { c.ParentIdeaID = c.IdeaID; c.MutationOrdinal = 1; c.MutationReason = "retry" },
	} {
		t.Run(name, func(t *testing.T) {
			c := b.Candidates[0]
			mutate(&c)
			if c.Validate() == nil {
				t.Fatal("accepted invalid candidate")
			}
		})
	}
	if _, err := NewIdeaBatch(snap, 2, "unknown", b.Candidates); err == nil {
		t.Fatal("accepted unknown policy")
	}
}

func TestSelectionValidateEnforcesStablePreferenceAndEvidence(t *testing.T) {
	snap, b, sel, _, _ := testContentChain(t)
	sel.SelectedIdeaID = sel.OrderedCandidateIDs[1]
	sel.SelectionDigest = ""
	sel.SelectionDigest = contentSum(sel)
	if sel.Validate(b) == nil {
		t.Fatal("accepted resealed non-preferred selection")
	}
	ids, err := b.OrderedFeasibleCandidateIDs(SelectionIdeaIDPolicyV1)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewIdeaSelection(snap.RequestDigest, b, ids[0], SelectionIdeaIDPolicyV1, []string{" z ", "a", "a"}, []Digest{SumBytes([]byte("z")), SumBytes([]byte("z"))})
	if err != nil {
		t.Fatal(err)
	}
	if err := other.Validate(b); err != nil {
		t.Fatal(err)
	}
	if len(other.ReasonCodes) != 2 || len(other.EvidenceDigests) != 1 {
		t.Fatal("sets not normalized")
	}
	if _, err := b.OrderedFeasibleCandidateIDs("unknown"); err == nil {
		t.Fatal("accepted unknown policy")
	}
	rejected := candidateDrafts(b.Candidates)
	rejected[0].FeasibilityStatus = "REJECTED"
	rejected[0].FeasibilityReasons = []string{"constraint"}
	rb, err := NewIdeaBatch(snap, 2, GenerationPolicyV1, rejected)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewIdeaSelection(snap.RequestDigest, rb, rb.Candidates[0].IdeaID, SelectionOrdinalPolicyV1, []string{"reason"}, nil); err == nil {
		t.Fatal("selected rejected candidate")
	}
}

func TestProblemSpecChainBindsRequestSelectionAndContent(t *testing.T) {
	s, b, sel, in, p := testContentChain(t)
	if err := p.ValidateChain(s, b, sel); err != nil {
		t.Fatal(err)
	}
	if err := in.ValidateChain(s, b, sel); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ProblemSpec){
		"language": func(p *ProblemSpec) { p.Language = "zh-CN" }, "limit": func(p *ProblemSpec) { p.TimeLimitMS++ },
		"constraints": func(p *ProblemSpec) { p.RequiredConstraints = nil }, "algorithm": func(p *ProblemSpec) { p.IntendedAlgorithm = "wrong" },
		"selection": func(p *ProblemSpec) { p.IdeaSelectionDigest = SumBytes([]byte("stale")) },
		"revision":  func(p *ProblemSpec) { p.Revision = 0 }, "sample": func(p *ProblemSpec) { p.Samples = nil },
	} {
		t.Run(name, func(t *testing.T) {
			copy := p
			mutate(&copy)
			copy.SpecDigest = ""
			copy.SpecDigest = contentSum(copy)
			if copy.ValidateChain(s, b, sel) == nil {
				t.Fatal("accepted divergent spec")
			}
		})
	}
	p.Samples[0].Output = "wrong"
	if p.Validate() == nil {
		t.Fatal("sample not digest bound")
	}
}

func TestAllContentDigestsAndCanonicalRoundTrip(t *testing.T) {
	s, b, sel, in, p := testContentChain(t)
	type contract interface {
		CanonicalJSON() ([]byte, error)
		Digest() (Digest, error)
	}
	for _, v := range []contract{testGenerationRequest(), s, b.Candidates[0], b, sel, in, p} {
		raw, err := v.CanonicalJSON()
		if err != nil {
			t.Fatal(err)
		}
		d, err := v.Digest()
		if err != nil || d.Validate() != nil {
			t.Fatalf("digest: %s %v", d, err)
		}
		target := reflect.New(reflect.TypeOf(v)).Interface()
		if err := json.Unmarshal(raw, target); err != nil {
			t.Fatal(err)
		}
		if err := canonicalJSON(raw); err != nil {
			t.Fatalf("incompatible with Slice 1 canonical JSON: %v", err)
		}
		roundTrip, err := target.(contract).CanonicalJSON()
		if err != nil || !bytes.Equal(raw, roundTrip) {
			t.Fatal("unstable canonical encoding")
		}
	}
}

func TestContentCanonicalEncodingPreservesLargeIntegerSeed(t *testing.T) {
	r := testGenerationRequest()
	seed := int64(9223372036854775807)
	r.Seed = &seed
	s, err := NewGenerationRequestSnapshotV1(r, seed)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded GenerationRequestSnapshotV1
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.EffectiveSeed != seed || *decoded.Request.Seed != seed {
		t.Fatal("integer precision lost")
	}
}

func TestContentConstructorsDetachEveryMutableField(t *testing.T) {
	s, b, _, in, p := testContentChain(t)
	candidates := b.Candidates
	candidates[0].FeasibilityReasons = []string{"feasible"}
	candidates[0].NegativeConstraints = []string{"no weights"}
	cloned, err := NewIdeaBatch(s, 2, GenerationPolicyV1, candidates, b.BatchOrdinal)
	if err != nil {
		t.Fatal(err)
	}
	candidates[0].FeasibilityReasons[0] = "mutated"
	candidates[0].NegativeConstraints[0] = "mutated"
	if err := cloned.Validate(); err != nil {
		t.Fatal(err)
	}
	reasons := []string{"reason"}
	evidence := []Digest{SumBytes([]byte("evidence"))}
	sel, err := NewIdeaSelection(s.RequestDigest, cloned, cloned.Candidates[0].IdeaID, SelectionOrdinalPolicyV1, reasons, evidence)
	if err != nil {
		t.Fatal(err)
	}
	reasons[0] = "changed"
	evidence[0] = SumBytes([]byte("changed"))
	if err := sel.Validate(cloned); err != nil {
		t.Fatal(err)
	}
	in.IdeaBatchDigest = cloned.BatchDigest
	in.IdeaSelectionDigest = sel.SelectionDigest
	in.SelectedIdeaID = sel.SelectedIdeaID
	spec, err := NewProblemSpec(in, s, cloned, sel, p)
	if err != nil {
		t.Fatal(err)
	}
	p.Input.Fields[0] = "changed"
	p.Output.Fields[0] = "changed"
	p.Samples[0].Input = "changed"
	if err := spec.ValidateChain(s, cloned, sel); err != nil {
		t.Fatal(err)
	}
}

func TestBatchAndSelectionPoliciesRejectInvalidMatrices(t *testing.T) {
	s, original, _, _, _ := testContentChain(t)
	for name, change := range map[string]func(*IdeaBatch){
		"negative batch": func(b *IdeaBatch) { b.BatchOrdinal = -1 },
		"count":          func(b *IdeaBatch) { b.RequestedCount = 3 },
		"ordinal":        func(b *IdeaBatch) { b.Candidates[1].CandidateOrdinal = 0 },
		"lineage initial": func(b *IdeaBatch) {
			b.BatchOrdinal = 0
			b.Candidates[0].MutationOrdinal = 1
			b.Candidates[0].MutationReason = "retry"
		},
		"duplicate lineage": func(b *IdeaBatch) {
			for i := range b.Candidates {
				b.Candidates[i].MutationOrdinal = 1
				b.Candidates[i].MutationReason = "retry"
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			b := original
			b.Candidates = candidateDrafts(original.Candidates)
			change(&b)
			if _, err := NewIdeaBatch(s, b.RequestedCount, b.GenerationPolicyVersion, b.Candidates, b.BatchOrdinal); err == nil {
				t.Fatal("accepted invalid batch")
			}
		})
	}
	mutated := candidateDrafts(original.Candidates)
	mutated[0].ParentIdeaID = original.Candidates[0].IdeaID
	mutated[0].MutationOrdinal = 1
	mutated[0].MutationReason = "more variation"
	if _, err := NewIdeaBatch(s, 2, GenerationPolicyV1, mutated, 4); err != nil {
		t.Fatalf("valid lineage: %v", err)
	}
	for i := range mutated {
		mutated[i].FeasibilityStatus = "REJECTED"
		mutated[i].FeasibilityReasons = []string{"constraint"}
	}
	b, err := NewIdeaBatch(s, 2, GenerationPolicyV1, mutated, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewIdeaSelection(s.RequestDigest, b, b.Candidates[0].IdeaID, SelectionOrdinalPolicyV1, []string{"reason"}, nil); err == nil {
		t.Fatal("selected from all-rejected batch")
	}
	// Find a deterministic fixture where ID order differs from candidate ordinal.
	for ordinal := 0; ordinal < 32; ordinal++ {
		b, err := NewIdeaBatch(s, 2, GenerationPolicyV1, candidateDrafts(original.Candidates), ordinal)
		if err != nil {
			t.Fatal(err)
		}
		ids, err := b.OrderedFeasibleCandidateIDs(SelectionIdeaIDPolicyV1)
		if err != nil {
			t.Fatal(err)
		}
		if ids[0] != b.Candidates[0].IdeaID {
			if _, err := NewIdeaSelection(s.RequestDigest, b, ids[0], SelectionOrdinalPolicyV1, []string{"reason"}, nil); err == nil {
				t.Fatal("policy did not affect selection")
			}
			if _, err := NewIdeaSelection(s.RequestDigest, b, ids[0], SelectionIdeaIDPolicyV1, []string{"reason"}, nil); err != nil {
				t.Fatal(err)
			}
			return
		}
	}
	t.Fatal("fixture did not exercise differing policy orders")
}

func TestRequestCanonicalEncodingPreservesSubmittedIdentity(t *testing.T) {
	a := testGenerationRequest()
	a.Brief = "Cafe\u0301"
	a.RequiredFeatures = []string{" z ", "a"}
	b := testGenerationRequest()
	b.Brief = " Café "
	b.RequiredFeatures = []string{"a", "z"}
	da, err := a.Digest()
	if err != nil {
		t.Fatal(err)
	}
	db, err := b.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if da == db {
		t.Fatal("distinct submitted requests lost their byte identity")
	}
}

func candidateDrafts(v []IdeaCandidate) []IdeaCandidate {
	out := append([]IdeaCandidate(nil), v...)
	for i := range out {
		out[i].SeedAxes = nil
	}
	return out
}

func TestSlice1RequestConversionPreservesAllLegalValues(t *testing.T) {
	for _, mode := range []string{"manual", "offline", "generate", " arbitrary "} {
		for _, empty := range []bool{false, true} {
			t.Run(mode+fmt.Sprint(empty), func(t *testing.T) {
				r := RunRequest(testGenerationRequest())
				r.Mode = mode
				r.Brief = "  Cafe\u0301\n "
				r.Tags = []string{"trees", "graphs"}
				r.NormalizedTags = nil
				r.RequiredFeatures = []string{" z ", "a"}
				r.ForbiddenFeatures = nil
				r.ExportTargets = nil
				if empty {
					r.Tags = []string{}
					r.NormalizedTags = []string{}
					r.ForbiddenFeatures = []string{}
					r.ExportTargets = []string{}
				}
				if err := r.Validate(); err != nil {
					t.Fatal(err)
				}
				original, err := contentJSON(r, nil)
				if err != nil {
					t.Fatal(err)
				}
				got, err := GenerationRequestFromRunRequest(r)
				if err != nil {
					t.Fatal(err)
				}
				back, err := got.ToRunRequest()
				if err != nil || !reflect.DeepEqual(r, back) {
					t.Fatalf("lossy adapter: %#v %v", back, err)
				}
				raw, err := got.CanonicalJSON()
				if err != nil || !bytes.Equal(raw, original) {
					t.Fatalf("submitted canonical bytes changed: %s %v", raw, err)
				}
				d, err := got.Digest()
				if err != nil || d != SumBytes(original) {
					t.Fatal("submitted digest changed")
				}
				back.RequiredFeatures[0] = "changed"
				*back.Seed = 77
				if reflect.DeepEqual(got.RequiredFeatures, back.RequiredFeatures) || *got.Seed == 77 {
					t.Fatal("adapter aliases source")
				}
			})
		}
	}
}

func TestGenerationRequestModeAndTagPolicy(t *testing.T) {
	for _, tt := range []struct {
		name, mode, brief string
		tags, normalized  []string
		valid             bool
	}{
		{"manual", "manual", "brief", []string{"graphs"}, []string{"graphs"}, true},
		{"random empty", "random", "", nil, nil, true},
		{"random seeded brief", "random", "hint", []string{" DP ", "graphs"}, []string{"dp", "graphs"}, true},
		{"manual empty", "manual", " ", nil, nil, false},
		{"unknown mode", "generate", "brief", nil, nil, false},
		{"unknown tag", "manual", "brief", []string{"not-a-topic"}, []string{"not-a-topic"}, false},
		{"mismatch", "manual", "brief", []string{"trees"}, []string{"graphs"}, false},
		{"missing derived", "manual", "brief", []string{"graphs"}, nil, false},
		{"sort", "manual", "brief", []string{"trees", "graphs"}, []string{"trees", "graphs"}, false},
		{"duplicate", "manual", "brief", []string{"graphs"}, []string{"graphs", "graphs"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := testGenerationRequest()
			r.Mode = tt.mode
			r.Brief = tt.brief
			r.Tags = tt.tags
			r.NormalizedTags = tt.normalized
			if err := r.Validate(); (err == nil) != tt.valid {
				t.Fatalf("Validate: %v", err)
			}
		})
	}
}

func TestGenerationRandomRequestRoundTripsThroughRunAdmission(t *testing.T) {
	r := testGenerationRequest()
	r.Mode, r.Brief, r.Seed = RequestModeRandom, "", nil
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	raw, err := r.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	run, err := r.ToRunRequest()
	if err != nil {
		t.Fatalf("valid random request cannot enter run persistence: %v", err)
	}
	restored, err := GenerationRequestFromRunRequest(run)
	if err != nil || !reflect.DeepEqual(restored, r) {
		t.Fatalf("restored=%+v err=%v", restored, err)
	}
	again, err := restored.CanonicalJSON()
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("random admission rewrote submitted content")
	}
	for _, invalid := range []RunRequest{func() RunRequest { v := run; v.Mode = RequestModeManual; return v }(), func() RunRequest { v := run; v.SchemaVersion = "cpgen.request/v2"; return v }(), func() RunRequest { v := run; v.Mode = "generate"; return v }()} {
		if err := invalid.Validate(); err == nil {
			t.Fatalf("blank brief accepted outside admitted random/v1: %+v", invalid)
		}
	}
}

func TestSnapshotDigestEqualsUnmodifiedSubmittedRequest(t *testing.T) {
	r := testGenerationRequest()
	r.Brief = " Cafe\u0301 "
	r.Tags = []string{" Trees ", "graphs"}
	r.NormalizedTags = []string{"graphs", "trees"}
	r.ForbiddenFeatures = nil
	r.ExportTargets = []string{}
	raw, err := contentJSON(RunRequest(r), nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewGenerationRequestSnapshotV1(r, *r.Seed)
	if err != nil {
		t.Fatal(err)
	}
	if s.RequestDigest != SumBytes(raw) || !reflect.DeepEqual(r, s.Request) {
		t.Fatal("snapshot changed submitted request")
	}
	encoded, err := s.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded GenerationRequestSnapshotV1
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RequestDigest != SumBytes(raw) || !reflect.DeepEqual(r, decoded.Request) {
		t.Fatal("snapshot roundtrip changed source")
	}
	decoded.Request.Brief = "Café"
	decoded.SnapshotDigest = ""
	decoded.SnapshotDigest = contentSum(decoded)
	if decoded.Validate() == nil {
		t.Fatal("source request identity no longer bound")
	}
}

func TestProblemSamplePreservesOpaqueData(t *testing.T) {
	s, b, sel, in, p := testContentChain(t)
	for _, data := range []string{"  a  \n\n", "\te\u0301 \n", "\n", "", "  "} {
		t.Run(fmt.Sprintf("%q", data), func(t *testing.T) {
			p.Samples = []ProblemSample{{Input: data, Output: data, Explanation: " Cafe\u0301 "}}
			spec, err := NewProblemSpec(in, s, b, sel, p)
			if err != nil {
				t.Fatal(err)
			}
			if spec.Samples[0].Input != data || spec.Samples[0].Output != data || spec.Samples[0].Explanation != "Café" {
				t.Fatal("opaque sample bytes changed")
			}
			raw, err := spec.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			var decoded ProblemSpec
			if err := json.Unmarshal(raw, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Samples[0].Input != data || decoded.Samples[0].Output != data {
				t.Fatal("opaque sample bytes changed on decode")
			}
		})
	}
	for _, data := range []string{"\xff", "a\r\n", strings.Repeat("a", 65537)} {
		p.Samples = []ProblemSample{{Input: data, Output: "ok"}}
		if _, err := NewProblemSpec(in, s, b, sel, p); err == nil {
			t.Fatal("accepted invalid sample data")
		}
	}
}

func TestBatchBindsCallBudgetAndDerivedSeedAxes(t *testing.T) {
	s, b, sel, in, _ := testContentChain(t)
	if b.CallBudget != s.Request.BudgetLimits || b.SeedDerivationPolicyVersion != SeedDerivationPolicyV1 {
		t.Fatal("missing budget or seed policy")
	}
	for _, c := range b.Candidates {
		axes, err := DeriveIdeaSeedAxes(s.RequestDigest, s.EffectiveSeed, b.BatchOrdinal, c.CandidateOrdinal, c.MutationOrdinal, SeedDerivationPolicyV1)
		if err != nil || !reflect.DeepEqual(c.SeedAxes, axes) {
			t.Fatal("seed axes not derived")
		}
	}
	for name, change := range map[string]func(*IdeaBatch){
		"unknown seed policy": func(b *IdeaBatch) { b.SeedDerivationPolicyVersion = "unknown" },
		"invented axes":       func(b *IdeaBatch) { b.Candidates[0].SeedAxes = []string{"clock:now"} },
		"negative budget":     func(b *IdeaBatch) { b.CallBudget.MaxLLMCalls = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			copy := b
			copy.Candidates = append([]IdeaCandidate{}, b.Candidates...)
			change(&copy)
			for i := range copy.Candidates {
				copy.Candidates[i].IdeaID = ideaID(copy, copy.Candidates[i])
			}
			copy.BatchDigest = ""
			copy.BatchDigest = contentSum(copy)
			if copy.Validate() == nil {
				t.Fatal("accepted invalid policy/budget")
			}
		})
	}
	b.CallBudget.MaxLLMCalls++
	b.BatchDigest = ""
	b.BatchDigest = contentSum(b)
	sel, err := NewIdeaSelection(s.RequestDigest, b, b.Candidates[0].IdeaID, SelectionOrdinalPolicyV1, []string{"reason"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	in.IdeaBatchDigest = b.BatchDigest
	in.IdeaSelectionDigest = sel.SelectionDigest
	if in.ValidateChain(s, b, sel) == nil {
		t.Fatal("request budget mismatch accepted")
	}
}

func TestProblemSpecBindsSelectedNegativeConstraints(t *testing.T) {
	s, b, _, in, p := testContentChain(t)
	drafts := candidateDrafts(b.Candidates)
	drafts[0].NegativeConstraints = []string{"no weights"}
	b, err := NewIdeaBatch(s, 2, GenerationPolicyV1, drafts, b.BatchOrdinal)
	if err != nil {
		t.Fatal(err)
	}
	sel, err := NewIdeaSelection(s.RequestDigest, b, b.Candidates[0].IdeaID, SelectionOrdinalPolicyV1, []string{"reason"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	in.IdeaBatchDigest = b.BatchDigest
	in.IdeaSelectionDigest = sel.SelectionDigest
	in.SelectedIdeaID = sel.SelectedIdeaID
	p, err = NewProblemSpec(in, s, b, sel, p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.NegativeConstraints, []string{"no weights"}) {
		t.Fatal("candidate negative constraints dropped")
	}
	p.NegativeConstraints = []string{}
	p.SpecDigest = ""
	p.SpecDigest = contentSum(p)
	if p.ValidateChain(s, b, sel) == nil {
		t.Fatal("negative constraints not chain bound")
	}
}

func TestStatementChainRejectsChangedSelectionEvidence(t *testing.T) {
	s, b, sel, in, p := testContentChain(t)
	sel.EvidenceDigests = []Digest{SumBytes([]byte("new evidence"))}
	sel.SelectionDigest = ""
	sel.SelectionDigest = contentSum(sel)
	if err := sel.Validate(b); err != nil {
		t.Fatal(err)
	}
	if in.ValidateChain(s, b, sel) == nil || p.ValidateChain(s, b, sel) == nil {
		t.Fatal("stale statement survived evidence change")
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
