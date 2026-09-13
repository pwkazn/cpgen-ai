package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestMutationRecordReaderRestoresExactReceiptAfterReopen(t *testing.T) {
	ctx := context.Background()
	f, command := mutationStageFixture(t)
	if _, err := f.store.ReadMutationRecord(ctx, command.Mutation.Grant); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unfinished claim returned a result: %v", err)
	}
	assertMutationStageAbsent(t, f)
	if _, err := f.store.FinishMutationStage(ctx, command); err != nil {
		t.Fatal(err)
	}
	first, err := f.store.ReadMutationRecord(ctx, command.Mutation.Grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordMutation(ctx, first); err != nil {
		t.Fatalf("read result is not the exact original record: %v", err)
	}
	raw, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	configuration := f.store.config
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(ctx, configuration)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	before, err := store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		restored, err := store.ReadMutationRecord(ctx, command.Mutation.Grant)
		if err != nil {
			t.Fatal(err)
		}
		actual, _ := json.Marshal(restored)
		if !bytes.Equal(raw, actual) {
			t.Fatalf("reopened record changed: %s", actual)
		}
		// Mutating a returned slice cannot alter a subsequent durable read.
		restored.Operations[0].CallRecordID = "callrec_000000000000000000000000000000ff"
	}
	after, err := store.GetRun(ctx, f.runID)
	if err != nil || after.Version != before.Version || after.CurrentStage != before.CurrentStage {
		t.Fatalf("metadata read changed progress: %+v %v", after, err)
	}
	changed := command.Mutation.Grant
	changed.LimitSnapshot++
	changed.GrantDigest, _ = mutationGrantDigest(changed)
	if _, err := store.ReadMutationRecord(ctx, changed); err == nil {
		t.Fatal("rehashed changed grant was accepted")
	}
	changed = command.Mutation.Grant
	changed.RunID = "run_000000000000000000000000000000ff"
	changed.GrantDigest, _ = mutationGrantDigest(changed)
	if _, err := store.ReadMutationRecord(ctx, changed); err == nil {
		t.Fatal("foreign run read another run's mutation record")
	}
}

func TestMutationRecordReaderRejectsDamagedImmutableEvidence(t *testing.T) {
	for _, damage := range []struct{ name, guard, statement string }{
		{"missing operation", "mutation_record_operations_delete_guard", "DELETE FROM mutation_record_operations"},
		{"operation order", "mutation_record_children_immutable", "UPDATE mutation_record_operations SET operation_ordinal=2"},
		{"missing reservation", "mutation_record_reservations_delete_guard", "DELETE FROM mutation_record_reservations"},
		{"reservation order", "mutation_record_reservation_immutable", "UPDATE mutation_record_reservations SET reservation_ordinal=2"},
		{"missing output", "mutation_record_output_delete_guard", "DELETE FROM mutation_record_output_occurrences"},
		{"command hash", "mutation_records_immutable", "UPDATE mutation_records SET command_digest='sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'"},
		{"source binding", "mutation_records_immutable", "UPDATE mutation_records SET source_batch_digest='sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa'"},
	} {
		t.Run(damage.name, func(t *testing.T) {
			f, record := mutationRecordFixture(t, domain.ArtifactOutput)
			if err := f.store.RecordMutation(context.Background(), record); err != nil {
				t.Fatal(err)
			}
			// Deliberate offline corruption bypasses the ordinary immutable guard.
			if _, err := f.store.db.Exec("DROP TRIGGER " + damage.guard); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.Exec(damage.statement); err != nil {
				t.Fatal(err)
			}
			result, err := f.store.ReadMutationRecord(context.Background(), record.Grant)
			if err == nil || result.RecordID != "" || result.Grant.ClaimID != "" {
				t.Fatalf("damaged evidence returned a partial receipt: %+v %v", result, err)
			}
		})
	}
}

func TestMutationRecordReaderRetainsFailedOperationsAndOriginalEvidenceOrder(t *testing.T) {
	ctx := context.Background()
	f, command := mutationStageFixture(t)
	extra := mustOpenMeteringCall(t, f, 2, domain.CallLLMGenerate)
	prepared, err := f.store.PrepareCalls(ctx, prepareOneRequest(f, extra, 2, domain.PhysicalLLMRequest, domain.BudgetLLMCalls, 1))
	if err != nil || prepared.Failure != nil {
		t.Fatalf("extra operation prepare=%+v %v", prepared, err)
	}
	grant := mustBeginDispatch(t, f, extra.ID, prepared.PhysicalCalls[0].ID, "mutation-failed-operation")
	if err := f.store.MarkSent(ctx, grant, f.now); err != nil {
		t.Fatal(err)
	}
	failure := domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
	response := domain.SumBytes([]byte("rejected fixture response"))
	if err := f.store.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage,
		AttemptID: f.attemptID, CallRecordID: extra.ID, AttemptCallID: grant.AttemptCallID, State: domain.PhysicalCompleted,
		Outcome: domain.PhysicalOutcomePermanentFailure, Failure: &failure, ProviderRequestID: "mutation-failed-fixture", ResponseDigest: &response,
		IdempotencyKey: meteringID("complete", "mutation-failed-operation"), At: f.now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.FinishCall(ctx, domain.FinishCallRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage,
		AttemptID: f.attemptID, CallRecordID: extra.ID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &grant.AttemptCallID,
		Failure: &failure, IdempotencyKey: meteringID("finish", "mutation-failed-operation"), At: f.now.Add(2 * time.Second)}); err != nil {
		t.Fatal(err)
	}
	// The original command's order is intentional and differs from ID order.
	command.Mutation.Operations = append([]domain.MutationOperation{{CallRecordID: extra.ID, AttemptID: f.attemptID}}, command.Mutation.Operations...)
	for i := len(prepared.Reservations) - 1; i >= 0; i-- {
		reservation := prepared.Reservations[i]
		command.Mutation.Reservations = append(command.Mutation.Reservations, domain.MutationReservation{ReservationID: reservation.ID,
			CallRecordID: extra.ID, AttemptCallID: reservation.AttemptCallID})
	}
	if _, err := f.store.FinishMutationStage(ctx, command); err != nil {
		t.Fatal(err)
	}
	record, err := f.store.ReadMutationRecord(ctx, command.Mutation.Grant)
	if err != nil || len(record.Operations) != 2 || record.Operations[0].CallRecordID != extra.ID || len(record.Reservations) != 5 {
		t.Fatalf("mixed result evidence=%+v %v", record, err)
	}
	for i, reservation := range record.Reservations {
		if reservation != command.Mutation.Reservations[i] {
			t.Fatalf("reservation order changed at %d", i)
		}
	}
	if err := f.store.RecordMutation(ctx, record); err != nil {
		t.Fatalf("ordered mixed evidence is not exact replay: %v", err)
	}
}
