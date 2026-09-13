package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

const SimilarityRoutePlanSchemaV1 = "cpgen.similarity-route-plan/v1"
const similarityRoutingPolicyV1 = "similarity-routing/v1"

type SimilarityRouteAction string

const (
	SimilarityRouteContinue   SimilarityRouteAction = "CONTINUE"
	SimilarityRouteReview     SimilarityRouteAction = "REVIEW"
	SimilarityRouteMutateIdea SimilarityRouteAction = "MUTATE_IDEA"
	SimilarityRouteRecheck    SimilarityRouteAction = "RECHECK_SIMILARITY"
)

// SimilarityRoutePlan is a deterministic read projection, never an execution
// grant. A later transaction must recheck these bindings and acquire any quota
// before invalidation or provider dispatch. The existing preview graph does not
// consume this plan or change its non-waivable checkpoint semantics.
type SimilarityRoutePlan struct {
	SchemaVersion         string                        `json:"schema_version"`
	RuleVersion           string                        `json:"rule_version"`
	RunID                 domain.RunID                  `json:"run_id"`
	RunVersion            int64                         `json:"run_version"`
	WorkflowRevision      string                        `json:"workflow_revision"`
	ConfigDigest          domain.Digest                 `json:"config_digest"`
	RequestSnapshotDigest domain.Digest                 `json:"request_snapshot_digest"`
	SourceBatchDigest     domain.Digest                 `json:"source_batch_digest"`
	SelectedIdeaID        string                        `json:"selected_idea_id"`
	ProblemSpecDigest     domain.Digest                 `json:"problem_spec_digest"`
	SimilarityInput       domain.SimilarityInputV1      `json:"similarity_input"`
	EvidenceDigest        domain.Digest                 `json:"evidence_digest"`
	Decision              similarity.Decision           `json:"decision"`
	DecisionDigest        domain.Digest                 `json:"decision_digest"`
	MutationScopeDigest   domain.Digest                 `json:"mutation_scope_digest"`
	MutationBudget        domain.MutationBudgetSnapshot `json:"mutation_budget"`
	NextMutationOrdinal   int64                         `json:"next_mutation_ordinal,omitempty"`
	Action                SimilarityRouteAction         `json:"action"`
	Reason                string                        `json:"reason"`
	PlanDigest            domain.Digest                 `json:"plan_digest"`
}

func (p SimilarityRoutePlan) Validate() error {
	if p.SchemaVersion != SimilarityRoutePlanSchemaV1 || p.RuleVersion != similarityRoutingPolicyV1 || p.RunVersion <= 0 || p.WorkflowRevision != workflow.LegacySimilarityCheckpointRevision {
		return errors.New("unsupported similarity route plan identity")
	}
	if err := p.RunID.Validate(); err != nil {
		return err
	}
	for _, digest := range []domain.Digest{p.ConfigDigest, p.RequestSnapshotDigest, p.SourceBatchDigest, p.ProblemSpecDigest, p.EvidenceDigest, p.DecisionDigest, p.MutationScopeDigest, p.PlanDigest} {
		if err := digest.Validate(); err != nil {
			return err
		}
	}
	if p.SelectedIdeaID == "" || len(p.SelectedIdeaID) > 256 || strings.ContainsAny(p.SelectedIdeaID, "\x00\r\n") {
		return errors.New("route plan selected idea is invalid")
	}
	if err := p.SimilarityInput.Validate(); err != nil {
		return err
	}
	if p.SimilarityInput.RequestSnapshotDigest != p.RequestSnapshotDigest || p.SimilarityInput.ProblemSpecDigest != p.ProblemSpecDigest {
		return errors.New("route plan input chain differs")
	}
	if err := p.Decision.Validate(); err != nil {
		return err
	}
	if p.Decision.EvidenceDigest != p.EvidenceDigest || p.Decision.PolicyDigest != p.SimilarityInput.DecisionPolicyDigest {
		return errors.New("route plan decision differs from collected evidence policy")
	}
	decision, err := canonicalJSON(p.Decision)
	if err != nil {
		return err
	}
	if domain.SumBytes(decision) != p.DecisionDigest {
		return errors.New("route plan decision digest differs")
	}
	if err := p.MutationBudget.Validate(); err != nil {
		return err
	}
	if p.MutationBudget.RunID != p.RunID || p.MutationBudget.StageName != "idea" || p.MutationBudget.RunVersion != p.RunVersion {
		return errors.New("route plan quota scope differs")
	}
	scope, err := ideaMutationScope(p.RunID, p.WorkflowRevision)
	if err != nil {
		return err
	}
	if scope != p.MutationScopeDigest {
		return errors.New("route plan mutation scope differs")
	}
	action, reason, ordinal, err := similarityRoute(p.Decision, p.MutationBudget)
	if err != nil {
		return err
	}
	if p.Action != action || p.Reason != reason || p.NextMutationOrdinal != ordinal {
		return errors.New("route action differs from evidence and remaining quota")
	}
	expected, err := p.digest()
	if err != nil {
		return err
	}
	if p.PlanDigest != expected {
		return errors.New("similarity route plan digest differs")
	}
	return nil
}

