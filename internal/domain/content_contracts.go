package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	RequestSchemaV1          = "cpgen.request/v1"
	RequestSnapshotSchemaV1  = "cpgen.request-snapshot/v1"
	IdeaCandidateSchemaV1    = "cpgen.idea-candidate/v1"
	IdeaBatchSchemaV1        = "cpgen.idea-batch/v1"
	IdeaSelectionSchemaV1    = "cpgen.idea-selection/v1"
	StatementInputSchemaV1   = "cpgen.statement-input/v1"
	ProblemSpecSchemaV1      = "cpgen.problem-spec/v1"
	GenerationPolicyV1       = "policy/v1"
	SelectionOrdinalPolicyV1 = "selection/v1"
	SelectionIdeaIDPolicyV1  = "selection.idea-id/v1"
)

// GenerationRequestV1 deliberately has the identical fields and wire representation
// as Slice 1's RunRequest. Conversions cannot silently discard future request fields.
type GenerationRequestV1 RunRequest

func GenerationRequestFromRunRequest(r RunRequest) (GenerationRequestV1, error) {
	n := GenerationRequestV1(r).normalized()
	return n, n.Validate()
}
func (r GenerationRequestV1) ToRunRequest() (RunRequest, error) {
	n := r.normalized()
	return RunRequest(n), n.Validate()
}
func (r GenerationRequestV1) normalized() GenerationRequestV1 {
	r.SchemaVersion = cleanText(r.SchemaVersion)
	r.Mode = cleanText(r.Mode)
	r.Brief = cleanText(r.Brief)
	r.Language = cleanText(r.Language)
	r.Difficulty = cleanText(r.Difficulty)
	r.SolutionLanguage = cleanText(r.SolutionLanguage)
	r.VerificationProfile = cleanText(r.VerificationProfile)
	r.Tags = cleanSet(r.Tags)
	r.NormalizedTags = cleanSet(r.NormalizedTags)
	r.RequiredFeatures = cleanSet(r.RequiredFeatures)
	r.ForbiddenFeatures = cleanSet(r.ForbiddenFeatures)
	r.ExportTargets = cleanSet(r.ExportTargets)
	if r.Seed != nil {
		seed := *r.Seed
		r.Seed = &seed
	}
	return r
}
func (r GenerationRequestV1) Validate() error {
	if r.SchemaVersion != RequestSchemaV1 {
		return errors.New("unsupported request schema")
	}
	if err := RunRequest(r).Validate(); err != nil {
		return err
	}
	if err := validateText(32768, false, r.Mode, r.Brief, r.Language, r.Difficulty, r.SolutionLanguage, r.VerificationProfile); err != nil {
		return err
	}
	for _, set := range [][]string{r.Tags, r.NormalizedTags, r.RequiredFeatures, r.ForbiddenFeatures, r.ExportTargets} {
		if err := validateSet(set); err != nil {
			return err
		}
	}
	return disjoint(r.RequiredFeatures, r.ForbiddenFeatures)
}
func (r *GenerationRequestV1) UnmarshalJSON(raw []byte) error {
	type plain GenerationRequestV1
	var v plain
	if err := strictJSON(string(raw), &v); err != nil {
		return err
	}
	n := GenerationRequestV1(v).normalized()
	if err := n.Validate(); err != nil {
		return err
	}
	*r = n
	return nil
}
func (r GenerationRequestV1) CanonicalJSON() ([]byte, error) {
	n := r.normalized()
	return contentJSON(n, n.Validate())
}
func (r GenerationRequestV1) Digest() (Digest, error) {
	raw, err := r.CanonicalJSON()
	return sumResult(raw, err)
}

type GenerationRequestSnapshotV1 struct {
	SchemaVersion  string              `json:"schema_version"`
	Request        GenerationRequestV1 `json:"request"`
	EffectiveSeed  int64               `json:"effective_seed"`
	RequestDigest  Digest              `json:"request_digest"`
	SnapshotDigest Digest              `json:"snapshot_digest"`
}

