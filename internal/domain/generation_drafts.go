package domain

import (
	"errors"
	"fmt"
	"slices"
)

const (
	IdeaDraftSchemaV1           = "cpgen.idea-draft/v1"
	IdeaDraftInputSchemaV1      = "cpgen.idea-draft-input/v1"
	StatementDraftSchemaV1      = "cpgen.statement-draft/v1"
	StatementDraftInputSchemaV1 = "cpgen.statement-draft-input/v1"
)

// Drafts contain proposed content only. Provider output cannot assign domain
// identities, seed axes, lineage, resource limits or request/selection bindings.
type IdeaContentDraft struct {
	AbstractTask       string   `json:"abstract_task"`
	IntendedAlgorithm  string   `json:"intended_algorithm"`
	TargetComplexity   string   `json:"target_complexity"`
	FeasibilityStatus  string   `json:"feasibility_status"`
	FeasibilityReasons []string `json:"feasibility_reasons"`
}

type IdeaDraftV1 struct {
	SchemaVersion string             `json:"schema_version"`
	Candidates    []IdeaContentDraft `json:"candidates"`
}

type IdeaDraftCandidateInput struct {
	CandidateOrdinal int      `json:"candidate_ordinal"`
	SeedAxes         []string `json:"seed_axes"`
}

type IdeaDraftInputV1 struct {
	SchemaVersion  string                      `json:"schema_version"`
	Snapshot       GenerationRequestSnapshotV1 `json:"snapshot"`
	RequestedCount int                         `json:"requested_count"`
	Candidates     []IdeaDraftCandidateInput   `json:"candidates"`
}

func NewIdeaDraftInput(snapshot GenerationRequestSnapshotV1, count int) (IdeaDraftInputV1, error) {
	if err := snapshot.Validate(); err != nil {
		return IdeaDraftInputV1{}, err
	}
	if count < 2 || count > 8 {
		return IdeaDraftInputV1{}, errors.New("idea draft requires 2-8 candidates")
	}
	snapshot.Request = snapshot.Request.clone()
	input := IdeaDraftInputV1{SchemaVersion: IdeaDraftInputSchemaV1, Snapshot: snapshot, RequestedCount: count, Candidates: make([]IdeaDraftCandidateInput, count)}
	for ordinal := range count {
		axes, err := DeriveIdeaSeedAxes(snapshot.RequestDigest, snapshot.EffectiveSeed, 0, ordinal, 0, SeedDerivationPolicyV1)
		if err != nil {
			return IdeaDraftInputV1{}, err
		}
		input.Candidates[ordinal] = IdeaDraftCandidateInput{CandidateOrdinal: ordinal, SeedAxes: axes}
	}
	return input, input.Validate()
}

func (input IdeaDraftInputV1) Validate() error {
	if input.SchemaVersion != IdeaDraftInputSchemaV1 || input.RequestedCount < 2 || input.RequestedCount > 8 || len(input.Candidates) != input.RequestedCount {
		return errors.New("invalid idea draft input schema or count")
	}
	if err := input.Snapshot.Validate(); err != nil {
		return err
	}
	for ordinal, candidate := range input.Candidates {
		axes, err := DeriveIdeaSeedAxes(input.Snapshot.RequestDigest, input.Snapshot.EffectiveSeed, 0, ordinal, 0, SeedDerivationPolicyV1)
		if err != nil {
			return err
		}
		if candidate.CandidateOrdinal != ordinal || !slices.Equal(candidate.SeedAxes, axes) {
			return errors.New("idea draft input differs from deterministic seed derivation")
		}
	}
	return nil
}

func (draft IdeaDraftV1) Validate() error {
	if draft.SchemaVersion != IdeaDraftSchemaV1 || len(draft.Candidates) < 2 || len(draft.Candidates) > 8 {
		return errors.New("invalid idea draft schema or candidate count")
	}
	for _, candidate := range draft.Candidates {
		if err := validateText(32768, false, candidate.AbstractTask, candidate.IntendedAlgorithm, candidate.TargetComplexity); err != nil {
			return err
		}
		if candidate.FeasibilityStatus != "FEASIBLE" && candidate.FeasibilityStatus != "REJECTED" {
			return errors.New("invalid draft feasibility status")
		}
		if err := validateSet(candidate.FeasibilityReasons); err != nil {
			return err
		}
		if candidate.FeasibilityStatus == "REJECTED" && len(candidate.FeasibilityReasons) == 0 {
			return errors.New("rejected draft candidate requires reasons")
		}
	}
	return nil
}

// Bind creates the initial batch using CPGen's existing deterministic domain
// constructor. Later mutation needs its own authorized lineage contract.
func (draft IdeaDraftV1) Bind(input IdeaDraftInputV1) (IdeaBatch, error) {
	if err := input.Validate(); err != nil {
		return IdeaBatch{}, err
	}
	if err := draft.Validate(); err != nil {
		return IdeaBatch{}, err
	}
	if len(draft.Candidates) != input.RequestedCount {
		return IdeaBatch{}, errors.New("draft candidate count differs from admitted input")
	}
	candidates := make([]IdeaCandidate, len(draft.Candidates))
	for ordinal, candidate := range draft.Candidates {
		candidates[ordinal] = IdeaCandidate{CandidateOrdinal: ordinal, AbstractTask: candidate.AbstractTask, IntendedAlgorithm: candidate.IntendedAlgorithm, TargetComplexity: candidate.TargetComplexity, FeasibilityStatus: candidate.FeasibilityStatus, FeasibilityReasons: slices.Clone(candidate.FeasibilityReasons), NegativeConstraints: cleanSet(input.Snapshot.Request.ForbiddenFeatures)}
	}
	return NewIdeaBatch(input.Snapshot, input.RequestedCount, GenerationPolicyV1, candidates)
}

