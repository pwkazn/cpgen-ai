package application_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/domain"
)

func TestIdeaBatchOutputRetainsTypedBytesAndSettlesAtStageCommit(t *testing.T) {
	for _, repaired := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "format-repair"}[repaired], func(t *testing.T) {
			f := newGenerationExecutorFixture(t, 4, repaired)
			view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
			result, err := f.executor.CollectIdeaCandidatesWithOutput(context.Background(), view, f.snapshot)
			if err != nil || result.Outcome.Value == nil {
				t.Fatalf("collection=%+v err=%v", result, err)
			}
			wantHTTP, wantOccurrences := int32(1), 2
			if repaired {
				wantHTTP, wantOccurrences = 2, 3
			}
			if f.httpCalls.Load() != wantHTTP || len(result.Occurrences) != wantOccurrences {
				t.Fatalf("HTTP=%d occurrences=%d", f.httpCalls.Load(), len(result.Occurrences))
			}
			output := result.Occurrences[len(result.Occurrences)-1].NewWrite
			if output == nil || output.Role != domain.ArtifactOutput || output.Blob.Digest == result.Outcome.Value.BatchDigest {
				t.Fatalf("missing independently hashed typed output: %+v", output)
			}
			reader, err := f.executorConfig.Blobs.OpenVerified(context.Background(), output.Blob)
			if err != nil {
				t.Fatal(err)
			}
			raw, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if err := errors.Join(readErr, closeErr); err != nil {
				t.Fatal(err)
			}
			var batch domain.IdeaBatch
			if err := json.Unmarshal(raw, &batch); err != nil || batch.BatchDigest != result.Outcome.Value.BatchDigest {
				t.Fatalf("typed batch=%+v err=%v", batch, err)
			}
			reconstructed, err := application.NewGenerationExecutor(f.executorConfig)
			if err != nil {
				t.Fatal(err)
			}
			replayed, err := reconstructed.CollectIdeaCandidatesWithOutput(context.Background(), view, f.snapshot)
			if err != nil || replayed.Occurrences[len(replayed.Occurrences)-1].NewWrite.WriterTokenID != output.WriterTokenID || f.httpCalls.Load() != wantHTTP {
				t.Fatalf("publication replay=%+v err=%v", replayed, err)
			}
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var state string
			if err := db.QueryRow(`SELECT state FROM call_records WHERE logical_operation_id LIKE 'idea-batch-output:%'`).Scan(&state); err != nil || state != "PREPARED" {
				t.Fatalf("output settled before attachment: %s %v", state, err)
			}
			f.finish(t, view, batch.BatchDigest, "statement", batch.BatchDigest, result.Occurrences)
			if err := db.QueryRow(`SELECT state FROM call_records WHERE logical_operation_id LIKE 'idea-batch-output:%'`).Scan(&state); err != nil || state != "TERMINAL" {
				t.Fatalf("typed output not settled atomically with attachment: %s %v", state, err)
			}
			var charged int64
			if err := db.QueryRow(`SELECT reservation.settled_value FROM budget_reservations reservation JOIN artifact_occurrences occurrence ON occurrence.reservation_id=reservation.reservation_id WHERE occurrence.writer_token_id=?`, output.WriterTokenID).Scan(&charged); err != nil || charged != output.PhysicalNewBytes {
				t.Fatalf("typed output charge=%d expected=%d err=%v", charged, output.PhysicalNewBytes, err)
			}
			if _, err := f.executor.Reader().ReadIdeaCandidates(context.Background(), f.runID); err == nil {
				t.Fatal("historical provider-only reader silently accepted the new output family")
			}
		})
	}
}

func TestIdeaBatchOutputRecoversSealedPublicationWithoutAnotherProviderRequest(t *testing.T) {
	f := newGenerationExecutorFixture(t, 4, false)
	store := &ideaOutputPublicationFailure{GenerationExecutionStore: f.store}
	configuration := f.executorConfig
	configuration.Store = store
	executor, err := application.NewGenerationExecutor(configuration)
	if err != nil {
		t.Fatal(err)
	}
	view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	if _, err := executor.CollectIdeaCandidatesWithOutput(context.Background(), view, f.snapshot); err == nil || f.httpCalls.Load() != 1 {
		t.Fatalf("sealed failure was not retained: %v HTTP=%d", err, f.httpCalls.Load())
	}
	executor, err = application.NewGenerationExecutor(configuration)
	if err != nil {
		t.Fatal(err)
	}
	result, err := executor.CollectIdeaCandidatesWithOutput(context.Background(), view, f.snapshot)
	if err != nil || result.Outcome.Value == nil || len(result.Occurrences) != 2 || f.httpCalls.Load() != 1 {
		t.Fatalf("sealed output recovery=%+v err=%v HTTP=%d", result, err, f.httpCalls.Load())
	}
	f.finish(t, view, result.Outcome.Value.BatchDigest, "statement", result.Outcome.Value.BatchDigest, result.Occurrences)
}

func TestIdeaBatchOutputRetainsAllRejectedCandidates(t *testing.T) {
	f := newGenerationExecutorFixture(t, 4, false, rejectedIdeaResponse(t))
	view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	result, err := f.executor.CollectIdeaCandidatesWithOutput(context.Background(), view, f.snapshot)
	if err != nil || result.Outcome.Value == nil || len(result.Occurrences) != 2 {
		t.Fatalf("rejected collection=%+v %v", result, err)
	}
	if _, err := domain.NoFeasibleIdeaEvidence(*result.Outcome.Value); err != nil {
		t.Fatal(err)
	}
	f.finish(t, view, result.Outcome.Value.BatchDigest, "statement", result.Outcome.Value.BatchDigest, result.Occurrences)
}

