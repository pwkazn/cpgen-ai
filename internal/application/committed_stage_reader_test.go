package application_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"cpgen/internal/domain"
)

func TestCommittedLLMStageReaderRequiresCurrentSuccessfulOutput(t *testing.T) {
	f := newStructuredLLMFixture(t, false)
	if _, err := f.store.ReadCommittedLLMStage(context.Background(), f.runID, "prepare"); err == nil {
		t.Fatal("running stage exposed a committed output")
	}
	result, err := f.structured.Generate(context.Background(), f.open, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ReadCommittedLLMStage(context.Background(), f.runID, "prepare"); err == nil {
		t.Fatal("unattached private receipts exposed a committed output")
	}
	commitStructuredSource(t, f, result)
	stage, err := f.store.ReadCommittedLLMStage(context.Background(), f.runID, "prepare")
	if err != nil || stage.Attempt.AttemptID != f.attemptID || stage.Attempt.State != domain.StageAttemptSucceeded || len(stage.Artifacts) != 2 || *stage.Attempt.OutputDigest != domain.SumBytes(result.Outcome.Value.Structured) {
		t.Fatalf("committed=%+v err=%v", stage, err)
	}
	for _, artifact := range stage.Artifacts {
		if artifact.Kind != domain.PendingOccurrenceNewWrite || artifact.OccurrenceID != artifact.Source.OccurrenceID || artifact.CurrentCallRecordID != artifact.Source.CallRecordID || artifact.Source.RunID != f.runID || artifact.Source.Digest != artifact.Blob.Blob.Digest {
			t.Fatalf("source bindings=%+v", artifact)
		}
	}
	calls, err := f.store.ReadAttemptLLMCalls(context.Background(), f.runID, "prepare", f.attemptID)
	if err != nil || len(calls) != 2 {
		t.Fatalf("original/repair history=%+v err=%v", calls, err)
	}
	for _, call := range calls {
		if call.RunID != f.runID || call.StageName != "prepare" || call.AttemptID != f.attemptID || call.Kind != domain.CallLLMGenerate || call.Provider == "private-blob" {
			t.Fatalf("history escaped its provider-call scope: %+v", call)
		}
	}
	if wrong, err := f.store.ReadAttemptLLMCalls(context.Background(), f.runID, "exercise", f.attemptID); err != nil || len(wrong) != 0 {
		t.Fatalf("wrong scope returned provider identities: %+v %v", wrong, err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE stage_records SET output_digest=? WHERE run_id=? AND stage_name='prepare'`, domain.SumBytes([]byte("substituted output")), f.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ReadCommittedLLMStage(context.Background(), f.runID, "prepare"); err == nil {
		t.Fatal("stage output no longer matching its committed attempt was accepted")
	}
	if _, err := db.Exec(`UPDATE stage_records SET state='PENDING',output_digest=NULL WHERE run_id=? AND stage_name='prepare'`, f.runID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ReadCommittedLLMStage(context.Background(), f.runID, "prepare"); err == nil {
		t.Fatal("invalidated stage resurrected its previous successful attempt")
	}
}

func TestCommittedLLMStageReaderKeepsCacheSourceAndCurrentAttemptDistinct(t *testing.T) {
	f := newStructuredLLMOptionsFixture(t, structuredLLMOptions{stages: []domain.StageName{"prepare", "exercise", "finish"}})
	cache := structuredCacheService(t, f)
	result, err := f.structured.Generate(context.Background(), f.open, f.request)
	if err != nil {
		t.Fatal(err)
	}
	open, request := commitStructuredSource(t, f, result)
	key, err := cache.Put(context.Background(), f.open, f.request)
	if err != nil {
		t.Fatal(err)
	}
	hit, err := cache.Reuse(context.Background(), open, request)
	if err != nil || !hit.Hit {
		t.Fatalf("hit=%+v err=%v", hit, err)
	}
	output := domain.SumBytes(hit.Outcome.Value.Structured)
	finish := domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 4, StageName: "exercise", AttemptID: open.AttemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "finish", NextInputDigest: &output, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceCacheReuse, CacheReuse: &hit.Reuses[0]}}, IdempotencyKey: "finish_00000000000000000000000000001401", At: f.clock.Now()}
	if _, err := f.store.FinishStage(context.Background(), finish); err != nil {
		t.Fatal(err)
	}
	// A committed use no longer depends on the mutable cache lookup index.
	if err := f.store.Invalidate(context.Background(), key, domain.InvalidationManual); err != nil {
		t.Fatal(err)
	}
	stage, err := f.store.ReadCommittedLLMStage(context.Background(), f.runID, "exercise")
	if err != nil || stage.Attempt.AttemptID != open.AttemptID || len(stage.Artifacts) != 1 {
		t.Fatalf("stage=%+v err=%v", stage, err)
	}
	artifact := stage.Artifacts[0]
	if artifact.Kind != domain.PendingOccurrenceCacheReuse || artifact.CurrentCallRecordID != open.ID || artifact.Source.CallRecordID != hit.Reuses[0].SourceCallRecordID || artifact.Source.OccurrenceID != hit.Reuses[0].SourceOccurrenceID || artifact.OccurrenceID == artifact.Source.OccurrenceID || artifact.CurrentCallRecordID == artifact.Source.CallRecordID {
		t.Fatalf("cache occurrence source=%+v", artifact)
	}
	if f.httpCalls.Load() != 2 {
		t.Fatal("committed receipt read repeated provider work")
	}
}