func NewGenerationRequestSnapshotV1(r GenerationRequestV1, seed int64) (GenerationRequestSnapshotV1, error) {
	r = r.normalized()
	rd, err := r.Digest()
	if err != nil {
		return GenerationRequestSnapshotV1{}, err
	}
	s := GenerationRequestSnapshotV1{SchemaVersion: RequestSnapshotSchemaV1, Request: r, EffectiveSeed: seed, RequestDigest: rd}
	s.SnapshotDigest = contentSum(s)
	return s, s.Validate()
}
func (s GenerationRequestSnapshotV1) Validate() error {
	if s.SchemaVersion != RequestSnapshotSchemaV1 {
		return errors.New("unsupported snapshot schema")
	}
	if err := s.Request.Validate(); err != nil {
		return err
	}
	if s.Request.Seed != nil && *s.Request.Seed != s.EffectiveSeed {
		return errors.New("effective seed differs from explicit seed")
	}
	rd, err := s.Request.Digest()
	if err != nil {
		return err
	}
	if s.RequestDigest != rd {
		return errors.New("request digest mismatch")
	}
	x := s
	x.SnapshotDigest = ""
	return matchContentDigest(s.SnapshotDigest, x)
}
func (s *GenerationRequestSnapshotV1) UnmarshalJSON(raw []byte) error {
	type plain GenerationRequestSnapshotV1
	var v plain
	if err := strictJSON(string(raw), &v); err != nil {
		return err
	}
	n := GenerationRequestSnapshotV1(v)
	if err := n.Validate(); err != nil {
		return err
	}
	*s = n
	return nil
}
func (s GenerationRequestSnapshotV1) CanonicalJSON() ([]byte, error) {
	return contentJSON(s, s.Validate())
}
func (s GenerationRequestSnapshotV1) Digest() (Digest, error) { return s.SnapshotDigest, s.Validate() }

