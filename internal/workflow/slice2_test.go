package workflow_test

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/similarity"
	"cpgen/internal/workflow"
)

type slice2IdeaStep struct {
	batch domain.IdeaBatch
}

func (slice2IdeaStep) Name() domain.StageName { return "idea" }

func (s slice2IdeaStep) Run(_ context.Context, _ domain.RunView, input domain.GenerationRequestSnapshotV1) (domain.AgentResult[domain.IdeaBatch], error) {
	if input.RequestDigest != s.batch.RequestDigest {
		return domain.AgentResult[domain.IdeaBatch]{}, errors.New("fixture request digest mismatch")
	}
	return domain.Success(s.batch), nil
}

type slice2StatementStep struct {
	snapshot domain.GenerationRequestSnapshotV1
	batch    domain.IdeaBatch
}

func (slice2StatementStep) Name() domain.StageName { return "statement" }

func (s slice2StatementStep) Run(_ context.Context, _ domain.RunView, input domain.StatementInput) (domain.AgentResult[domain.ProblemSpec], error) {
	selection, err := domain.NewIdeaSelection(s.snapshot.RequestDigest, s.batch, input.SelectedIdeaID, domain.SelectionOrdinalPolicyV1, []string{"deterministic_selection"}, []domain.Digest{s.batch.BatchDigest})
	if err != nil {
		return domain.AgentResult[domain.ProblemSpec]{}, err
	}
	problem, err := domain.NewProblemSpec(input, s.snapshot, s.batch, selection, domain.ProblemSpec{
		Revision:    1,
		Title:       "Shortest paths",
		Description: "Find the shortest distance from the first vertex to every other vertex.",
		Input:       domain.ProblemIO{Description: "A graph and its vertex count.", Fields: []string{"n", "edges"}},
		Output:      domain.ProblemIO{Description: "The shortest distances.", Fields: []string{"distances"}},
		Samples:     []domain.ProblemSample{{Input: "2 1", Output: "0 1", Explanation: "The edge connects the two vertices."}},
	})
	if err != nil {
		return domain.AgentResult[domain.ProblemSpec]{}, err
	}
	return domain.Success(problem), nil
}

type slice2SimilarityStep struct {
	blocked bool
}

func (s slice2SimilarityStep) Name() domain.StageName { return "similarity" }

func (s slice2SimilarityStep) Run(_ context.Context, _ domain.RunView, request similarity.Request) (domain.AgentResult[similarity.Evidence], error) {
	var hits []similarity.Hit
	if !s.blocked {
		hit, err := similarity.NewHit("fixture-index", "problem-1", "fixture title", .2, "https://example.test/problems/1")
		if err != nil {
			return domain.AgentResult[similarity.Evidence]{}, err
		}
		hits = []similarity.Hit{hit}
	}
	evidence, err := similarity.NewEvidence(request, "fixture-similarity", hits, time.Unix(1, 0).UTC(), similarity.Usage{InputTokens: 3, OutputTokens: 1}, similarity.CacheProvenance{Kind: similarity.CacheLive}, domain.CallTrace{LogicalOperationID: "similarity-fixture", DispatchKind: domain.DispatchNone})
	if err != nil {
		return domain.AgentResult[similarity.Evidence]{}, err
	}
	return domain.Success(evidence), nil
}

type revalidatingSimilarityStep struct {
	slice2SimilarityStep
	healthy atomic.Bool
	calls   atomic.Int32
}

func (s *revalidatingSimilarityStep) Revalidate(_ context.Context, _ domain.RunView, binding domain.BlockedCheckpoint) (bool, error) {
	s.calls.Add(1)
	if binding.StageName != "similarity" || binding.DependencyID != "similarity-provider" {
		return false, nil
	}
	return s.healthy.Load(), nil
}