func (draft *IdeaDraftV1) UnmarshalJSON(raw []byte) error {
	type plain IdeaDraftV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := IdeaDraftV1(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*draft = decoded
	return nil
}

func (input *IdeaDraftInputV1) UnmarshalJSON(raw []byte) error {
	type plain IdeaDraftInputV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := IdeaDraftInputV1(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*input = decoded
	return nil
}

type StatementDraftV1 struct {
	SchemaVersion string          `json:"schema_version"`
	Title         string          `json:"title"`
	Description   string          `json:"description"`
	Input         ProblemIO       `json:"input"`
	Output        ProblemIO       `json:"output"`
	Samples       []ProblemSample `json:"samples"`
}

// StatementDraftInputV1 carries the exact resolved chain. A digest-only input
// would force the model to guess the selected idea and frozen constraints.
type StatementDraftInputV1 struct {
	SchemaVersion string                      `json:"schema_version"`
	Input         StatementInput              `json:"input"`
	Snapshot      GenerationRequestSnapshotV1 `json:"snapshot"`
	Batch         IdeaBatch                   `json:"batch"`
	Selection     IdeaSelection               `json:"selection"`
}

func NewStatementDraftInput(input StatementInput, snapshot GenerationRequestSnapshotV1, batch IdeaBatch, selection IdeaSelection) (StatementDraftInputV1, error) {
	value := StatementDraftInputV1{SchemaVersion: StatementDraftInputSchemaV1, Input: input, Snapshot: snapshot, Batch: batch, Selection: selection}
	if err := value.Validate(); err != nil {
		return StatementDraftInputV1{}, err
	}
	// Decode the canonical chain to avoid mutable slice aliases across the
	// application/stage boundary while preserving int64 seeds and sample bytes.
	raw, err := value.CanonicalJSON()
	if err != nil {
		return StatementDraftInputV1{}, err
	}
	var cloned StatementDraftInputV1
	if err := cloned.UnmarshalJSON(raw); err != nil {
		return StatementDraftInputV1{}, err
	}
	return cloned, nil
}

func (input StatementDraftInputV1) Validate() error {
	if input.SchemaVersion != StatementDraftInputSchemaV1 {
		return errors.New("invalid statement draft input schema")
	}
	return input.Input.ValidateChain(input.Snapshot, input.Batch, input.Selection)
}

func (draft StatementDraftV1) Validate() error {
	if draft.SchemaVersion != StatementDraftSchemaV1 {
		return errors.New("invalid statement draft schema")
	}
	if err := validateText(32768, false, draft.Title, draft.Description); err != nil {
		return err
	}
	for _, value := range []ProblemIO{draft.Input, draft.Output} {
		if err := validateText(32768, false, value.Description); err != nil {
			return err
		}
		if len(value.Fields) == 0 || len(value.Fields) > 256 {
			return errors.New("draft input/output requires 1-256 structured fields")
		}
		if err := validateText(4096, false, value.Fields...); err != nil {
			return err
		}
		if err := uniqueNonEmpty("draft IO field", value.Fields); err != nil {
			return err
		}
	}
	if len(draft.Samples) == 0 || len(draft.Samples) > 32 {
		return errors.New("draft requires 1-32 samples")
	}
	for _, sample := range draft.Samples {
		if err := validateSampleData(sample.Input, sample.Output); err != nil {
			return err
		}
		if err := validateText(32768, true, sample.Explanation); err != nil {
			return err
		}
	}
	return nil
}

func (draft StatementDraftV1) Bind(input StatementDraftInputV1, revision int64) (ProblemSpec, error) {
	if err := input.Validate(); err != nil {
		return ProblemSpec{}, err
	}
	if err := draft.Validate(); err != nil {
		return ProblemSpec{}, err
	}
	if revision < 1 {
		return ProblemSpec{}, errors.New("problem revision must be positive")
	}
	problem, err := NewProblemSpec(input.Input, input.Snapshot, input.Batch, input.Selection, ProblemSpec{Revision: revision, Title: draft.Title, Description: draft.Description, Input: draft.Input, Output: draft.Output, Samples: draft.Samples})
	if err != nil {
		return ProblemSpec{}, fmt.Errorf("bind statement draft: %w", err)
	}
	return problem, nil
}

func (draft *StatementDraftV1) UnmarshalJSON(raw []byte) error {
	type plain StatementDraftV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := StatementDraftV1(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*draft = decoded
	return nil
}

func (input *StatementDraftInputV1) UnmarshalJSON(raw []byte) error {
	type plain StatementDraftInputV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := StatementDraftInputV1(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*input = decoded
	return nil
}

func (input IdeaDraftInputV1) CanonicalJSON() ([]byte, error) {
	return contentJSON(input, input.Validate())
}
func (input StatementDraftInputV1) CanonicalJSON() ([]byte, error) {
	return contentJSON(input, input.Validate())
}
func (draft IdeaDraftV1) CanonicalJSON() ([]byte, error) { return contentJSON(draft, draft.Validate()) }
func (draft StatementDraftV1) CanonicalJSON() ([]byte, error) {
	return contentJSON(draft, draft.Validate())
}