func (p SimilarityRoutePlan) digest() (domain.Digest, error) {
	p.PlanDigest = ""
	raw, err := canonicalJSON(p)
	if err != nil {
		return "", err
	}
	return domain.SumBytes(raw), nil
}

func (p SimilarityRoutePlan) CanonicalJSON() ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return canonicalJSON(p)
}

func (p *SimilarityRoutePlan) UnmarshalJSON(raw []byte) error {
	type plain SimilarityRoutePlan
	var decoded plain
	if err := port.DecodeStructuredOutput(raw, SimilarityRoutePlanSchemaV1, 64<<10, &decoded); err != nil {
		return err
	}
	// This envelope has no nullable field. encoding/json otherwise silently
	// turns null scalar fields into zero, changing the represented document.
	tokens := json.NewDecoder(bytes.NewReader(raw))
	for {
		token, err := tokens.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if token == nil {
			return errors.New("similarity route plan contains a null field")
		}
	}
	value := SimilarityRoutePlan(decoded)
	if err := value.Validate(); err != nil {
		return err
	}
	*p = value
	return nil
}

func ideaMutationScope(runID domain.RunID, revision string) (domain.Digest, error) {
	return domain.IdeaMutationScope(runID, revision)
}

// MutationCore prepares immutable content for a later checked transaction. It
// does not spend quota, retain historical proof or authorize execution.
func (p SimilarityRoutePlan) MutationCore(snapshot domain.GenerationRequestSnapshotV1, source domain.IdeaBatch) (domain.IdeaMutationCore, error) {
	var empty domain.IdeaMutationCore
	if err := p.Validate(); err != nil {
		return empty, err
	}
	if p.Action != SimilarityRouteMutateIdea {
		return empty, errors.New("route does not propose Idea mutation")
	}
	if snapshot.SnapshotDigest != p.RequestSnapshotDigest || source.BatchDigest != p.SourceBatchDigest || snapshot.Request.BudgetLimits.MaxMutationsPerStage != p.MutationBudget.Limit {
		return empty, errors.New("mutation route source differs from committed plan")
	}
	return domain.NewIdeaMutationCore(snapshot, source, domain.IdeaMutationParameters{
		RunID: p.RunID, WorkflowRevision: p.WorkflowRevision, ConfigDigest: p.ConfigDigest,
		TriggerKind: domain.IdeaMutationSimilarity, TriggerEvidenceDigest: p.DecisionDigest,
		ParentIdeaID: p.SelectedIdeaID, MutationOrdinal: p.NextMutationOrdinal,
	})
}

func similarityRoute(decision similarity.Decision, budget domain.MutationBudgetSnapshot) (SimilarityRouteAction, string, int64, error) {
	switch decision.Kind {
	case similarity.DecisionAccept:
		return SimilarityRouteContinue, "similarity_accepted", 0, nil
	case similarity.DecisionNeedsReview:
		return SimilarityRouteReview, string(decision.ExplanationCode), 0, nil
	case similarity.DecisionReject:
		if budget.Remaining() == 0 {
			return SimilarityRouteReview, "idea_mutation_budget_exhausted", 0, nil
		}
		return SimilarityRouteMutateIdea, string(decision.ExplanationCode), budget.Claimed + 1, nil
	case similarity.DecisionBlocked:
		if decision.ExplanationCode != similarity.ExplanationInsufficient {
			return "", "", 0, errors.New("route plan requires verified compatible Similarity evidence")
		}
		return SimilarityRouteRecheck, string(decision.ExplanationCode), 0, nil
	default:
		return "", "", 0, errors.New("unsupported Similarity decision")
	}
}

type similarityRouteReadStore interface {
	GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
	PendingCancel(context.Context, domain.RunID) (*domain.ControlRequest, error)
	ReadStageInputDigest(context.Context, domain.RunID, domain.StageName) (domain.Digest, error)
}

type SimilarityRoutePlanner struct {
	evidence *SimilarityContentReader
	budgets  port.MutationBudgetReader
	locks    *runlock.Manager
	store    similarityRouteReadStore
	policy   domain.Digest
}

