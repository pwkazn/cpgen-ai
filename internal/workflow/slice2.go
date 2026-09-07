package workflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/similarity"
)

// Slice2WorkflowRevision is compiled into the binary. Persisted stage names
// are selectors only; callers cannot supply an arbitrary graph.
const Slice2WorkflowRevision = "slice2.idea.statement.similarity.v1"

type Slice2Pipeline struct {
	idea            Step[domain.GenerationRequestSnapshotV1, domain.IdeaBatch]
	statement       Step[domain.StatementInput, domain.ProblemSpec]
	similarity      Step[similarity.Request, similarity.Evidence]
	policy          similarity.DecisionPolicy
	selectionPolicy string
}

// Slice2Output is the typed chain handed to the next slice. Every field is
// validated against the preceding content contract before the value leaves
// the pipeline.
type Slice2Output struct {
	Snapshot          domain.GenerationRequestSnapshotV1 `json:"snapshot"`
	Batch             domain.IdeaBatch                   `json:"batch"`
	Selection         domain.IdeaSelection               `json:"selection"`
	StatementInput    domain.StatementInput              `json:"statement_input"`
	Problem           domain.ProblemSpec                 `json:"problem"`
	SimilarityRequest similarity.Request                 `json:"similarity_request"`
	Evidence          similarity.Evidence                `json:"evidence"`
}

func (o Slice2Output) Validate() error {
	if err := o.Snapshot.Validate(); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if err := o.Batch.Validate(); err != nil {
		return fmt.Errorf("idea batch: %w", err)
	}
	if err := o.Selection.Validate(o.Batch); err != nil {
		return fmt.Errorf("idea selection: %w", err)
	}
	if err := o.StatementInput.ValidateChain(o.Snapshot, o.Batch, o.Selection); err != nil {
		return fmt.Errorf("statement input: %w", err)
	}
	if err := o.Problem.ValidateChain(o.Snapshot, o.Batch, o.Selection); err != nil {
		return fmt.Errorf("problem spec: %w", err)
	}
	if err := o.SimilarityRequest.Validate(); err != nil {
		return fmt.Errorf("similarity request: %w", err)
	}
	requestDigest, err := o.SimilarityRequest.Digest()
	if err != nil {
		return fmt.Errorf("similarity request digest: %w", err)
	}
	if err := o.Evidence.Validate(); err != nil {
		return fmt.Errorf("similarity evidence: %w", err)
	}
	if o.Evidence.RequestDigest != requestDigest || o.Evidence.PolicyDigest != o.SimilarityRequest.PolicyDigest {
		return errors.New("similarity evidence is not bound to request")
	}
	return nil
}

// NewSlice2Pipeline creates the default policy-bound static pipeline.
func NewSlice2Pipeline(
	idea Step[domain.GenerationRequestSnapshotV1, domain.IdeaBatch],
	statement Step[domain.StatementInput, domain.ProblemSpec],
	similarityStep Step[similarity.Request, similarity.Evidence],
) (Slice2Pipeline, error) {
	policy, err := similarity.NewPolicy("similarity/v1", .5, .8, .5, .8, 1, nil)
	if err != nil {
		return Slice2Pipeline{}, err
	}
	return NewSlice2PipelineWithPolicy(idea, statement, similarityStep, policy, domain.SelectionOrdinalPolicyV1)
}

func NewSlice2PipelineWithPolicy(
	idea Step[domain.GenerationRequestSnapshotV1, domain.IdeaBatch],
	statement Step[domain.StatementInput, domain.ProblemSpec],
	similarityStep Step[similarity.Request, similarity.Evidence],
	policy similarity.DecisionPolicy,
	selectionPolicy string,
) (Slice2Pipeline, error) {
	if idea == nil || statement == nil || similarityStep == nil {
		return Slice2Pipeline{}, errors.New("all slice2 steps are required")
	}
	if err := policy.Validate(); err != nil {
		return Slice2Pipeline{}, fmt.Errorf("similarity policy: %w", err)
	}
	if selectionPolicy != domain.SelectionOrdinalPolicyV1 && selectionPolicy != domain.SelectionIdeaIDPolicyV1 {
		return Slice2Pipeline{}, errors.New("unsupported slice2 selection policy")
	}
	for name, pair := range map[string]struct {
		got  domain.StageName
		want domain.StageName
	}{
		"idea":       {idea.Name(), "idea"},
		"statement":  {statement.Name(), "statement"},
		"similarity": {similarityStep.Name(), "similarity"},
	} {
		if pair.got != pair.want {
			return Slice2Pipeline{}, fmt.Errorf("%s step has name %q, want %q", name, pair.got, pair.want)
		}
	}
	return Slice2Pipeline{idea: idea, statement: statement, similarity: similarityStep, policy: policy, selectionPolicy: selectionPolicy}, nil
}

