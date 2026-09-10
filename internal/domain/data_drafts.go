package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
)

const (
	DataDraftSchemaV1      = "cpgen.data-draft/v1"
	DataDraftInputSchemaV1 = "cpgen.data-draft-input/v1"
	DataContentSchemaV1    = "cpgen.data-content/v1"
	TestPlanSchemaV1       = "cpgen.test-plan/v1"
)

type DataCaseKind string

const (
	DataCaseSmall    DataCaseKind = "small"
	DataCaseBoundary DataCaseKind = "boundary"
	DataCaseStress   DataCaseKind = "stress"
)

// A model proposes bounded cases and programs. The host owns case order,
// seeds, argv, paths, resource limits and every execution/validation verdict.
// Samples are taken from the committed ProblemSpec, never this proposal.
type DataCaseDraft struct {
	Kind    DataCaseKind `json:"kind"`
	Purpose string       `json:"purpose"`
}

type DataDraftV1 struct {
	SchemaVersion string          `json:"schema_version"`
	GeneratorCode string          `json:"generator_code"`
	ValidatorCode string          `json:"validator_code"`
	Cases         []DataCaseDraft `json:"cases"`
}

// Structural validity is not proof that verification passed. The application
// must reconstruct this input from its current committed Solution report.
type DataDraftInputV1 struct {
	SchemaVersion              string               `json:"schema_version"`
	SolutionInput              SolutionDraftInputV1 `json:"solution_input"`
	Solution                   SolutionContent      `json:"solution"`
	SolutionVerificationDigest Digest               `json:"solution_verification_digest"`
}

type DataCase struct {
	Ordinal int          `json:"ordinal"`
	Kind    DataCaseKind `json:"kind"`
	Purpose string       `json:"purpose"`
	Seed    uint64       `json:"seed"`
}

type TestPlanV1 struct {
	SchemaVersion string     `json:"schema_version"`
	EffectiveSeed int64      `json:"effective_seed"`
	Cases         []DataCase `json:"cases"`
}

// DataContent is a reproducible proposal, not validated test data. Coverage
// descriptions are claims until the following Docker and Judge stages pass.
type DataContent struct {
	SchemaVersion              string     `json:"schema_version"`
	InputDigest                Digest     `json:"input_digest"`
	ProblemSpecDigest          Digest     `json:"problem_spec_digest"`
	SolutionContentDigest      Digest     `json:"solution_content_digest"`
	SolutionVerificationDigest Digest     `json:"solution_verification_digest"`
	Language                   string     `json:"language"`
	GeneratorCode              string     `json:"generator_code"`
	ValidatorCode              string     `json:"validator_code"`
	Plan                       TestPlanV1 `json:"plan"`
	ContentDigest              Digest     `json:"content_digest"`
}

func NewDataDraftInput(input SolutionDraftInputV1, solution SolutionContent, verification Digest) (DataDraftInputV1, error) {
	value := DataDraftInputV1{DataDraftInputSchemaV1, input, solution, verification}
	raw, err := value.CanonicalJSON()
	if err != nil {
		return DataDraftInputV1{}, err
	}
	var cloned DataDraftInputV1
	if err := cloned.UnmarshalJSON(raw); err != nil {
		return DataDraftInputV1{}, err
	}
	return cloned, nil
}

func (v DataDraftInputV1) Validate() error {
	if v.SchemaVersion != DataDraftInputSchemaV1 {
		return errors.New("unsupported data draft input schema")
	}
	if err := v.Solution.ValidateInput(v.SolutionInput); err != nil {
		return err
	}
	return v.SolutionVerificationDigest.Validate()
}

func validateDataCaseDrafts(cases []DataCaseDraft) error {
	if len(cases) < 4 || len(cases) > 12 {
		return errors.New("test plan requires 4 to 12 generated cases")
	}
	counts, seen := make(map[DataCaseKind]int), make(map[string]bool)
	for _, item := range cases {
		if item.Kind != DataCaseSmall && item.Kind != DataCaseBoundary && item.Kind != DataCaseStress {
			return errors.New("unsupported generated case kind")
		}
		if err := validateText(4096, false, item.Purpose); err != nil {
			return err
		}
		if seen[item.Purpose] {
			return errors.New("generated cases must describe distinct purposes")
		}
		seen[item.Purpose], counts[item.Kind] = true, counts[item.Kind]+1
	}
	if counts[DataCaseSmall] < 2 || counts[DataCaseBoundary] < 1 || counts[DataCaseStress] < 1 {
		return errors.New("test plan requires two small cases, a boundary case and a stress case")
	}
	return nil
}

func (v DataDraftV1) Validate() error {
	if v.SchemaVersion != DataDraftSchemaV1 {
		return errors.New("unsupported data draft schema")
	}
	if err := validateProgramSource(v.GeneratorCode); err != nil {
		return err
	}
	if err := validateProgramSource(v.ValidatorCode); err != nil {
		return err
	}
	return validateDataCaseDrafts(v.Cases)
}

