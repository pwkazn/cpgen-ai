package application

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestContentRetryCoordinatorContinuesThenStopsAtPersistentLimit(t *testing.T) {
	for _, revision := range []string{workflow.GenerationRevision, workflow.RetryingGenerationRevision} {
		t.Run(revision, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
			store, err := sqlite.OpenWithClock(ctx, sqlite.Config{Path: filepath.Join(t.TempDir(), "retry.db"), BusyTimeout: time.Second, MaxReaders: 2}, clock.NewFake(now))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			graph, err := newCompiledRunGraph(revision)
			if err != nil {
				t.Fatal(err)
			}
			limits := domain.BudgetLimits{MaxActiveTimeMilliseconds: 60000}
			request, err := (domain.GenerationRequestV1{SchemaVersion: domain.RequestSchemaV1, Mode: domain.RequestModeManual, Brief: "retry", Tags: []string{"graphs"}, NormalizedTags: []string{"graphs"}, Language: "en", Difficulty: "hard", TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", VerificationProfile: "default", ExportTargets: []string{"internal"}, BudgetLimits: limits}).CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			config := []byte(`{"test":"retry"}`)
			run, err := store.CreateRun(ctx, domain.CreateRunRequest{RunID: "run_00000000000000000000000000001901", SubmittedRequestJSON: request, SubmittedRequestDigest: domain.SumBytes(request), RedactedEffectiveConfigJSON: config, RedactedEffectiveConfigDigest: domain.SumBytes(config), WorkflowRevision: revision, WorkflowDigest: domain.SumBytes([]byte(revision)), SchemaVersion: domain.RequestSchemaV1, StageSequence: graph.stages, BudgetLimits: domain.BudgetLimits{MaxActiveTimeMilliseconds: 60000}, CreatedAt: now, IdempotencyKey: "create_00000000000000000000000000001901"})
			if err != nil {
				t.Fatal(err)
			}
			service := &LocalRunService{runtime: store, graph: graph, clock: clock.NewFake(now)}
			calls := 0
			result, err := graph.run(ctx, run, func(ctx context.Context, current domain.RunSnapshot) (domain.RunSnapshot, error) {
				calls++
				if calls > 3 {
					t.Fatal("unbounded retry")
				}
				input, err := store.ReadStageInputDigest(ctx, current.RunID, current.CurrentStage)
				if err != nil {
					return current, err
				}
				attempt, err := store.BeginStage(ctx, domain.BeginStageCommand{RunID: current.RunID, ExpectedRunVersion: current.Version, StageName: current.CurrentStage, AttemptID: domain.AttemptID(fmt.Sprintf("attempt_%032x", calls)), InputDigest: input, At: now, IdempotencyKey: fmt.Sprintf("begin_%032x", calls)})
				if err != nil {
					return current, err
				}
				current, err = store.GetRun(ctx, current.RunID)
				if err != nil {
					return current, err
				}
				return service.finishOutcome(ctx, current, attempt, input, domain.Review[any](domain.ReviewRequest{Reason: "no_feasible_idea_candidates", EvidenceDigest: domain.SumBytes([]byte(fmt.Sprint(calls))), PolicyDigest: current.ConfigDigest}))
			})
			wantCalls := 1
			if revision == workflow.RetryingGenerationRevision {
				wantCalls = 3
			}
			if err != nil || calls != wantCalls || result.State != domain.RunNeedsReview || result.CurrentStage != "idea" {
				t.Fatalf("result=%+v calls=%d err=%v", result, calls, err)
			}
		})
	}
}
