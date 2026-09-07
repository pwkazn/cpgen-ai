package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// GenerationRequestV1 is the stable, provider-independent content request.
type GenerationRequestV1 struct {
	SchemaVersion       int          `json:"schema_version"`
	Mode                string       `json:"mode,omitempty"`
	Brief               string       `json:"brief,omitempty"`
	TitleHint           string       `json:"title_hint"`
	TopicTags           []string     `json:"topic_tags"`
	DifficultyLower     int          `json:"difficulty_lower"`
	DifficultyUpper     int          `json:"difficulty_upper"`
	TimeLimitMS         int64        `json:"time_limit_ms"`
	MemoryLimitMB       int64        `json:"memory_limit_mb"`
	Languages           []string     `json:"languages"`
	RequiredFeatures    []string     `json:"required_features,omitempty"`
	ForbiddenFeatures   []string     `json:"forbidden_features,omitempty"`
	SolutionLanguage    string       `json:"solution_language,omitempty"`
	Seed                *int64       `json:"seed,omitempty"`
	VerificationProfile string       `json:"verification_profile,omitempty"`
	ExportTargets       []string     `json:"export_targets,omitempty"`
	BudgetLimits        BudgetLimits `json:"budget_limits,omitempty"`
	SimilarityRequired  bool         `json:"similarity_required"`
	SimilarityThreshold float64      `json:"similarity_threshold"`
	OutputFormat        string       `json:"output_format"`
}

// UnmarshalJSON performs strict decoding at the contract boundary.
func (r *GenerationRequestV1) UnmarshalJSON(data []byte) error {
	type plain GenerationRequestV1
	var v plain
	if err := strictJSON(string(data), &v); err != nil {
		return err
	}
	if err := (GenerationRequestV1(v)).Validate(); err != nil {
		return err
	}
	*r = GenerationRequestV1(v)
	return nil
}

// GenerationRequestSnapshotV1 freezes a request and its effective seed.
type GenerationRequestSnapshotV1 struct {
	Request        GenerationRequestV1 `json:"request"`
	EffectiveSeed  uint64              `json:"effective_seed"`
	RequestDigest  Digest              `json:"request_digest"`
	SnapshotDigest Digest              `json:"snapshot_digest"`
}

type IdeaCandidate struct {
	IdeaID              string   `json:"idea_id"`
	CandidateOrdinal    int      `json:"candidate_ordinal"`
	SeedAxes            []string `json:"seed_axes"`
	AbstractTask        string   `json:"abstract_task"`
	IntendedAlgorithm   string   `json:"intended_algorithm"`
	TargetComplexity    string   `json:"target_complexity"`
	FeasibilityStatus   string   `json:"feasibility_status"`
	FeasibilityReasons  []string `json:"feasibility_reasons"`
	NegativeConstraints []string `json:"negative_constraints"`
	ParentIdeaID        string   `json:"parent_idea_id,omitempty"`
	MutationReason      string   `json:"mutation_reason,omitempty"`
	MutationOrdinal     int      `json:"mutation_ordinal,omitempty"`
}

type IdeaBatch struct {
	RequestDigest           Digest          `json:"request_digest"`
	RequestedCount          int             `json:"requested_count"`
	Candidates              []IdeaCandidate `json:"candidates"`
	GenerationPolicyVersion string          `json:"generation_policy_version"`
	EffectiveSeed           uint64          `json:"effective_seed"`
	BatchOrdinal            int             `json:"batch_ordinal,omitempty"`
	BatchDigest             Digest          `json:"batch_digest"`
}

type IdeaSelection struct {
	RequestDigest          Digest   `json:"request_digest"`
	IdeaBatchDigest        Digest   `json:"idea_batch_digest"`
	SelectedIdeaID         string   `json:"selected_idea_id"`
	SelectionPolicyVersion string   `json:"selection_policy_version"`
	OrderedCandidateIDs    []string `json:"ordered_candidate_ids"`
	ReasonCodes            []string `json:"reason_codes"`
	EvidenceDigests        []Digest `json:"evidence_digests"`
	SelectionDigest        Digest   `json:"selection_digest"`
}

type StatementInput struct {
	RequestSnapshotDigest Digest `json:"request_snapshot_digest"`
	IdeaBatchDigest       Digest `json:"idea_batch_digest"`
	IdeaSelectionDigest   Digest `json:"idea_selection_digest"`
	SelectedIdeaID        string `json:"selected_idea_id"`
}

