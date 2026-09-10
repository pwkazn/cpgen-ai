package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func rejectedIdeaResponse(t *testing.T) http.HandlerFunc {
	t.Helper()
	var draft domain.IdeaDraftV1
	if err := json.Unmarshal(llmBuiltinOutputs(t)["idea.draft"], &draft); err != nil {
		t.Fatal(err)
	}
	for i := range draft.Candidates {
		draft.Candidates[i].FeasibilityStatus = "REJECTED"
		draft.Candidates[i].FeasibilityReasons = []string{"recorded_constraint_failure"}
	}
	raw, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	return func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "rejected-candidates",
			"choices": []any{map[string]any{"message": map[string]string{"content": string(raw)}}},
			"usage":   map[string]int{"prompt_tokens": 3, "completion_tokens": 4},
		})
	}
}

func TestIdeaCandidatesRetainRejectedBatchAcrossDatabaseReopen(t *testing.T) {
	ctx := context.Background()
	f := newGenerationExecutorFixture(t, 4, false, rejectedIdeaResponse(t))
	view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	collected, err := f.executor.CollectIdeaCandidates(ctx, view, f.snapshot)
	if err != nil || collected.Outcome.Value == nil || len(collected.Occurrences) != 1 {
		t.Fatalf("collection=%+v err=%v", collected, err)
	}
	batch := *collected.Outcome.Value
	evidence, err := domain.NoFeasibleIdeaEvidence(batch)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.executor.Reader().ReadIdeaCandidates(ctx, f.runID); err == nil {
		t.Fatal("uncommitted provider receipt became candidate proof")
	}
	// Exercise the collection primitive's storage boundary. This fixture's
	// successor digest is deliberately not a StatementInput; no production
	// graph is changed or permitted to generate downstream content here.
	f.finish(t, view, batch.BatchDigest, "statement", batch.BatchDigest, collected.Occurrences)
	verify := func(reader *application.GenerationReader) {
		t.Helper()
		got, err := reader.ReadIdeaCandidates(ctx, f.runID)
		if err != nil || got.Batch.BatchDigest != batch.BatchDigest || got.Snapshot.SnapshotDigest != f.snapshot.SnapshotDigest {
			t.Fatalf("candidate proof=%+v err=%v", got, err)
		}
		gotEvidence, err := domain.NoFeasibleIdeaEvidence(got.Batch)
		if err != nil || gotEvidence != evidence {
			t.Fatalf("rejected-batch evidence changed: %s %v", gotEvidence, err)
		}
		if _, err := reader.ReadIdea(ctx, f.runID); err == nil {
			t.Fatal("rejected batch admitted as Statement input")
		}
		if f.httpCalls.Load() != 1 {
			t.Fatalf("candidate recovery dispatched %d provider requests", f.httpCalls.Load())
		}
	}
	verify(f.executor.Reader())
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.OpenWithClock(ctx, sqlite.Config{Path: f.path, BusyTimeout: time.Second, MaxReaders: 4}, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	executorConfig := f.executorConfig
	executorConfig.Store = store
	executor, err := application.NewGenerationExecutor(executorConfig)
	if err != nil {
		t.Fatal(err)
	}
	verify(executor.Reader())
}

func TestIdeaCandidatesDoNotChangePreviewNoFeasibleReview(t *testing.T) {
	ctx := context.Background()
	generation := newGenerationExecutorFixtureWithWorkflow(t, 4, false, workflow.Slice2CheckpointWorkflowRevision, 3, rejectedIdeaResponse(t))
	f := similarityExecutorFixtureFromGeneration(t, generation)
	service := slice2FixtureService(t, f)
	seed := f.snapshot.EffectiveSeed
	request := domain.RunRequest(f.snapshot.Request)
	request.Seed = &seed
	result, err := service.Generate(ctx, request)
	if err != nil || result.State != domain.RunNeedsReview || result.CurrentStage != "idea" || result.ActiveStartedAt != nil {
		t.Fatalf("preview=%+v err=%v", result, err)
	}
	if _, err := f.executor.Reader().ReadIdeaCandidates(ctx, result.RunID); err == nil {
		t.Fatal("legacy review committed a successful candidate collection")
	}
	resumed, err := service.Resume(ctx, result.RunID)
	if err != nil || resumed.Version != result.Version || f.httpCalls.Load() != 1 || f.sends.Load() != 0 {
		t.Fatalf("review resume=%+v err=%v HTTP=%d/%d", resumed, err, f.httpCalls.Load(), f.sends.Load())
	}
}