func (p Slice2Pipeline) Validate() error {
	if p.idea == nil || p.statement == nil || p.similarity == nil {
		return errors.New("all slice2 pipeline steps are required")
	}
	if p.idea.Name() != "idea" || p.statement.Name() != "statement" || p.similarity.Name() != "similarity" {
		return errors.New("slice2 pipeline has an invalid stage name")
	}
	return p.policy.Validate()
}

func (p Slice2Pipeline) Idea() Step[domain.GenerationRequestSnapshotV1, domain.IdeaBatch] {
	return p.idea
}
func (p Slice2Pipeline) Statement() Step[domain.StatementInput, domain.ProblemSpec] {
	return p.statement
}
func (p Slice2Pipeline) Similarity() Step[similarity.Request, similarity.Evidence] {
	return p.similarity
}
func (p Slice2Pipeline) Policy() similarity.DecisionPolicy { return p.policy }

// Revalidate delegates to the exact current compiled stage. A step may expose
// DependencyRevalidator to perform a fresh provider/cache check; historical
// health data is never consulted by this method.
func (p Slice2Pipeline) Revalidate(ctx context.Context, view domain.RunView, stage domain.StageName, binding domain.BlockedCheckpoint) (bool, error) {
	var candidate any
	switch stage {
	case "idea":
		candidate = p.idea
	case "statement":
		candidate = p.statement
	case "similarity":
		candidate = p.similarity
	default:
		return false, fmt.Errorf("unsupported slice2 stage %q", stage)
	}
	revalidator, ok := candidate.(DependencyRevalidator)
	if !ok {
		return true, nil
	}
	return revalidator.Revalidate(ctx, view, binding)
}