// ProblemSpec is the immutable structured statement input produced downstream.
type ProblemSpec struct {
	SchemaVersion          int      `json:"schema_version"`
	RequestDigest          Digest   `json:"request_digest"`
	IdeaBatchDigest        Digest   `json:"idea_batch_digest"`
	IdeaSelectionDigest    Digest   `json:"idea_selection_digest"`
	SelectedIdeaID         string   `json:"selected_idea_id"`
	SelectionPolicyVersion string   `json:"selection_policy_version"`
	Language               string   `json:"language"`
	TimeLimitMS            int64    `json:"time_limit_ms"`
	MemoryLimitMB          int64    `json:"memory_limit_mb"`
	RequiredConstraints    []string `json:"required_constraints"`
	ForbiddenConstraints   []string `json:"forbidden_constraints"`
}

func (p ProblemSpec) Validate() error {
	if p.SchemaVersion != 1 || p.SelectedIdeaID == "" || p.SelectionPolicyVersion == "" || p.Language == "" || p.TimeLimitMS <= 0 || p.MemoryLimitMB <= 0 {
		return errors.New("invalid problem spec")
	}
	if err := p.RequestDigest.Validate(); err != nil {
		return err
	}
	if err := p.IdeaBatchDigest.Validate(); err != nil {
		return err
	}
	if err := p.IdeaSelectionDigest.Validate(); err != nil {
		return err
	}
	return nil
}

func (r GenerationRequestV1) Validate() error {
	if r.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1")
	}
	if !utf8.ValidString(r.TitleHint) || len([]byte(r.TitleHint)) > 200 {
		return errors.New("title_hint is invalid or too long")
	}
	for _, s := range []string{r.Mode, r.Brief, r.SolutionLanguage, r.VerificationProfile} {
		if s != "" && (strings.TrimSpace(s) != s || !norm.NFC.IsNormalString(s) || len([]byte(s)) > 4096) {
			return errors.New("invalid request text")
		}
	}
	if r.DifficultyLower < 0 || r.DifficultyUpper < r.DifficultyLower {
		return errors.New("invalid difficulty range")
	}
	if r.TimeLimitMS <= 0 || r.MemoryLimitMB <= 0 {
		return errors.New("resource limits must be positive")
	}
	if len(r.Languages) == 0 {
		return errors.New("languages must not be empty")
	}
	if r.SimilarityThreshold < 0 || r.SimilarityThreshold > 1 {
		return errors.New("similarity_threshold out of range")
	}
	if r.OutputFormat == "" {
		return errors.New("output_format is required")
	}
	for _, s := range append(append([]string{}, r.TopicTags...), r.Languages...) {
		if !utf8.ValidString(s) || s == "" {
			return errors.New("invalid empty/non-UTF-8 tag or language")
		}
	}
	seen := map[string]bool{}
	for _, s := range append(append(append([]string{}, r.RequiredFeatures...), r.ForbiddenFeatures...), r.ExportTargets...) {
		if s == "" || strings.TrimSpace(s) != s || !norm.NFC.IsNormalString(s) || seen[s] {
			return errors.New("invalid or duplicate feature/export")
		}
		seen[s] = true
	}
	for _, req := range r.RequiredFeatures {
		for _, ban := range r.ForbiddenFeatures {
			if req == ban {
				return errors.New("required/forbidden feature conflict")
			}
		}
	}
	if err := r.BudgetLimits.Validate(); err != nil {
		return err
	}
	return nil
}

func (r GenerationRequestV1) CanonicalJSON() ([]byte, error) {
	if err := r.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(r)
}
func (r GenerationRequestV1) Digest() (Digest, error) {
	b, err := r.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return SumBytes(b), nil
}

func NewGenerationRequestSnapshotV1(r GenerationRequestV1, seed uint64) (GenerationRequestSnapshotV1, error) {
	r.TopicTags = append([]string(nil), r.TopicTags...)
	r.Languages = append([]string(nil), r.Languages...)
	rd, err := r.Digest()
	if err != nil {
		return GenerationRequestSnapshotV1{}, err
	}
	s := GenerationRequestSnapshotV1{Request: r, EffectiveSeed: seed, RequestDigest: rd}
	b, err := json.Marshal(s)
	if err != nil {
		return s, err
	}
	s.SnapshotDigest = SumBytes(b)
	return s, nil
}
func (s GenerationRequestSnapshotV1) Validate() error {
	rd, err := s.Request.Digest()
	if err != nil {
		return err
	}
	if s.RequestDigest != rd {
		return errors.New("request digest mismatch")
	}
	c := s
	c.SnapshotDigest = ""
	b, _ := json.Marshal(c)
	if s.SnapshotDigest != SumBytes(b) {
		return errors.New("snapshot digest mismatch")
	}
	return nil
}
func (s GenerationRequestSnapshotV1) CanonicalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return json.Marshal(s)
}