type IdeaCandidate struct {
	SchemaVersion       string   `json:"schema_version"`
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

func (c IdeaCandidate) normalized() IdeaCandidate {
	c.SchemaVersion = cleanText(c.SchemaVersion)
	c.IdeaID = cleanText(c.IdeaID)
	c.ParentIdeaID = cleanText(c.ParentIdeaID)
	c.AbstractTask = cleanText(c.AbstractTask)
	c.IntendedAlgorithm = cleanText(c.IntendedAlgorithm)
	c.TargetComplexity = cleanText(c.TargetComplexity)
	c.FeasibilityStatus = cleanText(c.FeasibilityStatus)
	c.MutationReason = cleanText(c.MutationReason)
	c.SeedAxes = cleanSet(c.SeedAxes)
	c.FeasibilityReasons = cleanSet(c.FeasibilityReasons)
	c.NegativeConstraints = cleanSet(c.NegativeConstraints)
	return c
}
func (c IdeaCandidate) Validate() error {
	if c.SchemaVersion != IdeaCandidateSchemaV1 || c.CandidateOrdinal < 0 || c.MutationOrdinal < 0 {
		return errors.New("invalid candidate schema or ordinal")
	}
	if err := validIdeaID(c.IdeaID); err != nil {
		return err
	}
	if err := validateText(32768, false, c.AbstractTask, c.IntendedAlgorithm, c.TargetComplexity); err != nil {
		return err
	}
	if len(c.SeedAxes) == 0 {
		return errors.New("candidate seed axes required")
	}
	for _, set := range [][]string{c.SeedAxes, c.FeasibilityReasons, c.NegativeConstraints} {
		if err := validateSet(set); err != nil {
			return err
		}
	}
	if c.FeasibilityStatus != "FEASIBLE" && c.FeasibilityStatus != "REJECTED" {
		return errors.New("invalid feasibility status")
	}
	if c.FeasibilityStatus == "REJECTED" && len(c.FeasibilityReasons) == 0 {
		return errors.New("rejected candidate requires reasons")
	}
	if c.MutationOrdinal == 0 {
		if c.ParentIdeaID != "" || c.MutationReason != "" {
			return errors.New("lineage requires mutation ordinal")
		}
	} else {
		if err := validateText(4096, false, c.MutationReason); err != nil {
			return err
		}
	}
	if c.ParentIdeaID != "" {
		if err := validIdeaID(c.ParentIdeaID); err != nil {
			return err
		}
		if c.ParentIdeaID == c.IdeaID {
			return errors.New("candidate cannot parent itself")
		}
	}
	return nil
}
func (c *IdeaCandidate) UnmarshalJSON(raw []byte) error {
	type plain IdeaCandidate
	var v plain
	if err := strictJSON(string(raw), &v); err != nil {
		return err
	}
	n := IdeaCandidate(v)
	if err := n.Validate(); err != nil {
		return err
	}
	*c = n
	return nil
}
func (c IdeaCandidate) CanonicalJSON() ([]byte, error) { return contentJSON(c, c.Validate()) }
func (c IdeaCandidate) Digest() (Digest, error) {
	raw, err := c.CanonicalJSON()
	return sumResult(raw, err)
}

type IdeaBatch struct {
	SchemaVersion           string          `json:"schema_version"`
	RequestDigest           Digest          `json:"request_digest"`
	RequestedCount          int             `json:"requested_count"`
	Candidates              []IdeaCandidate `json:"candidates"`
	GenerationPolicyVersion string          `json:"generation_policy_version"`
	EffectiveSeed           int64           `json:"effective_seed"`
	BatchOrdinal            int             `json:"batch_ordinal"`
	BatchDigest             Digest          `json:"batch_digest"`
}

// The optional batch ordinal keeps the initial-batch call concise; at most one is accepted.
func NewIdeaBatch(s GenerationRequestSnapshotV1, requested int, policy string, candidates []IdeaCandidate, ordinal ...int) (IdeaBatch, error) {
	if err := s.Validate(); err != nil {
		return IdeaBatch{}, err
	}
	if len(ordinal) > 1 {
		return IdeaBatch{}, errors.New("at most one batch ordinal")
	}
	b := IdeaBatch{SchemaVersion: IdeaBatchSchemaV1, RequestDigest: s.RequestDigest, RequestedCount: requested, GenerationPolicyVersion: cleanText(policy), EffectiveSeed: s.EffectiveSeed, Candidates: make([]IdeaCandidate, len(candidates))}
	if len(ordinal) == 1 {
		b.BatchOrdinal = ordinal[0]
	}
	for i, c := range candidates {
		c = c.normalized()
		if c.SchemaVersion == "" {
			c.SchemaVersion = IdeaCandidateSchemaV1
		}
		b.Candidates[i] = c
	}
	sort.Slice(b.Candidates, func(i, j int) bool { return b.Candidates[i].CandidateOrdinal < b.Candidates[j].CandidateOrdinal })
	for i := range b.Candidates {
		b.Candidates[i].IdeaID = ideaID(b, b.Candidates[i])
	}
	b.BatchDigest = contentSum(b)
	return b, b.Validate()
}
func ideaID(b IdeaBatch, c IdeaCandidate) string {
	c.IdeaID = ""
	return "idea:" + string(contentSum(struct {
		RequestDigest           Digest        `json:"request_digest"`
		EffectiveSeed           int64         `json:"effective_seed"`
		BatchOrdinal            int           `json:"batch_ordinal"`
		GenerationPolicyVersion string        `json:"generation_policy_version"`
		Candidate               IdeaCandidate `json:"candidate"`
	}{b.RequestDigest, b.EffectiveSeed, b.BatchOrdinal, b.GenerationPolicyVersion, c}))
}
func (b IdeaBatch) Validate() error {
	if b.SchemaVersion != IdeaBatchSchemaV1 || b.RequestDigest.Validate() != nil || b.RequestedCount < 2 || b.RequestedCount > 8 || b.RequestedCount != len(b.Candidates) || b.BatchOrdinal < 0 {
		return errors.New("invalid idea batch")
	}
	if b.GenerationPolicyVersion != GenerationPolicyV1 {
		return errors.New("unsupported generation policy")
	}
	ids := map[string]bool{}
	lineages := map[string]bool{}
	for i, c := range b.Candidates {
		if err := c.Validate(); err != nil {
			return err
		}
		if c.CandidateOrdinal != i || c.IdeaID != ideaID(b, c) {
			return errors.New("candidate identity or ordinal mismatch")
		}
		ids[c.IdeaID] = true
		if c.MutationOrdinal > 0 {
			if b.BatchOrdinal == 0 {
				return errors.New("initial batch cannot have mutation lineage")
			}
			// A mutation ordinal is unique per parent (or per request when parentless).
			key := fmt.Sprintf("%s/%d", c.ParentIdeaID, c.MutationOrdinal)
			if lineages[key] {
				return errors.New("duplicate mutation lineage")
			}
			lineages[key] = true
		}
	}
	for _, c := range b.Candidates {
		if ids[c.ParentIdeaID] {
			return errors.New("mutation parent must come from an earlier batch")
		}
	}
	x := b
	x.BatchDigest = ""
	return matchContentDigest(b.BatchDigest, x)
}
func (b *IdeaBatch) UnmarshalJSON(raw []byte) error {
	type plain IdeaBatch
	var v plain
	if err := strictJSON(string(raw), &v); err != nil {
		return err
	}
	n := IdeaBatch(v)
	if err := n.Validate(); err != nil {
		return err
	}
	*b = n
	return nil
}
func (b IdeaBatch) CanonicalJSON() ([]byte, error) { return contentJSON(b, b.Validate()) }
func (b IdeaBatch) Digest() (Digest, error)        { return b.BatchDigest, b.Validate() }
func (b IdeaBatch) FeasibleCandidateIDs() []string {
	ids := make([]string, 0, len(b.Candidates))
	for _, c := range b.Candidates {
		if c.FeasibilityStatus == "FEASIBLE" {
			ids = append(ids, c.IdeaID)
		}
	}
	return ids
}

// Versioned policies are explicit algorithms; unknown versions never silently fall back.
func (b IdeaBatch) OrderedFeasibleCandidateIDs(policy string) ([]string, error) {
	if err := b.Validate(); err != nil {
		return nil, err
	}
	ids := b.FeasibleCandidateIDs()
	switch policy {
	case SelectionOrdinalPolicyV1:
	case SelectionIdeaIDPolicyV1:
		sort.Strings(ids)
	default:
		return nil, errors.New("unsupported selection policy")
	}
	return ids, nil
}

type IdeaSelection struct {
	SchemaVersion          string   `json:"schema_version"`
	RequestDigest          Digest   `json:"request_digest"`
	IdeaBatchDigest        Digest   `json:"idea_batch_digest"`
	SelectedIdeaID         string   `json:"selected_idea_id"`
	SelectionPolicyVersion string   `json:"selection_policy_version"`
	OrderedCandidateIDs    []string `json:"ordered_candidate_ids"`
	ReasonCodes            []string `json:"reason_codes"`
	EvidenceDigests        []Digest `json:"evidence_digests"`
	SelectionDigest        Digest   `json:"selection_digest"`
}

func NewIdeaSelection(rd Digest, b IdeaBatch, id, policy string, reasons []string, evidence []Digest) (IdeaSelection, error) {
	policy = cleanText(policy)
	ids, err := b.OrderedFeasibleCandidateIDs(policy)
	if err != nil {
		return IdeaSelection{}, err
	}
	ev := append([]Digest{}, evidence...)
	sort.Slice(ev, func(i, j int) bool { return ev[i] < ev[j] })
	ev = slices.Compact(ev)
	s := IdeaSelection{SchemaVersion: IdeaSelectionSchemaV1, RequestDigest: rd, IdeaBatchDigest: b.BatchDigest, SelectedIdeaID: cleanText(id), SelectionPolicyVersion: policy, OrderedCandidateIDs: ids, ReasonCodes: cleanSet(reasons), EvidenceDigests: ev}
	s.SelectionDigest = contentSum(s)
	return s, s.Validate(b)
}

// Without a batch Validate checks the standalone envelope. Passing the batch is
// required at a trust boundary to establish membership and policy ordering.
func (s IdeaSelection) Validate(batches ...IdeaBatch) error {
	if len(batches) > 1 {
		return errors.New("at most one idea batch")
	}
	if s.SchemaVersion != IdeaSelectionSchemaV1 || s.RequestDigest.Validate() != nil || s.IdeaBatchDigest.Validate() != nil {
		return errors.New("invalid selection schema or digests")
	}
	if s.SelectionPolicyVersion != SelectionOrdinalPolicyV1 && s.SelectionPolicyVersion != SelectionIdeaIDPolicyV1 {
		return errors.New("unsupported selection policy")
	}
	if len(s.OrderedCandidateIDs) == 0 || s.SelectedIdeaID != s.OrderedCandidateIDs[0] {
		return errors.New("selected candidate is not the stable policy preference")
	}
	if err := uniqueNonEmpty("candidate ID", s.OrderedCandidateIDs); err != nil {
		return err
	}
	for _, id := range s.OrderedCandidateIDs {
		if err := validIdeaID(id); err != nil {
			return err
		}
	}
	if s.SelectionPolicyVersion == SelectionIdeaIDPolicyV1 && !slices.IsSorted(s.OrderedCandidateIDs) {
		return errors.New("invalid ID policy ordering")
	}
	if len(s.ReasonCodes) == 0 {
		return errors.New("selection reasons required")
	}
	if s.EvidenceDigests == nil {
		return errors.New("canonical evidence must be an array")
	}
	if err := validateSet(s.ReasonCodes); err != nil {
		return err
	}
	for i, d := range s.EvidenceDigests {
		if d.Validate() != nil || (i > 0 && s.EvidenceDigests[i-1] >= d) {
			return errors.New("invalid evidence digest set")
		}
	}
	if len(batches) == 1 {
		b := batches[0]
		ids, err := b.OrderedFeasibleCandidateIDs(s.SelectionPolicyVersion)
		if err != nil {
			return err
		}
		if s.RequestDigest != b.RequestDigest || s.IdeaBatchDigest != b.BatchDigest || !slices.Equal(ids, s.OrderedCandidateIDs) {
			return errors.New("selection batch chain or ordering mismatch")
		}
	}
	x := s
	x.SelectionDigest = ""
	return matchContentDigest(s.SelectionDigest, x)
}
func (s *IdeaSelection) UnmarshalJSON(raw []byte) error {
	type plain IdeaSelection
	var v plain
	if err := strictJSON(string(raw), &v); err != nil {
		return err
	}
	n := IdeaSelection(v)
	if err := n.Validate(); err != nil {
		return err
	}
	*s = n
	return nil
}
func (s IdeaSelection) CanonicalJSON() ([]byte, error) { return contentJSON(s, s.Validate()) }
func (s IdeaSelection) Digest() (Digest, error)        { return s.SelectionDigest, s.Validate() }

type StatementInput struct {
	SchemaVersion         string `json:"schema_version"`
	RequestSnapshotDigest Digest `json:"request_snapshot_digest"`
	IdeaBatchDigest       Digest `json:"idea_batch_digest"`
	IdeaSelectionDigest   Digest `json:"idea_selection_digest"`
	SelectedIdeaID        string `json:"selected_idea_id"`
}

func (i StatementInput) Validate() error {
	if i.SchemaVersion != StatementInputSchemaV1 {
		return errors.New("unsupported statement input schema")
	}
	for _, d := range []Digest{i.RequestSnapshotDigest, i.IdeaBatchDigest, i.IdeaSelectionDigest} {
		if err := d.Validate(); err != nil {
			return err
		}
	}
	return validIdeaID(i.SelectedIdeaID)
}
func (i StatementInput) ValidateChain(s GenerationRequestSnapshotV1, b IdeaBatch, sel IdeaSelection) error {
	if err := i.Validate(); err != nil {
		return err
	}
	if err := s.Validate(); err != nil {
		return err
	}
	if err := sel.Validate(b); err != nil {
		return err
	}
	if i.RequestSnapshotDigest != s.SnapshotDigest || i.IdeaBatchDigest != b.BatchDigest || i.IdeaSelectionDigest != sel.SelectionDigest || i.SelectedIdeaID != sel.SelectedIdeaID || b.RequestDigest != s.RequestDigest || b.EffectiveSeed != s.EffectiveSeed {
		return errors.New("statement input digest chain mismatch")
	}
	return nil
}
func (i *StatementInput) UnmarshalJSON(raw []byte) error {
	type plain StatementInput
	var v plain
	if err := strictJSON(string(raw), &v); err != nil {
		return err
	}
	n := StatementInput(v)
	if err := n.Validate(); err != nil {
		return err
	}
	*i = n
	return nil
}
func (i StatementInput) CanonicalJSON() ([]byte, error) { return contentJSON(i, i.Validate()) }
func (i StatementInput) Digest() (Digest, error) {
	raw, err := i.CanonicalJSON()
	return sumResult(raw, err)
}

// Field and sample order is semantic and is preserved. Only set-valued fields
// (tags, reasons, constraints, evidence) are sorted and deduplicated.
type ProblemIO struct {
	Description string   `json:"description"`
	Fields      []string `json:"fields"`
}
type ProblemSample struct {
	Input       string `json:"input"`
	Output      string `json:"output"`
	Explanation string `json:"explanation,omitempty"`
}
type ProblemSpec struct {
	SchemaVersion          string          `json:"schema_version"`
	Revision               int64           `json:"revision"`
	RequestSnapshotDigest  Digest          `json:"request_snapshot_digest"`
	RequestDigest          Digest          `json:"request_digest"`
	IdeaBatchDigest        Digest          `json:"idea_batch_digest"`
	IdeaSelectionDigest    Digest          `json:"idea_selection_digest"`
	SelectedIdeaID         string          `json:"selected_idea_id"`
	SelectionPolicyVersion string          `json:"selection_policy_version"`
	Language               string          `json:"language"`
	TimeLimitMS            int64           `json:"time_limit_ms"`
	MemoryLimitMB          int64           `json:"memory_limit_mb"`
	RequiredConstraints    []string        `json:"required_constraints"`
	ForbiddenConstraints   []string        `json:"forbidden_constraints"`
	Title                  string          `json:"title"`
	Description            string          `json:"description"`
	Input                  ProblemIO       `json:"input"`
	Output                 ProblemIO       `json:"output"`
	Samples                []ProblemSample `json:"samples"`
	IntendedAlgorithm      string          `json:"intended_algorithm"`
	TargetComplexity       string          `json:"target_complexity"`
	SpecDigest             Digest          `json:"spec_digest"`
}

func NewProblemSpec(in StatementInput, s GenerationRequestSnapshotV1, b IdeaBatch, sel IdeaSelection, p ProblemSpec) (ProblemSpec, error) {
	if err := in.ValidateChain(s, b, sel); err != nil {
		return ProblemSpec{}, err
	}
	p.SchemaVersion = ProblemSpecSchemaV1
	p.RequestSnapshotDigest = s.SnapshotDigest
	p.RequestDigest = s.RequestDigest
	p.IdeaBatchDigest = b.BatchDigest
	p.IdeaSelectionDigest = sel.SelectionDigest
	p.SelectedIdeaID = sel.SelectedIdeaID
	p.SelectionPolicyVersion = sel.SelectionPolicyVersion
	p.Language = s.Request.Language
	p.TimeLimitMS = s.Request.TimeLimitMilliseconds
	p.MemoryLimitMB = s.Request.MemoryLimitMegabytes
	p.RequiredConstraints = cleanSet(s.Request.RequiredFeatures)
	p.ForbiddenConstraints = cleanSet(s.Request.ForbiddenFeatures)
	for _, c := range b.Candidates {
		if c.IdeaID == sel.SelectedIdeaID {
			p.IntendedAlgorithm = c.IntendedAlgorithm
			p.TargetComplexity = c.TargetComplexity
		}
	}
	p.Title = cleanText(p.Title)
	p.Description = cleanText(p.Description)
	p.Input = cleanIO(p.Input)
	p.Output = cleanIO(p.Output)
	p.Samples = append([]ProblemSample{}, p.Samples...)
	for i := range p.Samples {
		p.Samples[i].Input = cleanText(p.Samples[i].Input)
		p.Samples[i].Output = cleanText(p.Samples[i].Output)
		p.Samples[i].Explanation = cleanText(p.Samples[i].Explanation)
	}
	p.SpecDigest = ""
	p.SpecDigest = contentSum(p)
	return p, p.ValidateChain(s, b, sel)
}
func (p ProblemSpec) Validate() error {
	if p.SchemaVersion != ProblemSpecSchemaV1 || p.Revision <= 0 || p.TimeLimitMS <= 0 || p.MemoryLimitMB <= 0 {
		return errors.New("invalid problem spec schema, revision or limits")
	}
	for _, d := range []Digest{p.RequestSnapshotDigest, p.RequestDigest, p.IdeaBatchDigest, p.IdeaSelectionDigest} {
		if err := d.Validate(); err != nil {
			return err
		}
	}
	if err := validIdeaID(p.SelectedIdeaID); err != nil {
		return err
	}
	if p.SelectionPolicyVersion != SelectionOrdinalPolicyV1 && p.SelectionPolicyVersion != SelectionIdeaIDPolicyV1 {
		return errors.New("unsupported selection policy")
	}
	if err := validateText(32768, false, p.Language, p.Title, p.Description, p.IntendedAlgorithm, p.TargetComplexity); err != nil {
		return err
	}
	for _, set := range [][]string{p.RequiredConstraints, p.ForbiddenConstraints} {
		if err := validateSet(set); err != nil {
			return err
		}
	}
	if err := disjoint(p.RequiredConstraints, p.ForbiddenConstraints); err != nil {
		return err
	}
	for _, v := range []ProblemIO{p.Input, p.Output} {
		if err := validateText(32768, false, v.Description); err != nil {
			return err
		}
		if len(v.Fields) == 0 {
			return errors.New("structured input/output fields required")
		}
		if err := validateText(4096, false, v.Fields...); err != nil {
			return err
		}
		if err := uniqueNonEmpty("IO field", v.Fields); err != nil {
			return err
		}
	}
	if len(p.Samples) == 0 || len(p.Samples) > 32 {
		return errors.New("problem spec requires 1-32 samples")
	}
	for _, sample := range p.Samples {
		if err := validateText(65536, false, sample.Input, sample.Output); err != nil {
			return err
		}
		if err := validateText(32768, true, sample.Explanation); err != nil {
			return err
		}
	}
	x := p
	x.SpecDigest = ""
	return matchContentDigest(p.SpecDigest, x)
}
func (p ProblemSpec) ValidateChain(s GenerationRequestSnapshotV1, b IdeaBatch, sel IdeaSelection) error {
	if err := p.Validate(); err != nil {
		return err
	}
	in := StatementInput{StatementInputSchemaV1, p.RequestSnapshotDigest, p.IdeaBatchDigest, p.IdeaSelectionDigest, p.SelectedIdeaID}
	if err := in.ValidateChain(s, b, sel); err != nil {
		return err
	}
	if p.RequestDigest != s.RequestDigest || p.SelectionPolicyVersion != sel.SelectionPolicyVersion || p.Language != s.Request.Language || p.TimeLimitMS != s.Request.TimeLimitMilliseconds || p.MemoryLimitMB != s.Request.MemoryLimitMegabytes || !slices.Equal(p.RequiredConstraints, s.Request.RequiredFeatures) || !slices.Equal(p.ForbiddenConstraints, s.Request.ForbiddenFeatures) {
		return errors.New("problem spec differs from frozen request or selection")
	}
	for _, c := range b.Candidates {
		if c.IdeaID == p.SelectedIdeaID {
			if p.IntendedAlgorithm != c.IntendedAlgorithm || p.TargetComplexity != c.TargetComplexity {
				return errors.New("problem spec differs from selected algorithm")
			}
			return nil
		}
	}
	return errors.New("selected idea missing")
}
func (p *ProblemSpec) UnmarshalJSON(raw []byte) error {
	type plain ProblemSpec
	var v plain
	if err := strictJSON(string(raw), &v); err != nil {
		return err
	}
	n := ProblemSpec(v)
	if err := n.Validate(); err != nil {
		return err
	}
	*p = n
	return nil
}
func (p ProblemSpec) CanonicalJSON() ([]byte, error) { return contentJSON(p, p.Validate()) }
func (p ProblemSpec) Digest() (Digest, error)        { return p.SpecDigest, p.Validate() }

func cleanText(s string) string {
	// Preserve invalid UTF-8 for validation; normalization must not repair it.
	if !utf8.ValidString(s) {
		return s
	}
	return norm.NFC.String(strings.TrimSpace(s))
}
func cleanSet(v []string) []string {
	out := make([]string, len(v))
	for i, s := range v {
		out[i] = cleanText(s)
	}
	sort.Strings(out)
	return slices.Compact(out)
}
func cleanIO(v ProblemIO) ProblemIO {
	v.Description = cleanText(v.Description)
	fields := make([]string, len(v.Fields))
	for i, s := range v.Fields {
		fields[i] = cleanText(s)
	}
	v.Fields = fields
	return v
}
func validateText(max int, empty bool, values ...string) error {
	for _, s := range values {
		if !utf8.ValidString(s) || len(s) > max || s != cleanText(s) || (!empty && s == "") {
			return errors.New("invalid, noncanonical or oversized text")
		}
	}
	return nil
}
func validateSet(v []string) error {
	if v == nil {
		return errors.New("canonical sets must use an empty array, not null")
	}
	if err := validateText(4096, false, v...); err != nil {
		return err
	}
	if len(v) > 256 {
		return errors.New("too many set members")
	}
	for i := 1; i < len(v); i++ {
		if v[i-1] >= v[i] {
			return errors.New("set must be sorted and unique")
		}
	}
	return nil
}
func disjoint(a, b []string) error {
	for _, v := range a {
		if slices.Contains(b, v) {
			return errors.New("required/forbidden constraint conflict")
		}
	}
	return nil
}
func validIdeaID(id string) error {
	if !strings.HasPrefix(id, "idea:") {
		return errors.New("invalid idea ID")
	}
	return Digest(strings.TrimPrefix(id, "idea:")).Validate()
}
func contentJSON(v any, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	// Match Slice 1's sorted-object canonical JSON without rounding int64 seeds
	// or budgets through a float64 intermediary.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}
func sumResult(raw []byte, err error) (Digest, error) {
	if err != nil {
		return "", err
	}
	return SumBytes(raw), nil
}

// Self-bound digests hash the canonical envelope with their own digest field empty.
// JSON serialization here cannot fail: these private callers contain no floats,
// interfaces, maps, cycles, or custom marshalers.
func contentSum(v any) Digest {
	raw, err := contentJSON(v, nil)
	if err != nil {
		panic(err)
	}
	return SumBytes(raw)
}
func matchContentDigest(d Digest, v any) error {
	if err := d.Validate(); err != nil {
		return err
	}
	if d != contentSum(v) {
		return errors.New("content digest mismatch")
	}
	return nil
}

func strictJSON(raw string, dst any) error {
	if !utf8.ValidString(raw) || hasUnpairedSurrogateEscape(raw) {
		return errors.New("invalid UTF-8 or unpaired surrogate")
	}
	if err := duplicateJSONKey([]byte(raw)); err != nil {
		return err
	}
	// encoding/json otherwise accepts case-insensitive field aliases, which can
	// overwrite an earlier exact key. Check exact declared names at every level.
	if err := exactJSONFields([]byte(raw), reflect.TypeOf(dst)); err != nil {
		return err
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func exactJSONFields(raw []byte, t reflect.Type) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		if t.Kind() == reflect.Struct {
			return errors.New("null contract object")
		}
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		allowed := map[string]reflect.Type{}
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if name == "" {
				name = f.Name
			}
			if name != "-" {
				allowed[name] = f.Type
			}
		}
		for k, v := range fields {
			ft, ok := allowed[k]
			if !ok {
				return fmt.Errorf("unknown JSON field %q", k)
			}
			if err := exactJSONFields(v, ft); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, v := range values {
			if err := exactJSONFields(v, t.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
func duplicateJSONKey(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := scanJSONValue(d); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON")
	}
	return nil
}
func scanJSONValue(d *json.Decoder) error {
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			token, err := d.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("invalid JSON key")
			}
			if seen[key] {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = true
			if err := scanJSONValue(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := scanJSONValue(d); err != nil {
				return err
			}
		}
	default:
		return errors.New("invalid JSON delimiter")
	}
	_, err = d.Token()
	return err
}

// Consume escapes as lexical units: an escaped backslash is not a Unicode
// escape, and a valid high/low pair must advance past both code units.
func hasUnpairedSurrogateEscape(s string) bool {
	inString := false
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			inString = !inString
			continue
		}
		if !inString || s[i] != '\\' {
			continue
		}
		i++
		if i >= len(s) {
			return false
		}
		if s[i] != 'u' {
			continue
		}
		if i+4 >= len(s) {
			return false
		}
		code, err := strconv.ParseUint(s[i+1:i+5], 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if code >= 0xdc00 && code <= 0xdfff {
			return true
		}
		if code < 0xd800 || code > 0xdbff {
			continue
		}
		if i+6 >= len(s) || s[i+1] != '\\' || s[i+2] != 'u' {
			return true
		}
		low, err := strconv.ParseUint(s[i+3:i+7], 16, 16)
		if err != nil || low < 0xdc00 || low > 0xdfff {
			return true
		}
		i += 6
	}
	return false
}
