package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/agent"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestBuiltinDraftsUseDurableRepairAndLocalDomainBinding(t *testing.T) {
	snapshot, err := domain.NewGenerationRequestSnapshotV1(domain.GenerationRequestV1{SchemaVersion: domain.RequestSchemaV1, Mode: "manual", Brief: "Graphs", Language: "en", Difficulty: "hard", TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", VerificationProfile: "default", BudgetLimits: domain.BudgetLimits{MaxLLMCalls: 4, MaxMutationsPerStage: 2}}, 42)
	if err != nil {
		t.Fatal(err)
	}
	ideaInput, err := domain.NewIdeaDraftInput(snapshot, 2)
	if err != nil {
		t.Fatal(err)
	}
	outputs := llmBuiltinOutputs(t)
	var ideaDraft domain.IdeaDraftV1
	if err := json.Unmarshal(outputs["idea.draft"], &ideaDraft); err != nil {
		t.Fatal(err)
	}
	batch, err := ideaDraft.Bind(ideaInput)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := domain.NewIdeaSelection(snapshot.RequestDigest, batch, batch.Candidates[0].IdeaID, domain.SelectionOrdinalPolicyV1, []string{"deterministic_selection"}, []domain.Digest{batch.BatchDigest})
	if err != nil {
		t.Fatal(err)
	}
	statementInput, err := domain.NewStatementDraftInput(domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: snapshot.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}, snapshot, batch, selection)
	if err != nil {
		t.Fatal(err)
	}
	var statementDraft domain.StatementDraftV1
	if err := json.Unmarshal(outputs["statement.draft"], &statementDraft); err != nil {
		t.Fatal(err)
	}
	problem, err := statementDraft.Bind(statementInput, 1)
	if err != nil {
		t.Fatal(err)
	}
	solutionInput, err := domain.NewSolutionDraftInput(snapshot, problem, domain.SumBytes([]byte("similarity input")), domain.SumBytes([]byte("accepted evidence")), domain.SumBytes([]byte("accepted decision")))
	if err != nil {
		t.Fatal(err)
	}
	var solutionDraft domain.SolutionDraftV1
	if err := json.Unmarshal(outputs["solution.draft"], &solutionDraft); err != nil {
		t.Fatal(err)
	}
	solutionContent, err := solutionDraft.Bind(solutionInput)
	if err != nil {
		t.Fatal(err)
	}
	dataInput, err := domain.NewDataDraftInput(solutionInput, solutionContent, domain.SumBytes([]byte("verified samples")))
	if err != nil {
		t.Fatal(err)
	}
	for stage, input := range map[string]any{"idea": ideaInput, "statement": statementInput, "mutation": nil, "solution": solutionInput, "data": dataInput} {
		t.Run(stage, func(t *testing.T) {
			step := stage + ".draft"
			if stage == "mutation" {
				step = "idea.mutate"
			}
			cfg := llmApplicationConfig(t)
			cfg.LLM.MaxFormatRepairs = 1
			t.Setenv(cfg.LLM.APIKeyEnv, "fixture-key")
			var httpCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				content := string(outputs[step])
				if httpCalls.Add(1) == 1 {
					content = `{"private":"invalid fixture output"}`
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"id": "draft-fixture", "choices": []any{map[string]any{"message": map[string]string{"content": content}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
			}))
			defer server.Close()
			mapped, _, err := application.BuildLLMConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			mapped.Endpoint, mapped.AllowInsecureHTTP = server.URL, true
			model, err := agent.NewLangChain(mapped)
			if err != nil {
				t.Fatal(err)
			}
			prompt, schema, err := application.BuildLLMDraftPrompt(stage)
			if stage == "mutation" {
				prompt, schema, err = application.BuildIdeaMutationPrompt()
			}
			if err != nil {
				t.Fatal(err)
			}
			fixture := newCoordinatorFixtureWithStages(t, "d2", domain.BudgetLimits{MaxLLMCalls: 4, MaxLLMInputTokens: 100000, MaxLLMOutputTokens: 2000, MaxLLMCostMicroUSD: 2000, MaxArtifactBytes: 500000, MaxMutationsPerStage: 2}, []domain.StageName{"prepare", "idea", "exercise"})
			var mutationInput domain.IdeaMutationDraftInputV1
			if stage == "mutation" {
				// This is the lower-level provider/format contract fixture. It
				// consumes one generic claim without enabling a runtime route.
				core, err := domain.NewIdeaMutationCore(snapshot, batch, domain.IdeaMutationParameters{
					RunID: fixture.runID, WorkflowRevision: "slice1/v1", ConfigDigest: cfg.EffectiveDigest(),
					TriggerKind: domain.IdeaMutationSimilarity, TriggerEvidenceDigest: domain.SumBytes([]byte("fixture rejection")),
					ParentIdeaID: selection.SelectedIdeaID, MutationOrdinal: 1,
				})
				if err != nil {
					t.Fatal(err)
				}
				claim, err := core.ClaimRequest(fixture.clock.Now())
				if err != nil {
					t.Fatal(err)
				}
				grant, err := fixture.store.ClaimMutation(context.Background(), claim)
				if err != nil {
					t.Fatal(err)
				}
				intent, err := domain.NewIdeaMutationIntent(core, grant)
				if err != nil {
					t.Fatal(err)
				}
				mutationInput, err = domain.NewIdeaMutationDraftInput(snapshot, batch, intent)
				if err != nil {
					t.Fatal(err)
				}
				input = mutationInput
			}
			variables, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			open := fixture.openRequest(1001)
			open.RetryPolicy.MaxAttempts = 1
			request := port.GenerateRequest{Prompt: prompt, Schema: schema, Variables: variables, Sampling: port.SamplingPolicy{TopP: 1}, MaxOutput: port.OutputLimit{Tokens: 512, Bytes: 8192}, LogicalIdempotencyKey: open.LogicalOperationID, ProviderPolicyDigest: cfg.EffectiveDigest(), PrivacyClassification: "private"}
			plan, err := model.PlanGenerate(request)
			if err != nil {
				t.Fatal(err)
			}
			open.Provider, open.RequestDigest, open.PolicyDigest = plan.Provider, plan.RequestDigest, request.ProviderPolicyDigest
			blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "private"))
			if err != nil {
				t.Fatal(err)
			}
			calls, err := application.NewReplayableLLMCalls(fixture.store, model, blobs, fixture.clock, 100)
			if err != nil {
				t.Fatal(err)
			}
			policy, err := application.BuildFormatRepairPolicy(cfg, step)
			if err != nil {
				t.Fatal(err)
			}
			for replay := 0; replay < 2; replay++ {
				structured, err := application.NewStructuredLLMCalls(calls, policy)
				if err != nil {
					t.Fatal(err)
				}
				result, err := structured.Generate(context.Background(), open, request)
				if err != nil || result.Outcome.Value == nil {
					t.Fatalf("result=%+v error=%v", result, err)
				}
				if httpCalls.Load() != 2 || len(result.CallTraces) != 2 || len(result.Artifacts) != 2 || result.Usage != (port.Usage{InputTokens: 6, OutputTokens: 8}) {
					t.Fatalf("HTTP=%d result=%+v", httpCalls.Load(), result)
				}
				if stage == "mutation" {
					var draft domain.IdeaDraftV1
					if err := json.Unmarshal(result.Outcome.Value.Structured, &draft); err != nil {
						t.Fatal(err)
					}
					bound, err := draft.BindMutation(mutationInput)
					if err != nil || bound.BatchOrdinal != batch.BatchOrdinal+1 || bound.BatchDigest == batch.BatchDigest {
						t.Fatalf("mutation batch=%+v err=%v", bound, err)
					}
					quota, err := fixture.store.ReadMutationBudget(context.Background(), fixture.runID, "idea")
					if err != nil || quota.Claimed != 1 {
						t.Fatalf("format repair or replay consumed another content claim: %+v %v", quota, err)
					}
				} else if stage == "data" {
					var draft domain.DataDraftV1
					if err := json.Unmarshal(result.Outcome.Value.Structured, &draft); err != nil {
						t.Fatal(err)
					}
					content, err := draft.Bind(dataInput)
					if err != nil || content.ValidateInput(dataInput) != nil || len(content.Plan.Cases) != 4 {
						t.Fatalf("data=%+v %v", content, err)
					}
				} else if stage == "solution" {
					var draft domain.SolutionDraftV1
					if err := json.Unmarshal(result.Outcome.Value.Structured, &draft); err != nil {
						t.Fatal(err)
					}
					content, err := draft.Bind(solutionInput)
					if err != nil || content.ProblemSpecDigest != problem.SpecDigest || content.ValidateInput(solutionInput) != nil {
						t.Fatalf("solution content=%+v err=%v", content, err)
					}
					quota, err := fixture.store.ReadMutationBudget(context.Background(), fixture.runID, "idea")
					if err != nil || quota.Claimed != 0 {
						t.Fatalf("solution used mutation quota: %+v %v", quota, err)
					}
				} else if stage == "idea" {
					var draft domain.IdeaDraftV1
					if err := json.Unmarshal(result.Outcome.Value.Structured, &draft); err != nil {
						t.Fatal(err)
					}
					bound, err := draft.Bind(ideaInput)
					if err != nil || bound.BatchDigest != batch.BatchDigest {
						t.Fatalf("batch=%+v error=%v", bound, err)
					}
				} else {
					var draft domain.StatementDraftV1
					if err := json.Unmarshal(result.Outcome.Value.Structured, &draft); err != nil {
						t.Fatal(err)
					}
					bound, err := draft.Bind(statementInput, 1)
					if err != nil {
						t.Fatal(err)
					}
					if err := bound.ValidateChain(snapshot, batch, selection); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}
