package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func mutationStageFixture(t *testing.T, mismatch ...string) (meteringFixture, domain.FinishMutationStageCommand) {
	t.Helper()
	f, record, finish := pendingMutationRecordFixture(t, domain.ArtifactOutput, mismatch...)
	// A typed output's semantic digest need not equal its serialized Blob hash.
	semantic := domain.SumBytes([]byte("locally validated mutation batch"))
	finish.OutputDigest = &semantic
	command := domain.FinishMutationStageCommand{Finish: finish, Mutation: domain.MutationStageRecord{
		Grant: record.Grant, RecordID: record.RecordID, Operations: record.Operations, Reservations: record.Reservations,
		OutputWriterTokenID: finish.Occurrences[0].NewWrite.WriterTokenID, OutputBlobDigest: finish.Occurrences[0].NewWrite.Blob.Digest,
	}}
	if err := command.Validate(); err != nil {
		t.Fatal(err)
	}
	return f, command
}

func assertMutationStageAbsent(t *testing.T, f meteringFixture) {
	t.Helper()
	for _, table := range []string{"artifact_occurrences", "mutation_records", "mutation_record_operations", "mutation_record_reservations", "mutation_record_output_occurrences"} {
		var count int
		if err := f.store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s rows=%d err=%v", table, count, err)
		}
	}
	var state, pinState string
	if err := f.store.db.QueryRow(`SELECT state FROM stage_attempts WHERE attempt_id=?`, f.attemptID).Scan(&state); err != nil || state != "RUNNING" {
		t.Fatalf("attempt advanced after failure: %s %v", state, err)
	}
	if err := f.store.db.QueryRow(`SELECT pin.state FROM blob_pins pin JOIN artifact_writer_tokens token ON token.pin_id=pin.pin_id WHERE token.run_id=?`, f.runID).Scan(&pinState); err != nil || pinState != "ACTIVE" {
		t.Fatalf("output lost its recovery pin: %s %v", pinState, err)
	}
}

