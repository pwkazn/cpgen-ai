package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/domain"
)

func TestSimilarityRoutePlannerUsesCommittedDecisionAndCurrentMutationQuota(t *testing.T) {
	for _, test := range []struct {
		name, response string
		limit          int64
		action         application.SimilarityRouteAction
		reason         string
	}{
		{"accept", `{"provider_identity":"fixture","hits":[{"source":"fixture","external_id":"one","score":0.2}]}`, 2, application.SimilarityRouteContinue, "similarity_accepted"},
		{"review", `{"provider_identity":"fixture","hits":[{"source":"fixture","external_id":"one","score":0.6}]}`, 2, application.SimilarityRouteReview, "within_review_band"},
		{"mutate", `{"provider_identity":"fixture","hits":[{"source":"fixture","external_id":"one","score":0.9}]}`, 2, application.SimilarityRouteMutateIdea, "at_or_above_rejection_threshold"},
		{"exhausted", `{"provider_identity":"fixture","hits":[{"source":"fixture","external_id":"one","score":0.9}]}`, 0, application.SimilarityRouteReview, "idea_mutation_budget_exhausted"},
		{"insufficient", `{"provider_identity":"fixture","hits":[]}`, 2, application.SimilarityRouteRecheck, "insufficient_evidence"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSimilarityExecutorFixture(t, 3, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(test.response)) })
			request := domain.RunRequest(f.snapshot.Request)
			request.BudgetLimits.MaxMutationsPerStage = test.limit
			run, err := slice2FixtureService(t, f).Generate(context.Background(), request)
			if err != nil || run.CurrentStage != "slice2_checkpoint" {
				t.Fatalf("preview=%+v %v", run, err)
			}
			planner, err := application.NewSimilarityRoutePlanner(f.service)
			if err != nil {
				t.Fatal(err)
			}
			first, err := planner.Read(context.Background(), run.RunID)
			if err != nil || first.Validate() != nil || first.Action != test.action || first.Reason != test.reason || first.RunVersion != run.Version || first.MutationBudget.Limit != test.limit {
				t.Fatalf("plan=%+v %v", first, err)
			}
			second, err := planner.Read(context.Background(), run.RunID)
			if err != nil || second != first {
				t.Fatalf("read-only plan changed: %+v %v", second, err)
			}
			raw, err := first.CanonicalJSON()
			if err != nil {
				t.Fatal(err)
			}
			var decoded application.SimilarityRoutePlan
			if err := json.Unmarshal(raw, &decoded); err != nil || decoded != first {
				t.Fatalf("route plan did not round trip: %+v %v", decoded, err)
			}
			for name, invalid := range map[string][]byte{
				"unknown":    bytes.Replace(raw, []byte(`"rule_version":`), []byte(`"unknown":true,"rule_version":`), 1),
				"duplicate":  bytes.Replace(raw, []byte(`"rule_version":`), []byte(`"rule_version":"discarded","rule_version":`), 1),
				"case alias": bytes.Replace(raw, []byte(`"rule_version":`), []byte(`"RULE_VERSION":`), 1),
				"null quota": bytes.Replace(raw, []byte(`"claimed":0`), []byte(`"claimed":null`), 1),
			} {
				if err := json.Unmarshal(invalid, &decoded); err == nil {
					t.Fatalf("%s route fields were accepted", name)
				}
				if decoded != first {
					t.Fatalf("failed %s decode replaced the prior plan", name)
				}
			}
			current, err := f.store.GetRun(context.Background(), run.RunID)
			if err != nil || current.Version != run.Version || current.State != domain.RunNeedsReview || f.httpCalls.Load() != 2 || f.sends.Load() != 1 {
				t.Fatalf("planning executed a route: %+v %v", current, err)
			}
			bad := first
			bad.EvidenceDigest = domain.SumBytes([]byte("changed evidence"))
			if bad.Validate() == nil {
				t.Fatal("changed route evidence accepted")
			}
			if test.action == application.SimilarityRouteMutateIdea {
				for i := int64(1); i <= 2; i++ {
					_, err := f.store.ClaimMutation(context.Background(), domain.MutationClaimRequest{RunID: run.RunID, StageName: "idea", ScopeDigest: domain.SumBytes([]byte("metadata-scope")), SourceBatchDigest: first.SourceBatchDigest, Ordinal: i, LimitSnapshot: test.limit, Kind: domain.MutationMetadata, IntentDigest: domain.SumBytes([]byte{byte(i)}), At: f.clock.Now()})
					if err != nil {
						t.Fatal(err)
					}
				}
				changed, err := planner.Read(context.Background(), run.RunID)
				if err != nil || changed.Action != application.SimilarityRouteReview || changed.Reason != "idea_mutation_budget_exhausted" || changed.PlanDigest == first.PlanDigest || changed.DecisionDigest != first.DecisionDigest || changed.MutationBudget.Claimed != 2 {
					t.Fatalf("shared quota ignored: %+v %v", changed, err)
				}
			}
			if test.action == application.SimilarityRouteRecheck {
				_, err := f.store.RequestCancel(context.Background(), domain.CancelRequest{ID: "control_00000000000000000000000000002605", RunID: run.RunID, ExpectedRunVersion: run.Version, Reason: "cancel before route", IdempotencyKey: coordinatorID("cancel", "route-plan"), At: f.clock.Now()})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := planner.Read(context.Background(), run.RunID); err == nil {
					t.Fatal("pending cancellation allowed route planning")
				}
			}
		})
	}
}

