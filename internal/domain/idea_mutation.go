package domain

import (
	"encoding/json"
	"errors"
	"math"
	"time"
)

const (
	IdeaMutationCoreSchemaV1   = "cpgen.idea-mutation-core/v1"
	IdeaMutationIntentSchemaV1 = "cpgen.idea-mutation-intent/v1"
	IdeaMutationPolicyV1       = "idea-mutation-policy/v1"
	IdeaMutationSimilarity     = "SIMILARITY"
	IdeaMutationNoFeasible     = "NO_FEASIBLE"
)

// IdeaMutationParameters is supplied by a verified application decision. These
// content contracts cannot prove that a trigger or quota claim is committed;
// the execution boundary must independently resolve that durable authority.
type IdeaMutationParameters struct {
	RunID                 RunID
	WorkflowRevision      string
	ConfigDigest          Digest
	TriggerKind           string
	TriggerEvidenceDigest Digest
	ParentIdeaID          string
	MutationOrdinal       int64
}

// IdeaMutationCore is hashed before claiming quota. Its digest is the existing
// MutationClaimRequest.IntentDigest; it deliberately contains no future claim ID.
type IdeaMutationCore struct {
	SchemaVersion         string `json:"schema_version"`
	PolicyVersion         string `json:"policy_version"`
	RunID                 RunID  `json:"run_id"`
	WorkflowRevision      string `json:"workflow_revision"`
	ConfigDigest          Digest `json:"config_digest"`
	RequestDigest         Digest `json:"request_digest"`
	RequestSnapshotDigest Digest `json:"request_snapshot_digest"`
	SourceBatchDigest     Digest `json:"source_batch_digest"`
	SourceBatchOrdinal    int    `json:"source_batch_ordinal"`
	RequestedCount        int    `json:"requested_count"`
	ParentIdeaID          string `json:"parent_idea_id,omitempty"`
	TriggerKind           string `json:"trigger_kind"`
	TriggerEvidenceDigest Digest `json:"trigger_evidence_digest"`
	MutationReason        string `json:"mutation_reason"`
	MutationOrdinal       int64  `json:"mutation_ordinal"`
	LimitSnapshot         int64  `json:"limit_snapshot"`
	StageScopeDigest      Digest `json:"stage_scope_digest"`
	CoreDigest            Digest `json:"core_digest"`
}

func IdeaMutationScope(runID RunID, revision string) (Digest, error) {
	if err := runID.Validate(); err != nil {
		return "", err
	}
	if err := validateText(256, false, revision); err != nil {
		return "", err
	}
	return sumResult(contentJSON(struct {
		RunID            RunID     `json:"run_id"`
		WorkflowRevision string    `json:"workflow_revision"`
		Stage            StageName `json:"stage_name"`
		Policy           string    `json:"policy_version"`
	}{runID, revision, "idea", IdeaMutationPolicyV1}, nil))
}

func NewIdeaMutationCore(snapshot GenerationRequestSnapshotV1, source IdeaBatch, parameters IdeaMutationParameters) (IdeaMutationCore, error) {
	var empty IdeaMutationCore
	if err := snapshot.Validate(); err != nil {
		return empty, err
	}
	if err := source.Validate(); err != nil {
		return empty, err
	}
	scope, err := IdeaMutationScope(parameters.RunID, parameters.WorkflowRevision)
	if err != nil {
		return empty, err
	}
	reason := "similarity_rejected"
	if parameters.TriggerKind == IdeaMutationNoFeasible {
		reason = "no_feasible_candidate"
	}
	core := IdeaMutationCore{SchemaVersion: IdeaMutationCoreSchemaV1, PolicyVersion: IdeaMutationPolicyV1,
		RunID: parameters.RunID, WorkflowRevision: parameters.WorkflowRevision, ConfigDigest: parameters.ConfigDigest,
		RequestDigest: snapshot.RequestDigest, RequestSnapshotDigest: snapshot.SnapshotDigest,
		SourceBatchDigest: source.BatchDigest, SourceBatchOrdinal: source.BatchOrdinal, RequestedCount: source.RequestedCount,
		ParentIdeaID: parameters.ParentIdeaID, TriggerKind: parameters.TriggerKind, TriggerEvidenceDigest: parameters.TriggerEvidenceDigest,
		MutationReason: reason, MutationOrdinal: parameters.MutationOrdinal, LimitSnapshot: snapshot.Request.BudgetLimits.MaxMutationsPerStage,
		StageScopeDigest: scope,
	}
	core.CoreDigest = contentSum(core)
	return core, core.ValidateSource(snapshot, source)
}