// Preserve all 64 seed bits and derive each case independently of process
// order, time and platform. Generator argv uses the unsigned decimal value.
func dataCaseSeed(seed int64, ordinal int) uint64 {
	var values [16]byte
	binary.BigEndian.PutUint64(values[:8], uint64(seed))
	binary.BigEndian.PutUint64(values[8:], uint64(ordinal))
	sum := sha256.Sum256(append([]byte("cpgen.test-case-seed/v1\x00"), values[:]...))
	return binary.BigEndian.Uint64(sum[:8])
}

func (v TestPlanV1) Validate() error {
	if v.SchemaVersion != TestPlanSchemaV1 || len(v.Cases) < 4 || len(v.Cases) > 12 {
		return errors.New("invalid bounded test plan")
	}
	drafts := make([]DataCaseDraft, len(v.Cases))
	for i, item := range v.Cases {
		if item.Ordinal != i+1 || item.Seed != dataCaseSeed(v.EffectiveSeed, i+1) {
			return errors.New("test case differs from its host ordinal or derived seed")
		}
		drafts[i] = DataCaseDraft{item.Kind, item.Purpose}
	}
	return validateDataCaseDrafts(drafts)
}

func (v DataDraftV1) Bind(input DataDraftInputV1) (DataContent, error) {
	if err := v.Validate(); err != nil {
		return DataContent{}, err
	}
	digest, err := input.Digest()
	if err != nil {
		return DataContent{}, err
	}
	plan := TestPlanV1{SchemaVersion: TestPlanSchemaV1, EffectiveSeed: input.SolutionInput.Snapshot.EffectiveSeed, Cases: make([]DataCase, len(v.Cases))}
	for i, item := range v.Cases {
		plan.Cases[i] = DataCase{i + 1, item.Kind, item.Purpose, dataCaseSeed(plan.EffectiveSeed, i+1)}
	}
	content := DataContent{SchemaVersion: DataContentSchemaV1, InputDigest: digest, ProblemSpecDigest: input.Solution.ProblemSpecDigest, SolutionContentDigest: input.Solution.ContentDigest, SolutionVerificationDigest: input.SolutionVerificationDigest, Language: input.Solution.Language, GeneratorCode: v.GeneratorCode, ValidatorCode: v.ValidatorCode, Plan: plan}
	content.ContentDigest = contentSum(content)
	return content, content.ValidateInput(input)
}

func (v DataContent) Validate() error {
	if v.SchemaVersion != DataContentSchemaV1 {
		return errors.New("unsupported data content schema")
	}
	for _, digest := range []Digest{v.InputDigest, v.ProblemSpecDigest, v.SolutionContentDigest, v.SolutionVerificationDigest, v.ContentDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	if v.Language != "cpp" && v.Language != "go" {
		return errors.New("data programs require cpp or go")
	}
	if err := validateProgramSource(v.GeneratorCode); err != nil {
		return err
	}
	if err := validateProgramSource(v.ValidatorCode); err != nil {
		return err
	}
	if err := v.Plan.Validate(); err != nil {
		return err
	}
	expected := v.ContentDigest
	v.ContentDigest = ""
	if contentSum(v) != expected {
		return errors.New("data content digest differs")
	}
	return nil
}

func (v DataContent) ValidateInput(input DataDraftInputV1) error {
	if err := v.Validate(); err != nil {
		return err
	}
	digest, err := input.Digest()
	if err != nil {
		return err
	}
	if v.InputDigest != digest || v.ProblemSpecDigest != input.Solution.ProblemSpecDigest || v.SolutionContentDigest != input.Solution.ContentDigest || v.SolutionVerificationDigest != input.SolutionVerificationDigest || v.Language != input.Solution.Language || v.Plan.EffectiveSeed != input.SolutionInput.Snapshot.EffectiveSeed {
		return errors.New("data content differs from committed solution input")
	}
	return nil
}

func (v DataDraftInputV1) CanonicalJSON() ([]byte, error) { return contentJSON(v, v.Validate()) }
func (v DataDraftInputV1) Digest() (Digest, error) {
	raw, err := v.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return SumBytes(raw), nil
}
func (v DataDraftV1) CanonicalJSON() ([]byte, error) { return contentJSON(v, v.Validate()) }
func (v DataContent) CanonicalJSON() ([]byte, error) { return contentJSON(v, v.Validate()) }
func (v TestPlanV1) CanonicalJSON() ([]byte, error)  { return contentJSON(v, v.Validate()) }

func (v *DataDraftV1) UnmarshalJSON(raw []byte) error {
	type plain DataDraftV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := DataDraftV1(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*v = decoded
	return nil
}
func (v *DataDraftInputV1) UnmarshalJSON(raw []byte) error {
	type plain DataDraftInputV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := DataDraftInputV1(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*v = decoded
	return nil
}
func (v *DataContent) UnmarshalJSON(raw []byte) error {
	type plain DataContent
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := DataContent(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*v = decoded
	return nil
}
func (v *TestPlanV1) UnmarshalJSON(raw []byte) error {
	type plain TestPlanV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := TestPlanV1(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*v = decoded
	return nil
}