func TestSimilarityRoutePlannerRejectsUncommittedStage(t *testing.T) {
	f := newSimilarityExecutorFixture(t, 3)
	if err := f.runGuard.Close(); err != nil {
		t.Fatal(err)
	}
	planner, err := application.NewSimilarityRoutePlanner(f.service)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := planner.Read(context.Background(), f.runID); err == nil || f.httpCalls.Load() != 0 || f.sends.Load() != 0 {
		t.Fatalf("uncommitted stage planned or dispatched: %v", err)
	}
}

func TestSimilarityRouteMutationCoreBindsDurableClaimWithoutExecuting(t *testing.T) {
	f := newSimilarityExecutorFixture(t, 3, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"provider_identity":"fixture","hits":[{"source":"fixture","external_id":"one","score":0.9}]}`))
	})
	ctx := context.Background()
	request := domain.RunRequest(f.snapshot.Request)
	request.BudgetLimits.MaxMutationsPerStage = 2
	run, err := slice2FixtureService(t, f).Generate(ctx, request)
	if err != nil || run.CurrentStage != "slice2_checkpoint" {
		t.Fatalf("preview=%+v %v", run, err)
	}
	planner, err := application.NewSimilarityRoutePlanner(f.service)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planner.Read(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	content, err := f.service.ReadCommitted(ctx, run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	core, err := plan.MutationCore(content.Statement.Idea.Snapshot, content.Statement.Idea.Batch)
	if err != nil || core.TriggerEvidenceDigest != plan.DecisionDigest || core.StageScopeDigest != plan.MutationScopeDigest {
		t.Fatalf("core=%+v %v", core, err)
	}
	// This fixture exercises the low-level ledger only. The production preview
	// never consumes the plan; checked route authorization remains separate.
	command, err := core.ClaimRequest(f.clock.Now())
	if err != nil {
		t.Fatal(err)
	}
	grant, err := f.store.ClaimMutation(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := f.store.ClaimMutation(ctx, command)
	if err != nil || replayed != grant {
		t.Fatalf("claim replay=%+v %v", replayed, err)
	}
	intent, err := domain.NewIdeaMutationIntent(core, grant)
	if err != nil {
		t.Fatalf("SQLite grant does not bind to canonical core: %v", err)
	}
	input, err := domain.NewIdeaMutationDraftInput(content.Statement.Idea.Snapshot, content.Statement.Idea.Batch, intent)
	if err != nil {
		t.Fatal(err)
	}
	if input.Intent.Grant.ClaimID != grant.ClaimID || input.BatchOrdinal != content.Statement.Idea.Batch.BatchOrdinal+1 {
		t.Fatal("authorized draft lost durable claim or source")
	}
	currentPlan, err := planner.Read(ctx, run.RunID)
	if err != nil || currentPlan.MutationBudget.Claimed != 1 || currentPlan.NextMutationOrdinal != 2 || currentPlan.DecisionDigest != plan.DecisionDigest {
		t.Fatalf("new quota plan=%+v %v", currentPlan, err)
	}
	current, err := f.store.GetRun(ctx, run.RunID)
	if err != nil || current.Version != run.Version || current.State != domain.RunNeedsReview || f.httpCalls.Load() != 2 || f.sends.Load() != 1 {
		t.Fatalf("contract construction executed a route: %+v %v", current, err)
	}
	badSource := content.Statement.Idea.Batch
	badSource.BatchDigest = domain.SumBytes([]byte("foreign source"))
	if _, err := plan.MutationCore(content.Statement.Idea.Snapshot, badSource); err == nil {
		t.Fatal("crossed committed source admitted")
	}
}