func newSlice2Fixture(t *testing.T, blocked bool) (domain.GenerationRequestSnapshotV1, workflow.Slice2Pipeline, domain.RunView) {
	t.Helper()
	seed := int64(42)
	request := domain.GenerationRequestV1{
		SchemaVersion:         domain.RequestSchemaV1,
		Mode:                  domain.RequestModeManual,
		Brief:                 "Generate a graph problem",
		Tags:                  []string{"graphs"},
		NormalizedTags:        []string{"graphs"},
		Language:              "en",
		Difficulty:            "medium",
		RequiredFeatures:      []string{"connected"},
		ForbiddenFeatures:     []string{"interactive"},
		TimeLimitMilliseconds: 1000,
		MemoryLimitMegabytes:  64,
		SolutionLanguage:      "cpp",
		Seed:                  &seed,
		VerificationProfile:   "default",
		ExportTargets:         []string{"internal"},
		BudgetLimits:          domain.BudgetLimits{MaxLLMCalls: 4, MaxSimilarityCalls: 2},
	}
	snapshot, err := domain.NewGenerationRequestSnapshotV1(request, seed)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := domain.NewIdeaBatch(snapshot, 2, domain.GenerationPolicyV1, []domain.IdeaCandidate{
		{CandidateOrdinal: 1, AbstractTask: "A tree query", IntendedAlgorithm: "tree traversal", TargetComplexity: "O(n)", FeasibilityStatus: "FEASIBLE"},
		{CandidateOrdinal: 0, AbstractTask: "A graph traversal", IntendedAlgorithm: "BFS", TargetComplexity: "O(n+m)", FeasibilityStatus: "FEASIBLE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	idea := slice2IdeaStep{batch: batch}
	statement := slice2StatementStep{snapshot: snapshot, batch: batch}
	similarityStep := slice2SimilarityStep{blocked: blocked}
	pipeline, err := workflow.NewSlice2Pipeline(idea, statement, similarityStep)
	if err != nil {
		t.Fatal(err)
	}
	view, err := domain.NewRunView(domain.RunViewData{
		RunID:            domain.RunID("run_0123456789abcdef0123456789abcdef"),
		WorkflowRevision: workflow.Slice2WorkflowRevision,
		SchemaVersion:    domain.SchemaVersion(snapshot.SchemaVersion),
		RequestDigest:    snapshot.RequestDigest,
		ConfigDigest:     domain.SumBytes([]byte("config")),
		WorkflowDigest:   domain.SumBytes([]byte(workflow.Slice2WorkflowRevision)),
		State:            domain.RunRunning,
		CurrentStage:     "idea",
		Version:          1,
		Budget:           domain.BudgetSnapshot{Limits: request.BudgetLimits},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot, pipeline, view
}

func TestSlice2PipelineRunsTypedStagesDeterministically(t *testing.T) {
	snapshot, pipeline, view := newSlice2Fixture(t, false)
	first, err := pipeline.Run(context.Background(), view, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pipeline.Run(context.Background(), view, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("first result: %v", err)
	}
	if err := second.Validate(); err != nil {
		t.Fatalf("second result: %v", err)
	}
	if first.Value == nil || second.Value == nil {
		t.Fatalf("expected ACCEPT success, got %s and %s", first.Outcome(), second.Outcome())
	}
	if !reflect.DeepEqual(*first.Value, *second.Value) {
		t.Fatal("same snapshot produced different typed output")
	}
	if err := first.Value.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSlice2PipelineRevalidatesFreshDependencyAfterBlocked(t *testing.T) {
	snapshot, _, view := newSlice2Fixture(t, true)
	idea := slice2IdeaStep{}
	// Rebuild the deterministic chain used by the fixture so the custom
	// revalidator can be installed without weakening the typed stage boundary.
	batch, err := domain.NewIdeaBatch(snapshot, 2, domain.GenerationPolicyV1, []domain.IdeaCandidate{
		{CandidateOrdinal: 1, AbstractTask: "A tree query", IntendedAlgorithm: "tree traversal", TargetComplexity: "O(n)", FeasibilityStatus: "FEASIBLE"},
		{CandidateOrdinal: 0, AbstractTask: "A graph traversal", IntendedAlgorithm: "BFS", TargetComplexity: "O(n+m)", FeasibilityStatus: "FEASIBLE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	idea.batch = batch
	statement := slice2StatementStep{snapshot: snapshot, batch: batch}
	step := &revalidatingSimilarityStep{slice2SimilarityStep: slice2SimilarityStep{blocked: true}}
	pipeline, err := workflow.NewSlice2Pipeline(idea, statement, step)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(context.Background(), view, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if result.Blocked == nil || result.Outcome() != "BLOCKED" {
		t.Fatalf("expected BLOCKED similarity result, got %s", result.Outcome())
	}
	if err := result.Validate(); err != nil {
		t.Fatal(err)
	}
	revalidateView, err := domain.NewRunView(domain.RunViewData{
		RunID: view.RunID(), AttemptID: view.AttemptID(), WorkflowRevision: view.WorkflowRevision(),
		SchemaVersion: view.SchemaVersion(), RequestDigest: view.RequestDigest(), ConfigDigest: view.ConfigDigest(),
		WorkflowDigest: view.WorkflowDigest(), State: domain.RunBlocked, CurrentStage: "similarity", Version: view.Version(),
		Budget: view.Budget(), RequestJSON: view.RequestJSON(), ConfigJSON: view.ConfigJSON(), CommittedArtifacts: view.CommittedArtifacts(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := pipeline.Revalidate(context.Background(), revalidateView, "similarity", *result.Blocked); err != nil || ok {
		t.Fatalf("unhealthy dependency revalidation = %v, %v", ok, err)
	}
	step.healthy.Store(true)
	if ok, err := pipeline.Revalidate(context.Background(), revalidateView, "similarity", *result.Blocked); err != nil || !ok {
		t.Fatalf("healthy dependency revalidation = %v, %v", ok, err)
	}
	if got := step.calls.Load(); got != 2 {
		t.Fatalf("revalidation calls = %d, want 2 fresh calls", got)
	}
}
