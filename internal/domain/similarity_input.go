package domain

import "errors"

const SimilarityInputSchemaV1 = "cpgen.similarity-input/v1"

// SimilarityInputV1 binds the semantic stage input before a successor attempt
// exists. It contains identities only; the application derives the allowed
// wire projection from the verified ProblemSpec and adds its attempt identity.
type SimilarityInputV1 struct {
	SchemaVersion             string `json:"schema_version"`
	RequestSnapshotDigest     Digest `json:"request_snapshot_digest"`
	ProblemSpecDigest         Digest `json:"problem_spec_digest"`
	CandidateProjectionDigest Digest `json:"candidate_projection_digest"`
	DecisionPolicyDigest      Digest `json:"decision_policy_digest"`
	ProviderPolicyDigest      Digest `json:"provider_policy_digest"`
	ExecutionPolicyDigest     Digest `json:"execution_policy_digest"`
	Limit                     int    `json:"limit"`
}

func NewSimilarityInputV1(problem ProblemSpec, projection, decisionPolicy, providerPolicy, executionPolicy Digest, limit int) (SimilarityInputV1, error) {
	if err := problem.Validate(); err != nil {
		return SimilarityInputV1{}, err
	}
	input := SimilarityInputV1{SimilarityInputSchemaV1, problem.RequestSnapshotDigest, problem.SpecDigest, projection, decisionPolicy, providerPolicy, executionPolicy, limit}
	return input, input.Validate()
}

func (i SimilarityInputV1) Validate() error {
	if i.SchemaVersion != SimilarityInputSchemaV1 || i.Limit <= 0 || i.Limit > 10000 {
		return errors.New("invalid similarity input schema or result limit")
	}
	for _, digest := range []Digest{i.RequestSnapshotDigest, i.ProblemSpecDigest, i.CandidateProjectionDigest, i.DecisionPolicyDigest, i.ProviderPolicyDigest, i.ExecutionPolicyDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (i SimilarityInputV1) ValidateProblem(problem ProblemSpec, projection Digest) error {
	if err := i.Validate(); err != nil {
		return err
	}
	if err := problem.Validate(); err != nil {
		return err
	}
	if i.RequestSnapshotDigest != problem.RequestSnapshotDigest || i.ProblemSpecDigest != problem.SpecDigest || i.CandidateProjectionDigest != projection {
		return errors.New("similarity input differs from its verified problem or query projection")
	}
	return nil
}

func (i SimilarityInputV1) CanonicalJSON() ([]byte, error) { return contentJSON(i, i.Validate()) }
func (i SimilarityInputV1) Digest() (Digest, error) {
	raw, err := i.CanonicalJSON()
	return sumResult(raw, err)
}

func (i *SimilarityInputV1) UnmarshalJSON(raw []byte) error {
	type plain SimilarityInputV1
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	input := SimilarityInputV1(value)
	if err := input.Validate(); err != nil {
		return err
	}
	*i = input
	return nil
}