func NewSimilarityRoutePlanner(executor *SimilarityExecutor) (*SimilarityRoutePlanner, error) {
	if executor == nil {
		return nil, errors.New("similarity route planning requires a committed evidence executor")
	}
	budgets, ok := executor.store.(port.MutationBudgetReader)
	if !ok {
		return nil, errors.New("similarity route planning requires authoritative mutation quota reads")
	}
	store, ok := executor.store.(similarityRouteReadStore)
	if !ok {
		return nil, errors.New("route planning requires current run and stage input readers")
	}
	return &SimilarityRoutePlanner{executor.Reader(), budgets, executor.locks, store, executor.admission.policy}, nil
}

// Read owns shared run/artifact locks for a standalone inspection. It performs
// no provider operation, quota claim, stage transition or cache lookup. Future
// execution must use an explicit compatible graph and a checked write command.
func (p *SimilarityRoutePlanner) Read(ctx context.Context, runID domain.RunID) (SimilarityRoutePlan, error) {
	var empty SimilarityRoutePlan
	if ctx == nil {
		return empty, errors.New("similarity route read requires a context")
	}
	if err := runID.Validate(); err != nil {
		return empty, err
	}
	guard, err := p.locks.AcquireRun(ctx, runID, runlock.Shared)
	if err != nil {
		return empty, err
	}
	defer guard.Close()
	artifacts, err := p.locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		return empty, err
	}
	defer artifacts.Close()
	current, err := p.store.GetRun(ctx, runID)
	if err != nil {
		return empty, err
	}
	if current.WorkflowRevision != workflow.LegacySimilarityCheckpointRevision || current.WorkflowDigest != domain.SumBytes([]byte(current.WorkflowRevision)) || current.ConfigDigest != p.policy || current.CurrentStage != "slice2_checkpoint" || (current.State != domain.RunRunning && current.State != domain.RunNeedsReview) {
		return empty, errors.New("route planning requires the current frozen Similarity checkpoint")
	}
	if pending, err := p.store.PendingCancel(ctx, runID); err != nil {
		return empty, err
	} else if pending != nil {
		return empty, errors.New("cancelled work cannot plan a new business route")
	}
	content, err := p.evidence.ReadCommitted(ctx, runID)
	if err != nil {
		return empty, err
	}
	input, err := p.store.ReadStageInputDigest(ctx, runID, "slice2_checkpoint")
	if err != nil {
		return empty, err
	}
	if input != content.Evidence.EvidenceDigest {
		return empty, errors.New("route checkpoint input differs from committed evidence")
	}
	budget, err := p.budgets.ReadMutationBudget(ctx, runID, "idea")
	if err != nil {
		return empty, err
	}
	if budget.RunVersion != current.Version || budget.Limit != content.Statement.Idea.Snapshot.Request.BudgetLimits.MaxMutationsPerStage {
		return empty, errors.New("mutation quota differs from frozen request or current run version")
	}
	decision, err := canonicalJSON(content.Decision)
	if err != nil {
		return empty, err
	}
	scope, err := ideaMutationScope(runID, current.WorkflowRevision)
	if err != nil {
		return empty, err
	}
	action, reason, ordinal, err := similarityRoute(content.Decision, budget)
	if err != nil {
		return empty, err
	}
	plan := SimilarityRoutePlan{SchemaVersion: SimilarityRoutePlanSchemaV1, RuleVersion: similarityRoutingPolicyV1, RunID: runID, RunVersion: current.Version, WorkflowRevision: current.WorkflowRevision, ConfigDigest: current.ConfigDigest,
		RequestSnapshotDigest: content.Statement.Idea.Snapshot.SnapshotDigest, SourceBatchDigest: content.Statement.Idea.Batch.BatchDigest, SelectedIdeaID: content.Statement.Idea.Selection.SelectedIdeaID,
		ProblemSpecDigest: content.Statement.Problem.SpecDigest, SimilarityInput: content.Input, EvidenceDigest: content.Evidence.EvidenceDigest, Decision: content.Decision, DecisionDigest: domain.SumBytes(decision),
		MutationScopeDigest: scope, MutationBudget: budget, NextMutationOrdinal: ordinal, Action: action, Reason: reason,
	}
	plan.PlanDigest, err = plan.digest()
	if err != nil {
		return empty, err
	}
	if err := plan.Validate(); err != nil {
		return empty, err
	}
	after, err := p.store.GetRun(ctx, runID)
	if err != nil {
		return empty, err
	}
	if after.Version != current.Version {
		return empty, errors.New("run changed while reading its route plan")
	}
	return plan, nil
}
