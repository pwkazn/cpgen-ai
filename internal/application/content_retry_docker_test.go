package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

var errContentRetryGap = errors.New("fixture interruption after atomic content retry")

type contentRetryGapStore struct {
	*sqlite.Store
	fired atomic.Bool
}

func (s *contentRetryGapStore) FinishContentRetry(ctx context.Context, command domain.FinishContentRetryCommand) (domain.RunSnapshot, error) {
	result, err := s.Store.FinishContentRetry(ctx, command)
	if err == nil && result.State == domain.RunRunning && s.fired.CompareAndSwap(false, true) {
		return result, errContentRetryGap
	}
	return result, err
}

func TestContentRetryDockerRegeneratesAndReverifiesAfterRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()
	base := newDockerSandboxTestConfig(t, ctx)
	for _, mode := range []string{"sample", "compile", "data"} {
		t.Run(mode, func(t *testing.T) {
			outputs := dataDockerOutputs(t, "pass")
			sequence := []string{"idea.draft", "statement.draft", "bad", "solution.draft", "data.draft"}
			target, wantCalls := domain.StageName("solution"), int32(5)
			switch mode {
			case "compile":
				outputs["bad"] = solutionDockerOutputs(t, "compile_error")["solution.draft"]
			case "sample":
				var statement domain.StatementDraftV1
				if err := json.Unmarshal(outputs["statement.draft"], &statement); err != nil {
					t.Fatal(err)
				}
				statement.Samples[1].Output = "42\n"
				outputs["bad"], _ = json.Marshal(statement)
				sequence = []string{"idea.draft", "bad", "solution.draft", "statement.draft", "solution.draft", "data.draft"}
				target, wantCalls = "statement", 6
			case "data":
				var draft domain.DataDraftV1
				if err := json.Unmarshal(outputs["data.draft"], &draft); err != nil {
					t.Fatal(err)
				}
				draft.ValidatorCode = "int main( {\n"
				outputs["bad"], _ = json.Marshal(draft)
				sequence = []string{"idea.draft", "statement.draft", "solution.draft", "bad", "data.draft"}
				target = "data"
			}
			var calls atomic.Int32
			f := similarityExecutorFixtureFromGeneration(t, newGenerationExecutorFixtureWithWorkflow(t, 8, false, workflow.RetryingGenerationRevision, 3, func(w http.ResponseWriter, _ *http.Request) {
				index := int(calls.Add(1)) - 1
				if index >= len(sequence) {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]string{"content": string(outputs[sequence[index]])}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
			}))
			gap := &contentRetryGapStore{Store: f.store}
			g := f.executorConfig
			g.Store, g.Clock = gap, clock.Real{}
			generation, err := application.NewGenerationExecutor(g)
			if err != nil {
				t.Fatal(err)
			}
			config := f.config
			config.Generation, config.WorkflowRevision = generation, workflow.RetryingGenerationRevision
			evidence, err := application.NewSimilarityExecutor(config)
			if err != nil {
				t.Fatal(err)
			}
			_, frozen, err := f.store.RunViewDocuments(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			newService := func() *application.LocalRunService {
				service, err := application.NewGenerationRunService(application.GenerationRunConfig{Store: gap, Blobs: g.Blobs, Clock: g.Clock, Locks: g.Locks, Generation: generation, Similarity: evidence, Reviews: gap, EffectiveConfigJSON: frozen, SolutionSandbox: &base})
				if err != nil {
					t.Fatal(err)
				}
				return service
			}
			request := domain.RunRequest(f.snapshot.Request)
			request.BudgetLimits.MaxSandboxCreates = 256
			result, err := newService().Generate(ctx, request)
			if !errors.Is(err, errContentRetryGap) {
				t.Fatalf("did not reach committed retry: %+v %v", result, err)
			}
			persisted, err := gap.GetRun(ctx, result.RunID)
			if err != nil || persisted.State != domain.RunRunning || persisted.CurrentStage != target {
				t.Fatalf("retry target=%+v %v", persisted, err)
			}
			result, err = newService().Resume(ctx, result.RunID)
			if err != nil || result.State != domain.RunReady || calls.Load() != wantCalls {
				t.Fatalf("regenerated run=%+v calls=%d %v", result, calls.Load(), err)
			}
			wantSimilarity := int32(1)
			if mode == "sample" {
				wantSimilarity = 2
			}
			if f.sends.Load() != wantSimilarity {
				t.Fatalf("similarity did not revalidate new statement: %d", f.sends.Load())
			}
			attempt, err := gap.CurrentStageAttempt(ctx, result.RunID, target)
			if err != nil || attempt.Ordinal != 2 || attempt.State != domain.StageAttemptSucceeded {
				t.Fatalf("regeneration attempt=%+v %v", attempt, err)
			}
			budget, err := gap.BudgetSnapshot(ctx, result.RunID)
			if err != nil || budget.Remaining[domain.BudgetLLMCalls] != 8-int64(wantCalls) {
				t.Fatalf("retry budget=%+v %v", budget, err)
			}
		})
	}
}