func (c IdeaMutationCore) Validate() error {
	if c.SchemaVersion != IdeaMutationCoreSchemaV1 || c.PolicyVersion != IdeaMutationPolicyV1 || c.RequestedCount < 2 || c.RequestedCount > 8 || c.SourceBatchOrdinal < 0 || c.SourceBatchOrdinal == math.MaxInt {
		return errors.New("invalid mutation core schema, count or batch ordinal")
	}
	if c.MutationOrdinal <= 0 || c.MutationOrdinal > c.LimitSnapshot {
		return errors.New("mutation logical ordinal exceeds frozen quota")
	}
	// Eight slots per logical claim keep child ordinals unique even if a later
	// policy permits different batch sizes. Check before arithmetic/conversion.
	if c.MutationOrdinal > int64(math.MaxInt)/8 {
		return errors.New("mutation child ordinal range overflows")
	}
	for _, digest := range []Digest{c.ConfigDigest, c.RequestDigest, c.RequestSnapshotDigest, c.SourceBatchDigest, c.TriggerEvidenceDigest, c.StageScopeDigest, c.CoreDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	scope, err := IdeaMutationScope(c.RunID, c.WorkflowRevision)
	if err != nil {
		return err
	}
	if scope != c.StageScopeDigest {
		return errors.New("mutation stage scope differs")
	}
	switch c.TriggerKind {
	case IdeaMutationSimilarity:
		if c.MutationReason != "similarity_rejected" || validIdeaID(c.ParentIdeaID) != nil {
			return errors.New("similarity mutation requires its selected parent")
		}
	case IdeaMutationNoFeasible:
		if c.MutationReason != "no_feasible_candidate" || c.ParentIdeaID != "" {
			return errors.New("no-feasible mutation regenerates the source batch without a selected parent")
		}
	default:
		return errors.New("unsupported mutation trigger")
	}
	copy := c
	copy.CoreDigest = ""
	return matchContentDigest(c.CoreDigest, copy)
}

func (c IdeaMutationCore) ValidateSource(snapshot GenerationRequestSnapshotV1, source IdeaBatch) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := snapshot.Validate(); err != nil {
		return err
	}
	if err := source.Validate(); err != nil {
		return err
	}
	if c.RequestDigest != snapshot.RequestDigest || c.RequestSnapshotDigest != snapshot.SnapshotDigest || source.RequestDigest != snapshot.RequestDigest || source.EffectiveSeed != snapshot.EffectiveSeed || source.CallBudget != snapshot.Request.BudgetLimits || c.SourceBatchDigest != source.BatchDigest || c.SourceBatchOrdinal != source.BatchOrdinal || c.RequestedCount != source.RequestedCount || c.LimitSnapshot != source.CallBudget.MaxMutationsPerStage {
		return errors.New("mutation source differs from frozen request or batch")
	}
	if c.TriggerKind == IdeaMutationNoFeasible {
		evidence, err := NoFeasibleIdeaEvidence(source)
		if err != nil {
			return err
		}
		if evidence != c.TriggerEvidenceDigest {
			return errors.New("mutation no-feasible evidence differs from source candidates")
		}
		return nil
	}
	for _, candidate := range source.Candidates {
		if candidate.IdeaID == c.ParentIdeaID && candidate.FeasibilityStatus == "FEASIBLE" {
			return nil
		}
	}
	return errors.New("mutation selected parent is not a feasible source candidate")
}

// NoFeasibleIdeaEvidence summarizes the recorded candidate assessments. It
// cannot establish algorithm correctness or replace later Judge/Quality gates.
// The application must retain the full source proof before using this trigger.
func NoFeasibleIdeaEvidence(source IdeaBatch) (Digest, error) {
	if err := source.Validate(); err != nil {
		return "", err
	}
	if len(source.FeasibleCandidateIDs()) != 0 {
		return "", errors.New("source batch still has feasible candidates")
	}
	return sumResult(contentJSON(struct {
		Policy            string          `json:"policy_version"`
		SourceBatchDigest Digest          `json:"source_batch_digest"`
		Candidates        []IdeaCandidate `json:"candidates"`
	}{"recorded-candidate-feasibility/v1", source.BatchDigest, source.Candidates}, nil))
}