func NewIdeaBatch(s GenerationRequestSnapshotV1, requested int, policy string, candidates []IdeaCandidate) (IdeaBatch, error) {
	if err := s.Validate(); err != nil {
		return IdeaBatch{}, err
	}
	if requested < 2 || requested > 8 || len(candidates) != requested {
		return IdeaBatch{}, errors.New("invalid requested candidate count")
	}
	r := IdeaBatch{RequestDigest: s.RequestDigest, RequestedCount: requested, GenerationPolicyVersion: policy, EffectiveSeed: s.EffectiveSeed, Candidates: append([]IdeaCandidate(nil), candidates...)}
	sort.Slice(r.Candidates, func(i, j int) bool { return r.Candidates[i].CandidateOrdinal < r.Candidates[j].CandidateOrdinal })
	for i := range r.Candidates {
		if r.Candidates[i].CandidateOrdinal != i {
			return IdeaBatch{}, errors.New("candidate ordinals must be contiguous")
		}
		if r.Candidates[i].FeasibilityStatus != "FEASIBLE" && r.Candidates[i].FeasibilityStatus != "REJECTED" {
			return IdeaBatch{}, errors.New("invalid feasibility status")
		}
		r.Candidates[i].IdeaID = ideaID(r.RequestDigest, r.EffectiveSeed, r.BatchOrdinal, i, r.Candidates[i])
		r.Candidates[i].SeedAxes = append([]string(nil), r.Candidates[i].SeedAxes...)
		r.Candidates[i].FeasibilityReasons = append([]string(nil), r.Candidates[i].FeasibilityReasons...)
		r.Candidates[i].NegativeConstraints = append([]string(nil), r.Candidates[i].NegativeConstraints...)
	}
	b, _ := json.Marshal(r)
	r.BatchDigest = SumBytes(b)
	return r, nil
}

func (b IdeaBatch) Validate() error {
	if b.RequestDigest.Validate() != nil || b.RequestedCount != len(b.Candidates) || b.RequestedCount < 2 || b.RequestedCount > 8 {
		return errors.New("invalid idea batch")
	}
	for n, c := range b.Candidates {
		if c.CandidateOrdinal != n || (c.FeasibilityStatus != "FEASIBLE" && c.FeasibilityStatus != "REJECTED") || c.IdeaID != ideaID(b.RequestDigest, b.EffectiveSeed, b.BatchOrdinal, n, c) {
			return errors.New("invalid idea candidate")
		}
	}
	x := b
	x.BatchDigest = ""
	raw, _ := json.Marshal(x)
	if b.BatchDigest != SumBytes(raw) {
		return errors.New("batch digest mismatch")
	}
	return nil
}
func ideaID(rd Digest, seed uint64, batchOrdinal, ordinal int, c IdeaCandidate) string {
	c.IdeaID = ""
	b, _ := json.Marshal(struct {
		R Digest
		S uint64
		B int
		O int
		C IdeaCandidate
	}{rd, seed, batchOrdinal, ordinal, c})
	return "idea:" + string(SumBytes(b))
}
func (b IdeaBatch) FeasibleCandidateIDs() []string {
	out := make([]string, 0)
	for _, c := range b.Candidates {
		if c.FeasibilityStatus == "FEASIBLE" {
			out = append(out, c.IdeaID)
		}
	}
	return out
}

func NewIdeaSelection(rd Digest, b IdeaBatch, id, policy string, reasons []string, evidence []Digest) (IdeaSelection, error) {
	if err := b.Validate(); err != nil {
		return IdeaSelection{}, err
	}
	if policy == "" || len(reasons) == 0 {
		return IdeaSelection{}, errors.New("selection policy/reasons required")
	}
	for _, d := range evidence {
		if err := d.Validate(); err != nil {
			return IdeaSelection{}, err
		}
	}
	if rd != b.RequestDigest {
		return IdeaSelection{}, errors.New("request digest mismatch")
	}
	var found bool
	for _, c := range b.Candidates {
		if c.IdeaID == id {
			found = true
			if c.FeasibilityStatus != "FEASIBLE" {
				return IdeaSelection{}, errors.New("rejected candidate cannot be selected")
			}
		}
	}
	if !found {
		return IdeaSelection{}, errors.New("selected candidate not in batch")
	}
	o := b.FeasibleCandidateIDs()
	if len(o) == 0 || id != o[0] {
		return IdeaSelection{}, errors.New("selected candidate is not the stable policy preference")
	}
	s := IdeaSelection{RequestDigest: rd, IdeaBatchDigest: b.BatchDigest, SelectedIdeaID: id, SelectionPolicyVersion: policy, OrderedCandidateIDs: o, ReasonCodes: append([]string(nil), reasons...), EvidenceDigests: append([]Digest(nil), evidence...)}
	x := s
	x.SelectionDigest = ""
	bb, _ := json.Marshal(x)
	s.SelectionDigest = SumBytes(bb)
	return s, nil
}

