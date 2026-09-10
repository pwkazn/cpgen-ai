package domain

import (
	"errors"
	"slices"
)

const IdeaMutationDraftInputSchemaV1 = "cpgen.idea-mutation-draft-input/v1"

type IdeaMutationDraftCandidateInput struct {
	CandidateOrdinal int      `json:"candidate_ordinal"`
	MutationOrdinal  int      `json:"mutation_ordinal"`
	SeedAxes         []string `json:"seed_axes"`
}

// This versioned input is separate from the initial batch-zero input. Provider
// content still uses IdeaDraftV1 and cannot replace any authorized lineage.
type IdeaMutationDraftInputV1 struct {
	SchemaVersion string                            `json:"schema_version"`
	Snapshot      GenerationRequestSnapshotV1       `json:"snapshot"`
	SourceBatch   IdeaBatch                         `json:"source_batch"`
	Intent        IdeaMutationIntent                `json:"intent"`
	BatchOrdinal  int                               `json:"batch_ordinal"`
	Candidates    []IdeaMutationDraftCandidateInput `json:"candidates"`
}

func NewIdeaMutationDraftInput(snapshot GenerationRequestSnapshotV1, source IdeaBatch, intent IdeaMutationIntent) (IdeaMutationDraftInputV1, error) {
	var empty IdeaMutationDraftInputV1
	if err := intent.Validate(); err != nil {
		return empty, err
	}
	if err := intent.Core.ValidateSource(snapshot, source); err != nil {
		return empty, err
	}
	input := IdeaMutationDraftInputV1{SchemaVersion: IdeaMutationDraftInputSchemaV1, Snapshot: snapshot, SourceBatch: source, Intent: intent,
		BatchOrdinal: source.BatchOrdinal + 1, Candidates: make([]IdeaMutationDraftCandidateInput, intent.Core.RequestedCount),
	}
	for ordinal := range input.Candidates {
		mutationOrdinal, err := intent.Core.ChildMutationOrdinal(ordinal)
		if err != nil {
			return empty, err
		}
		axes, err := DeriveIdeaSeedAxes(snapshot.RequestDigest, snapshot.EffectiveSeed, input.BatchOrdinal, ordinal, mutationOrdinal, SeedDerivationPolicyV1)
		if err != nil {
			return empty, err
		}
		input.Candidates[ordinal] = IdeaMutationDraftCandidateInput{CandidateOrdinal: ordinal, MutationOrdinal: mutationOrdinal, SeedAxes: axes}
	}
	// Preserve exact int64 seed and independent source/request slices.
	raw, err := input.CanonicalJSON()
	if err != nil {
		return empty, err
	}
	var cloned IdeaMutationDraftInputV1
	if err := cloned.UnmarshalJSON(raw); err != nil {
		return empty, err
	}
	return cloned, nil
}

func (input IdeaMutationDraftInputV1) Validate() error {
	if input.SchemaVersion != IdeaMutationDraftInputSchemaV1 {
		return errors.New("unsupported mutation draft input schema")
	}
	if err := input.Intent.Validate(); err != nil {
		return err
	}
	if err := input.Intent.Core.ValidateSource(input.Snapshot, input.SourceBatch); err != nil {
		return err
	}
	if input.BatchOrdinal != input.SourceBatch.BatchOrdinal+1 || len(input.Candidates) != input.Intent.Core.RequestedCount {
		return errors.New("mutation draft differs from authorized batch/count")
	}
	for ordinal, candidate := range input.Candidates {
		mutationOrdinal, err := input.Intent.Core.ChildMutationOrdinal(ordinal)
		if err != nil {
			return err
		}
		axes, err := DeriveIdeaSeedAxes(input.Snapshot.RequestDigest, input.Snapshot.EffectiveSeed, input.BatchOrdinal, ordinal, mutationOrdinal, SeedDerivationPolicyV1)
		if err != nil {
			return err
		}
		if candidate.CandidateOrdinal != ordinal || candidate.MutationOrdinal != mutationOrdinal || !slices.Equal(candidate.SeedAxes, axes) {
			return errors.New("mutation draft child differs from deterministic lineage/seed axes")
		}
	}
	return nil
}

func (draft IdeaDraftV1) BindMutation(input IdeaMutationDraftInputV1) (IdeaBatch, error) {
	if err := input.Validate(); err != nil {
		return IdeaBatch{}, err
	}
	if err := draft.Validate(); err != nil {
		return IdeaBatch{}, err
	}
	if len(draft.Candidates) != len(input.Candidates) {
		return IdeaBatch{}, errors.New("mutation draft count differs from authorized input")
	}
	candidates := make([]IdeaCandidate, len(draft.Candidates))
	for ordinal, candidate := range draft.Candidates {
		candidates[ordinal] = IdeaCandidate{CandidateOrdinal: ordinal, AbstractTask: candidate.AbstractTask, IntendedAlgorithm: candidate.IntendedAlgorithm,
			TargetComplexity: candidate.TargetComplexity, FeasibilityStatus: candidate.FeasibilityStatus, FeasibilityReasons: slices.Clone(candidate.FeasibilityReasons),
			NegativeConstraints: cleanSet(input.Snapshot.Request.ForbiddenFeatures), ParentIdeaID: input.Intent.Core.ParentIdeaID,
			MutationReason: input.Intent.Core.MutationReason, MutationOrdinal: input.Candidates[ordinal].MutationOrdinal,
			SeedAxes: slices.Clone(input.Candidates[ordinal].SeedAxes),
		}
	}
	return NewIdeaBatch(input.Snapshot, len(candidates), GenerationPolicyV1, candidates, input.BatchOrdinal)
}

func (input IdeaMutationDraftInputV1) CanonicalJSON() ([]byte, error) {
	return contentJSON(input, input.Validate())
}

func (input *IdeaMutationDraftInputV1) UnmarshalJSON(raw []byte) error {
	if len(raw) > 4<<20 {
		return errors.New("mutation draft input exceeds bounded envelope")
	}
	type plain IdeaMutationDraftInputV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := IdeaMutationDraftInputV1(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*input = decoded
	return nil
}