func TestIdeaBatchOutputReviewClosesLocalPublicationWithoutAttachingOutput(t *testing.T) {
	ctx := context.Background()
	f := newGenerationExecutorFixture(t, 4, false)
	view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	result, err := f.executor.CollectIdeaCandidatesWithOutput(ctx, view, f.snapshot)
	if err != nil || result.Outcome.Value == nil {
		t.Fatalf("collection=%+v %v", result, err)
	}
	output := result.Occurrences[len(result.Occurrences)-1].NewWrite
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.active.Stop(ctx, f.runID, current.Version); err != nil {
		t.Fatal(err)
	}
	current, err = f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	evidence, policy := result.Outcome.Value.BatchDigest, f.options.ProviderPolicyDigest
	command := domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "idea", AttemptID: view.AttemptID(),
		AttemptState: domain.StageAttemptNeedsReview, RunState: domain.RunNeedsReview, ReviewEvidenceDigest: &evidence, ReviewPolicyDigest: &policy,
		IdempotencyKey: coordinatorID("finish", "typed-output-review"), At: f.clock.Now()}
	if _, err := f.store.FinishStage(ctx, command); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishStage(ctx, command); err != nil {
		t.Fatalf("review replay: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, query := range map[string]string{
		"live operations":   "SELECT count(*) FROM call_records WHERE state<>'TERMINAL'",
		"live reservations": "SELECT count(*) FROM budget_reservations WHERE state='RESERVED'",
		"attached output":   "SELECT count(*) FROM artifact_occurrences",
		"active pins":       "SELECT count(*) FROM blob_pins WHERE state='ACTIVE'",
	} {
		var count int
		if err := db.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Errorf("%s=%d %v", name, count, err)
		}
	}
	var charged int64
	if err := db.QueryRow(`SELECT reservation.settled_value FROM budget_reservations reservation JOIN artifact_declarations declaration ON declaration.reservation_id=reservation.reservation_id JOIN artifact_writer_tokens token ON token.declaration_id=declaration.declaration_id WHERE token.writer_token_id=?`, output.WriterTokenID).Scan(&charged); err != nil || charged != output.PhysicalNewBytes {
		t.Errorf("discarded output charge=%d expected=%d err=%v", charged, output.PhysicalNewBytes, err)
	}
	if f.httpCalls.Load() != 1 {
		t.Fatalf("review/replay added model work: %d", f.httpCalls.Load())
	}
}

func TestIdeaBatchOutputBudgetRefusalAddsNoProviderWork(t *testing.T) {
	ctx := context.Background()
	f := newGenerationExecutorFixture(t, 4, false)
	view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	if result, err := f.executor.CollectIdeaCandidates(ctx, view, f.snapshot); err != nil || result.Outcome.Value == nil {
		t.Fatalf("provider preparation=%+v %v", result, err)
	}
	budget, err := f.store.BudgetSnapshot(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	run, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	// A separate local-write reservation occupies the remaining allowance.
	// This uses the real budget ledger, not a fabricated provider failure.
	open := f.openRequest(902)
	open.StageName, open.AttemptID, open.ExpectedRunVersion = "idea", view.AttemptID(), run.Version
	open.Provider = "private-blob"
	open.RetryPolicy.MaxAttempts = 1
	call, err := f.store.OpenCall(ctx, open)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := f.store.PrepareCalls(ctx, domain.PrepareCallsRequest{RunID: f.runID, ExpectedRunVersion: run.Version, StageName: "idea", AttemptID: view.AttemptID(),
		CallRecordID: call.ID, PlanDigest: domain.SumBytes([]byte("occupied artifact allowance")), IdempotencyKey: coordinatorID("prepare", "occupied output allowance"), At: f.clock.Now(),
		Calls: []domain.PhysicalCallPlan{{ID: domain.AttemptCallID(coordinatorID("call", "occupied output allowance")), Ordinal: 1, RetryGroup: "output-budget", RetryOrdinal: 1,
			Kind: domain.PhysicalLocalArtifactWrite, Provider: "private-blob", RequestDigest: open.RequestDigest, IdempotencyKey: coordinatorID("physical", "occupied output allowance"),
			Reservations: []domain.ReservationPlan{{ID: domain.ReservationID(coordinatorID("res", "occupied output allowance")), Dimension: domain.BudgetArtifactPhysicalNewBytes,
				Subkey: "output", UpperBound: budget.Remaining[domain.BudgetArtifactPhysicalNewBytes]}}}},
	})
	if err != nil || prepared.Failure != nil {
		t.Fatalf("occupy output allowance=%+v %v", prepared, err)
	}
	result, err := f.executor.CollectIdeaCandidatesWithOutput(ctx, view, f.snapshot)
	if err != nil || result.Outcome.Review == nil || result.Outcome.Review.Reason != "idea_output_budget_exhausted" || len(result.Occurrences) != 0 || f.httpCalls.Load() != 1 {
		t.Fatalf("output budget refusal=%+v %v HTTP=%d", result, err, f.httpCalls.Load())
	}
}

type ideaOutputPublicationFailure struct {
	application.GenerationExecutionStore
	finalizes int
}

func (s *ideaOutputPublicationFailure) FinalizeArtifact(ctx context.Context, token domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	s.finalizes++
	if s.finalizes == 2 {
		return errors.New("injected typed output publication failure after seal")
	}
	return s.GenerationExecutionStore.FinalizeArtifact(ctx, token, ref)
}