func TestFinishMutationStageCommitsOutputRecordAndProgressExactlyOnce(t *testing.T) {
	ctx := context.Background()
	f, command := mutationStageFixture(t)
	before, err := f.store.BudgetSnapshot(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.store.FinishMutationStage(ctx, command)
	if err != nil || result.Version != 3 || result.CurrentStage != "exercise" {
		t.Fatalf("atomic finish=%+v err=%v", result, err)
	}
	for _, table := range []string{"artifact_occurrences", "mutation_records", "mutation_record_operations", "mutation_record_reservations", "mutation_record_output_occurrences"} {
		var count int
		if err := f.store.db.QueryRow(`SELECT count(*) FROM ` + table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("committed %s rows=%d err=%v", table, count, err)
		}
	}
	var output domain.ArtifactOccurrenceID
	if err := f.store.db.QueryRow(`SELECT occurrence_id FROM mutation_record_output_occurrences WHERE record_id=?`, command.Mutation.RecordID).Scan(&output); err != nil {
		t.Fatal(err)
	}
	record := domain.MutationRecordRequest{Grant: command.Mutation.Grant, RecordID: command.Mutation.RecordID, Operations: command.Mutation.Operations,
		Reservations: command.Mutation.Reservations, OutputOccurrenceID: output, At: command.Finish.At}
	if err := f.store.RecordMutation(ctx, record); err != nil {
		t.Fatalf("atomic record differs from the generic immutable receipt: %v", err)
	}
	// A later stage may already be active when the original commit response is
	// retried. Replay returns its original result without rewinding that stage.
	_, err = f.store.BeginStage(ctx, domain.BeginStageCommand{RunID: f.runID, ExpectedRunVersion: result.Version, StageName: "exercise",
		AttemptID: "attempt_000000000000000000000000000000d2", InputDigest: *command.Finish.NextInputDigest,
		IdempotencyKey: meteringID("begin", "mutation-successor"), At: command.Finish.At.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := f.store.FinishMutationStage(ctx, command)
	if err != nil || replayed.Version != result.Version {
		t.Fatalf("atomic replay=%+v err=%v", replayed, err)
	}
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil || current.Version != 4 || current.CurrentStage != "exercise" {
		t.Fatalf("replay rewound successor: %+v %v", current, err)
	}
	if _, err := f.store.FinishStage(ctx, command.Finish); err == nil {
		t.Fatal("ordinary stage command reused the composite command identity")
	}
	command.Mutation.RecordID += "_changed"
	if _, err := f.store.FinishMutationStage(ctx, command); err == nil {
		t.Fatal("changed mutation receipt replay accepted")
	}
	after, err := f.store.BudgetSnapshot(ctx, f.runID)
	if err != nil || after.Version != before.Version || after.Remaining[domain.BudgetArtifactPhysicalNewBytes] != before.Remaining[domain.BudgetArtifactPhysicalNewBytes] {
		t.Fatalf("atomic attachment/replay charged twice: %+v %v", after, err)
	}
}

func TestFinishMutationStageRollsBackEveryCommitComponent(t *testing.T) {
	for _, failure := range []string{"record insertion", "successor update", "transaction commit"} {
		t.Run(failure, func(t *testing.T) {
			f, command := mutationStageFixture(t)
			bad := command
			switch failure {
			case "record insertion":
				bad.Mutation.Grant.LimitSnapshot++
				bad.Mutation.Grant.GrantDigest, _ = mutationGrantDigest(bad.Mutation.Grant)
			case "successor update":
				bad.Finish.NextStage = "missing"
			case "transaction commit":
				f.store.endTransactionHook = func(operation string) error {
					if operation == "commit" {
						return errors.New("injected mutation commit failure")
					}
					return nil
				}
			}
			_, err := f.store.FinishMutationStage(context.Background(), bad)
			f.store.endTransactionHook = nil
			if err == nil {
				t.Fatalf("%s unexpectedly committed", failure)
			}
			assertMutationStageAbsent(t, f)
			if _, err := f.store.FinishMutationStage(context.Background(), command); err != nil {
				t.Fatalf("corrected exact commit could not recover: %v", err)
			}
		})
	}
}

func TestFinishMutationStageRejectsIncompleteAttemptEvidence(t *testing.T) {
	for _, omitted := range []string{"operation", "reservation"} {
		t.Run(omitted, func(t *testing.T) {
			f, command := mutationStageFixture(t)
			extra := mustOpenMeteringCall(t, f, 2, domain.CallLLMGenerate)
			if omitted == "reservation" {
				_, err := f.store.PrepareCalls(context.Background(), prepareOneRequest(f, extra, 2, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1))
				if err != nil {
					t.Fatal(err)
				}
				command.Mutation.Operations = append(command.Mutation.Operations, domain.MutationOperation{CallRecordID: extra.ID, AttemptID: f.attemptID})
			}
			if _, err := f.store.FinishMutationStage(context.Background(), command); err == nil {
				t.Fatalf("omitted %s accepted", omitted)
			}
			assertMutationStageAbsent(t, f)
		})
	}
}

func TestFinishMutationStageHonorsPendingCancellation(t *testing.T) {
	f, command := mutationStageFixture(t)
	_, err := f.store.RequestCancel(context.Background(), domain.CancelRequest{ID: "control_000000000000000000000000000000d1", RunID: f.runID,
		ExpectedRunVersion: 2, Reason: "cancel mutation", IdempotencyKey: meteringID("cancel", "mutation"), At: f.now.Add(3 * time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	command.Finish.ExpectedRunVersion = 3
	command.Finish.At = f.now.Add(4 * time.Second)
	if _, err := f.store.FinishMutationStage(context.Background(), command); !errors.Is(err, ErrCancelPending) {
		t.Fatalf("pending cancellation=%v", err)
	}
	assertMutationStageAbsent(t, f)
}

func TestFinishMutationStageRejectsFailedOutputOperation(t *testing.T) {
	f, command := mutationStageFixture(t, "failed logical call")
	if _, err := f.store.FinishMutationStage(context.Background(), command); err == nil {
		t.Fatal("failed output operation became a successful mutation record")
	}
	assertMutationStageAbsent(t, f)
}