func (s IdeaSelection) Validate(b IdeaBatch) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if s.RequestDigest != b.RequestDigest || s.IdeaBatchDigest != b.BatchDigest {
		return errors.New("selection digest chain mismatch")
	}
	feasible := b.FeasibleCandidateIDs()
	if len(feasible) != len(s.OrderedCandidateIDs) {
		return errors.New("selection ordering mismatch")
	}
	for n := range feasible {
		if feasible[n] != s.OrderedCandidateIDs[n] {
			return errors.New("selection ordering mismatch")
		}
	}
	found := false
	for _, id := range feasible {
		if id == s.SelectedIdeaID {
			found = true
		}
	}
	if !found {
		return errors.New("selected idea is not feasible")
	}
	x := s
	x.SelectionDigest = ""
	raw, _ := json.Marshal(x)
	if s.SelectionDigest != SumBytes(raw) {
		return errors.New("selection digest mismatch")
	}
	return nil
}
func (i StatementInput) ValidateChain(s GenerationRequestSnapshotV1, b IdeaBatch, sel IdeaSelection) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if i.RequestSnapshotDigest != s.SnapshotDigest || i.IdeaBatchDigest != b.BatchDigest || i.IdeaSelectionDigest != sel.SelectionDigest || sel.RequestDigest != s.RequestDigest || sel.IdeaBatchDigest != b.BatchDigest || sel.SelectedIdeaID != i.SelectedIdeaID {
		return errors.New("statement input digest chain mismatch")
	}
	if err := sel.Validate(b); err != nil {
		return err
	}
	for _, c := range b.Candidates {
		if c.IdeaID == i.SelectedIdeaID && c.FeasibilityStatus == "FEASIBLE" {
			return nil
		}
	}
	return errors.New("selected idea is not feasible")
}

func strictJSON(raw string, dst any) error {
	if !utf8.ValidString(raw) || hasUnpairedSurrogateEscape(raw) {
		return errors.New("invalid UTF-8")
	}
	if err := duplicateJSONKey([]byte(raw)); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader([]byte(raw)))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func duplicateJSONKey(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := scanJSONValue(d); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("trailing JSON")
		}
		return err
	}
	return nil
}

func scanJSONValue(d *json.Decoder) error {
	t, err := d.Token()
	if err != nil {
		return err
	}
	if delim, ok := t.(json.Delim); ok {
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				key, err := d.Token()
				if err != nil {
					return err
				}
				ks, ok := key.(string)
				if !ok {
					return errors.New("invalid object key")
				}
				if seen[ks] {
					return fmt.Errorf("duplicate JSON field %q", ks)
				}
				seen[ks] = true
				if err := scanJSONValue(d); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		case '[':
			for d.More() {
				if err := scanJSONValue(d); err != nil {
					return err
				}
			}
			_, err = d.Token()
			return err
		}
	}
	return nil
}

func hasUnpairedSurrogateEscape(s string) bool {
	for i := 0; i+5 < len(s); i++ {
		if s[i] != '\\' || s[i+1] != 'u' {
			continue
		}
		h := s[i+2 : i+6]
		if len(h) == 4 && ((h[0] == 'd' || h[0] == 'D') && ((h[1] >= '8' && h[1] <= '9') || (h[1] == 'a' || h[1] == 'A') || (h[1] == 'b' || h[1] == 'B') || (h[1] == 'c' || h[1] == 'C') || (h[1] == 'd' || h[1] == 'D') || (h[1] == 'e' || h[1] == 'E') || (h[1] == 'f' || h[1] == 'F'))) {
			if i+11 >= len(s) || s[i+6:i+8] != "\\u" {
				return true
			}
			n := s[i+8 : i+12]
			if !(n[0] == 'd' || n[0] == 'D') || !((n[1] >= 'c' && n[1] <= 'f') || (n[1] >= 'C' && n[1] <= 'F')) {
				return true
			}
		}
	}
	return false
}