func (c IdeaMutationCore) ChildMutationOrdinal(candidateOrdinal int) (int, error) {
	if err := c.Validate(); err != nil {
		return 0, err
	}
	if candidateOrdinal < 0 || candidateOrdinal >= c.RequestedCount {
		return 0, errors.New("mutation candidate ordinal is outside the batch")
	}
	return int((c.MutationOrdinal-1)*8) + candidateOrdinal + 1, nil
}

// ClaimRequest constructs the original quota command. The checked application
// transaction must retain its exact timestamp and bytes for idempotent replay.
func (c IdeaMutationCore) ClaimRequest(at time.Time) (MutationClaimRequest, error) {
	if err := c.Validate(); err != nil {
		return MutationClaimRequest{}, err
	}
	request := MutationClaimRequest{RunID: c.RunID, StageName: "idea", ScopeDigest: c.StageScopeDigest, SourceBatchDigest: c.SourceBatchDigest, Ordinal: c.MutationOrdinal, LimitSnapshot: c.LimitSnapshot, Kind: MutationContent, IntentDigest: c.CoreDigest, At: at}
	return request, request.Validate()
}

// IdeaMutationIntent binds the pre-claim core to a returned durable grant. A
// structurally valid value is still not proof that the grant exists in SQLite.
type IdeaMutationIntent struct {
	SchemaVersion string           `json:"schema_version"`
	Core          IdeaMutationCore `json:"core"`
	Grant         MutationGrant    `json:"grant"`
	IntentDigest  Digest           `json:"intent_digest"`
}

func NewIdeaMutationIntent(core IdeaMutationCore, grant MutationGrant) (IdeaMutationIntent, error) {
	value := IdeaMutationIntent{SchemaVersion: IdeaMutationIntentSchemaV1, Core: core, Grant: grant}
	value.IntentDigest = contentSum(value)
	return value, value.Validate()
}

func (i IdeaMutationIntent) Validate() error {
	if i.SchemaVersion != IdeaMutationIntentSchemaV1 {
		return errors.New("invalid idea mutation intent schema")
	}
	if err := i.Core.Validate(); err != nil {
		return err
	}
	if err := i.Grant.Validate(); err != nil {
		return err
	}
	g, c := i.Grant, i.Core
	if g.RunID != c.RunID || g.StageName != "idea" || g.ScopeDigest != c.StageScopeDigest || g.SourceBatchDigest != c.SourceBatchDigest || g.Ordinal != c.MutationOrdinal || g.LimitSnapshot != c.LimitSnapshot || g.Kind != MutationContent || g.IntentDigest != c.CoreDigest {
		return errors.New("mutation grant differs from pre-claim core")
	}
	g.GrantDigest = ""
	// Slice 1 grant receipts hash the declared struct JSON order. Preserve that
	// immutable protocol; new core/intent envelopes use sorted content JSON.
	grantJSON, err := json.Marshal(g)
	if err != nil {
		return err
	}
	if SumBytes(grantJSON) != i.Grant.GrantDigest {
		return errors.New("mutation grant receipt digest differs")
	}
	copy := i
	copy.IntentDigest = ""
	return matchContentDigest(i.IntentDigest, copy)
}

func (c IdeaMutationCore) CanonicalJSON() ([]byte, error)   { return contentJSON(c, c.Validate()) }
func (i IdeaMutationIntent) CanonicalJSON() ([]byte, error) { return contentJSON(i, i.Validate()) }

func (c *IdeaMutationCore) UnmarshalJSON(raw []byte) error {
	if len(raw) > 16<<10 {
		return errors.New("mutation core exceeds bounded envelope")
	}
	type plain IdeaMutationCore
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := IdeaMutationCore(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*c = decoded
	return nil
}

func (i *IdeaMutationIntent) UnmarshalJSON(raw []byte) error {
	if len(raw) > 32<<10 {
		return errors.New("mutation intent exceeds bounded envelope")
	}
	type plain IdeaMutationIntent
	var value plain
	if err := strictJSON(string(raw), &value); err != nil {
		return err
	}
	decoded := IdeaMutationIntent(value)
	if err := decoded.Validate(); err != nil {
		return err
	}
	*i = decoded
	return nil
}
