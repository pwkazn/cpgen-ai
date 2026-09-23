package domain

import (
	"errors"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	SolutionDraftSchemaV1      = "cpgen.solution-draft/v1"
	SolutionDraftInputSchemaV1 = "cpgen.solution-draft-input/v1"
	SolutionContentSchemaV1    = "cpgen.solution-content/v1"
)

// SolutionDraftV1 proposes content only. Compilation, sample checks and Judge
// produce separate evidence; a model cannot assign a verification verdict.
type SolutionDraftV1 struct {
	SchemaVersion string `json:"schema_version"`
	ReferenceCode string `json:"reference_code"`
	BruteCode     string `json:"brute_code"`
	Explanation   string `json:"explanation"`
}

// Similarity digests bind the exact committed acceptance inspected by the
// application. Structural validation is not permission to execute a model.
type SolutionDraftInputV1 struct {
	SchemaVersion            string                      `json:"schema_version"`
	Snapshot                 GenerationRequestSnapshotV1 `json:"snapshot"`
	Problem                  ProblemSpec                 `json:"problem"`
	SimilarityInputDigest    Digest                      `json:"similarity_input_digest"`
	SimilarityEvidenceDigest Digest                      `json:"similarity_evidence_digest"`
	SimilarityDecisionDigest Digest                      `json:"similarity_decision_digest"`
}

// SolutionContent is a bound proposal, not a successfully compiled or judged
// solution. Its exact source bytes can be published through declared artifacts.
type SolutionContent struct {
	SchemaVersion     string `json:"schema_version"`
	InputDigest       Digest `json:"input_digest"`
	ProblemSpecDigest Digest `json:"problem_spec_digest"`
	Language          string `json:"language"`
	ReferenceCode     string `json:"reference_code"`
	BruteCode         string `json:"brute_code"`
	Explanation       string `json:"explanation"`
	ContentDigest     Digest `json:"content_digest"`
}

func NewSolutionDraftInput(snapshot GenerationRequestSnapshotV1, problem ProblemSpec, similarityInput, evidence, decision Digest) (SolutionDraftInputV1, error) {
	input := SolutionDraftInputV1{SolutionDraftInputSchemaV1, snapshot, problem, similarityInput, evidence, decision}
	raw, err := input.CanonicalJSON()
	if err != nil {
		return SolutionDraftInputV1{}, err
	}
	var cloned SolutionDraftInputV1
	if err := cloned.UnmarshalJSON(raw); err != nil {
		return SolutionDraftInputV1{}, err
	}
	return cloned, nil
}

func (v SolutionDraftInputV1) Validate() error {
	if v.SchemaVersion != SolutionDraftInputSchemaV1 {
		return errors.New("unsupported solution draft input schema")
	}
	if err := v.Snapshot.Validate(); err != nil {
		return err
	}
	if err := v.Problem.Validate(); err != nil {
		return err
	}
	for _, digest := range []Digest{v.SimilarityInputDigest, v.SimilarityEvidenceDigest, v.SimilarityDecisionDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	p, s := v.Problem, v.Snapshot
	if p.RequestSnapshotDigest != s.SnapshotDigest || p.RequestDigest != s.RequestDigest || p.Language != cleanText(s.Request.Language) || p.TimeLimitMS != s.Request.TimeLimitMilliseconds || p.MemoryLimitMB != s.Request.MemoryLimitMegabytes || !slices.Equal(p.RequiredConstraints, cleanSet(s.Request.RequiredFeatures)) || !slices.Equal(p.ForbiddenConstraints, cleanSet(s.Request.ForbiddenFeatures)) {
		return errors.New("solution problem differs from frozen request")
	}
	return nil
}

func validateProgramSource(source string) error {
	if len(source) > 262144 || !utf8.ValidString(source) || strings.ContainsRune(source, 0) || strings.TrimSpace(source) == "" {
		return errors.New("program source must be nonempty bounded UTF-8 without NUL")
	}
	return nil
}

func (v SolutionDraftV1) Validate() error {
	if v.SchemaVersion != SolutionDraftSchemaV1 {
		return errors.New("unsupported solution draft schema")
	}
	if err := validateProgramSource(v.ReferenceCode); err != nil {
		return err
	}
	if err := validateProgramSource(v.BruteCode); err != nil {
		return err
	}
	return validateText(32768, false, v.Explanation)
}

// ValidateDistinctSources is the stricter executed-samples policy. Historical
// V1/V2 runs retain SolutionDraftV1.Validate semantics; only the V3 model
// admission path applies this additional source independence check.
func (v SolutionDraftV1) ValidateDistinctSources() error {
	if err := v.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(v.ReferenceCode) == strings.TrimSpace(v.BruteCode) {
		return errors.New("brute must be independently implemented, not the reference source")
	}
	return nil
}

func (v SolutionDraftV1) Bind(input SolutionDraftInputV1) (SolutionContent, error) {
	if err := v.Validate(); err != nil {
		return SolutionContent{}, err
	}
	digest, err := input.Digest()
	if err != nil {
		return SolutionContent{}, err
	}
	content := SolutionContent{SchemaVersion: SolutionContentSchemaV1, InputDigest: digest, ProblemSpecDigest: input.Problem.SpecDigest, Language: input.Snapshot.Request.SolutionLanguage, ReferenceCode: v.ReferenceCode, BruteCode: v.BruteCode, Explanation: v.Explanation}
	content.ContentDigest = contentSum(content)
	return content, content.ValidateInput(input)
}

func (v SolutionContent) Validate() error {
	if v.SchemaVersion != SolutionContentSchemaV1 {
		return errors.New("unsupported solution content schema")
	}
	for _, digest := range []Digest{v.InputDigest, v.ProblemSpecDigest, v.ContentDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	if err := validateText(256, false, v.Language); err != nil {
		return err
	}
	if err := (SolutionDraftV1{SolutionDraftSchemaV1, v.ReferenceCode, v.BruteCode, v.Explanation}).Validate(); err != nil {
		return err
	}
	expected := v.ContentDigest
	v.ContentDigest = ""
	if contentSum(v) != expected {
		return errors.New("solution content digest differs")
	}
	return nil
}

func (v SolutionContent) ValidateInput(input SolutionDraftInputV1) error {
	if err := v.Validate(); err != nil {
		return err
	}
	digest, err := input.Digest()
	if err != nil {
		return err
	}
	if v.InputDigest != digest || v.ProblemSpecDigest != input.Problem.SpecDigest || v.Language != input.Snapshot.Request.SolutionLanguage {
		return errors.New("solution content differs from the verified input")
	}
	return nil
}

func (v SolutionDraftInputV1) CanonicalJSON() ([]byte, error) { return contentJSON(v, v.Validate()) }
func (v SolutionDraftInputV1) Digest() (Digest, error) {
	raw, err := v.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return SumBytes(raw), nil
}
func (v SolutionDraftV1) CanonicalJSON() ([]byte, error) { return contentJSON(v, v.Validate()) }
func (v SolutionContent) CanonicalJSON() ([]byte, error) { return contentJSON(v, v.Validate()) }

func (v *SolutionDraftInputV1) UnmarshalJSON(raw []byte) error {
	type plain SolutionDraftInputV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := SolutionDraftInputV1(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*v = decoded
	return nil
}
func (v *SolutionDraftV1) UnmarshalJSON(raw []byte) error {
	type plain SolutionDraftV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := SolutionDraftV1(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*v = decoded
	return nil
}
func (v *SolutionContent) UnmarshalJSON(raw []byte) error {
	type plain SolutionContent
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := SolutionContent(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*v = decoded
	return nil
}