// Run executes Idea -> Statement -> Similarity in the fixed order. It returns
// a normal AgentResult control outcome at every pause boundary, so callers can
// persist BLOCKED/REVIEW and later invoke Revalidate under the existing run
// lock before starting a fresh attempt.
func (p Slice2Pipeline) Run(ctx context.Context, view domain.RunView, snapshot domain.GenerationRequestSnapshotV1) (domain.AgentResult[Slice2Output], error) {
	var empty domain.AgentResult[Slice2Output]
	if err := p.Validate(); err != nil {
		return empty, err
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := snapshot.Validate(); err != nil {
		return empty, err
	}
	if snapshot.RequestDigest != view.RequestDigest() || snapshot.SchemaVersion != string(view.SchemaVersion()) {
		return empty, errors.New("slice2 snapshot is not bound to RunView")
	}
	ideaResult, err := p.idea.Run(ctx, view, snapshot)
	if err != nil {
		return empty, err
	}
	if control := controlResult[domain.IdeaBatch, Slice2Output](ideaResult); control != nil {
		return *control, nil
	}
	batch := *ideaResult.Value
	if err := batch.Validate(); err != nil {
		return empty, err
	}
	if batch.RequestDigest != snapshot.RequestDigest || batch.EffectiveSeed != snapshot.EffectiveSeed {
		return empty, errors.New("idea batch is not bound to request snapshot")
	}
	feasible, err := batch.OrderedFeasibleCandidateIDs(p.selectionPolicy)
	if err != nil {
		return empty, err
	}
	if len(feasible) == 0 {
		return domain.Review[Slice2Output](domain.ReviewRequest{EvidenceDigest: batch.BatchDigest, PolicyDigest: p.policy.PolicyDigest, Reason: "no feasible idea candidates"}), nil
	}
	selection, err := domain.NewIdeaSelection(snapshot.RequestDigest, batch, feasible[0], p.selectionPolicy, []string{"deterministic_selection"}, []domain.Digest{batch.BatchDigest})
	if err != nil {
		return empty, err
	}
	statementInput := domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: snapshot.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}
	if err := statementInput.ValidateChain(snapshot, batch, selection); err != nil {
		return empty, err
	}
	statementResult, err := p.statement.Run(ctx, view, statementInput)
	if err != nil {
		return empty, err
	}
	if control := controlResult[domain.ProblemSpec, Slice2Output](statementResult); control != nil {
		return *control, nil
	}
	problem := *statementResult.Value
	if err := problem.ValidateChain(snapshot, batch, selection); err != nil {
		return empty, err
	}
	projection, err := similarity.NewPackageSafeProjection(problem.Title, problem.Description, problem.RequiredConstraints, problem.Language)
	if err != nil {
		return empty, err
	}
	request, err := similarity.NewRequest(projection, p.policy.PolicyRef, p.policy.PolicyDigest, logicalSimilarityID(view, problem))
	if err != nil {
		return empty, err
	}
	similarityResult, err := p.similarity.Run(ctx, view, request)
	if err != nil {
		return empty, err
	}
	if control := controlResult[similarity.Evidence, Slice2Output](similarityResult); control != nil {
		return *control, nil
	}
	evidence := *similarityResult.Value
	if err := evidence.Validate(); err != nil {
		return empty, err
	}
	requestDigest, err := request.Digest()
	if err != nil {
		return empty, err
	}
	if evidence.RequestDigest != requestDigest || evidence.PolicyDigest != p.policy.PolicyDigest {
		return empty, errors.New("similarity evidence is stale or bound to another policy")
	}
	decision := similarity.Evaluate(p.policy, evidence)
	if err := decision.Validate(); err != nil {
		return empty, err
	}
	output := Slice2Output{Snapshot: snapshot, Batch: batch, Selection: selection, StatementInput: statementInput, Problem: problem, SimilarityRequest: request, Evidence: evidence}
	if err := output.Validate(); err != nil {
		return empty, err
	}
	switch decision.Kind {
	case similarity.DecisionAccept:
		return domain.Success(output), nil
	case similarity.DecisionNeedsReview:
		return domain.Review[Slice2Output](domain.ReviewRequest{EvidenceDigest: evidence.EvidenceDigest, PolicyDigest: p.policy.PolicyDigest, Reason: string(decision.ExplanationCode)}), nil
	case similarity.DecisionReject:
		return domain.Failure[Slice2Output](domain.PermanentFailure{Code: domain.FailurePolicyRejected, Evidence: evidence.EvidenceDigest}), nil
	case similarity.DecisionBlocked:
		return domain.Blocked[Slice2Output](blockedSimilarityCheckpoint(view, requestDigest, p.policy.PolicyDigest, evidence.EvidenceDigest)), nil
	default:
		return empty, errors.New("unknown similarity decision")
	}
}

func blockedSimilarityCheckpoint(view domain.RunView, inputDigest, policyDigest, errorDigest domain.Digest) domain.BlockedCheckpoint {
	now := time.Unix(0, 0).UTC()
	return domain.BlockedCheckpoint{RunID: view.RunID(), StageName: "similarity", StageInputDigest: inputDigest, DependencyID: "similarity-provider", DependencyDigest: inputDigest, PolicyDigest: policyDigest, ErrorDigest: errorDigest, RetryAfter: now, CreatedAt: now}
}

func logicalSimilarityID(view domain.RunView, problem domain.ProblemSpec) string {
	digest, _ := problem.Digest()
	return "similarity-" + strings.TrimPrefix(string(digest), "sha256:")[:32]
}

func controlResult[I, O any](result domain.AgentResult[I]) *domain.AgentResult[O] {
	if result.Value != nil {
		return nil
	}
	converted := domain.AgentResult[O]{}
	switch {
	case result.Retryable != nil:
		converted = domain.Retry[O](*result.Retryable)
	case result.Blocked != nil:
		converted = domain.Blocked[O](*result.Blocked)
	case result.Review != nil:
		converted = domain.Review[O](*result.Review)
	case result.Failure != nil:
		converted = domain.Failure[O](*result.Failure)
	case result.Cancellation != nil:
		converted = domain.Cancelled[O](*result.Cancellation)
	default:
		return nil
	}
	return &converted
}
