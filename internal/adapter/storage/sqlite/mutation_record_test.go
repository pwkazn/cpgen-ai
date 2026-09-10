package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	artifactsession "cpgen/internal/artifact"
	"cpgen/internal/domain"
)

func pendingMutationRecordFixture(t *testing.T, role domain.ArtifactRole, mismatch ...string) (meteringFixture, domain.MutationRecordRequest, domain.FinishStageCommand) {
	t.Helper()
	ctx := context.Background()
	f := newMeteringFixture(t, "d1", testCreateRunRequest(testRunID, testNow, time.Minute).BudgetLimits)
	grant, err := f.store.ClaimMutation(ctx, domain.MutationClaimRequest{RunID: f.runID, StageName: f.stage, ScopeDigest: domain.SumBytes([]byte("mutation-scope")), SourceBatchDigest: domain.SumBytes([]byte("source-batch")), Ordinal: 1, LimitSnapshot: 2, Kind: domain.MutationContent, IntentDigest: domain.SumBytes([]byte("mutation-core")), At: f.now})
	if err != nil {
		t.Fatal(err)
	}
	call := mustOpenMeteringCall(t, f, 1, domain.CallSandboxRun)
	prepared, err := f.store.PrepareCalls(ctx, prepareOneRequest(f, call, 1, domain.PhysicalLocalArtifactWrite, domain.BudgetArtifactPhysicalNewBytes, 64))
	if err != nil {
		t.Fatal(err)
	}
	physical, reservation := prepared.PhysicalCalls[0], prepared.Reservations[0]
	declarationID := domain.ArtifactDeclarationID("decl_000000000000000000000000000000d1")
	declaration := domain.ArtifactDeclarationRecord{ID: declarationID, RunID: f.runID, StageName: f.stage, AttemptID: f.attemptID, CallRecordID: call.ID,
		AttemptCallID: physical.ID, ReservationID: reservation.ID, ReservationSubkey: reservation.Subkey, MediaType: "application/json", Role: role,
		LogicalPath: "mutation-output.json", MaxBytes: 64, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "mutation-ledger-fixture"}, CreatedAt: f.now,
	}
	if err := f.store.CreateArtifactDeclaration(ctx, declaration); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := artifactsession.NewPreparedArtifactSession(f.store, blobs, prepared)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.Prepare(ctx, declarationID)
	if err != nil {
		t.Fatal(err)
	}
	writeGrant := mustBeginDispatch(t, f, call.ID, physical.ID, "mutation_output_dispatch")
	if _, err := writer.Write([]byte(`{"fixture":"output"}`)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkSent(ctx, writeGrant, f.now); err != nil {
		t.Fatal(err)
	}
	pending, err := writer.Finalize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	completion := domain.CompletePhysicalRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID,
		CallRecordID: call.ID, AttemptCallID: physical.ID, State: domain.PhysicalCompleted, Outcome: domain.PhysicalOutcomeSuccess, ResponseDigest: &pending.Blob.Digest, ProviderRequestID: "local-artifact:" + string(pending.WriterTokenID),
		Usage:          []domain.ReservationUsage{{ReservationID: reservation.ID, Dimension: reservation.Dimension, Subkey: reservation.Subkey, Value: pending.PhysicalNewBytes, Verified: true}},
		IdempotencyKey: meteringID("complete", "mutation-output"), At: f.now.Add(time.Second),
	}
	if len(mismatch) > 0 {
		switch mismatch[0] {
		case "byte count":
			completion.Usage[0].Value++
		case "receipt digest":
			digest := domain.SumBytes([]byte("unrelated blob"))
			completion.ResponseDigest = &digest
		case "failed output":
			completion.Outcome = domain.PhysicalOutcomePermanentFailure
			completion.Failure = &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
		case "provenance producer":
			pending.Provenance.Producer = "substituted producer"
		case "provenance schema":
			pending.Provenance.SchemaVersion = "cpgen.substituted/v1"
		case "provenance input":
			digest := domain.SumBytes([]byte("substituted provenance input"))
			pending.Provenance.InputDigest = &digest
		}
	}
	if err := f.store.CompletePhysical(ctx, completion); err != nil {
		t.Fatal(err)
	}
	finishCall := domain.FinishCallRequest{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID, CallRecordID: call.ID,
		DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &physical.ID, IdempotencyKey: meteringID("finish", "mutation-output"), At: f.now.Add(2 * time.Second),
		Failure: completion.Failure,
	}
	if len(mismatch) > 0 && mismatch[0] == "failed logical call" {
		finishCall.Failure = &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
	}
	if _, err := f.store.FinishCall(ctx, finishCall); err != nil {
		t.Fatal(err)
	}
	record := domain.MutationRecordRequest{Grant: grant, RecordID: "mutation_record_fixture", Operations: []domain.MutationOperation{{CallRecordID: call.ID, AttemptID: f.attemptID}},
		Reservations:       []domain.MutationReservation{{ReservationID: reservation.ID, CallRecordID: call.ID, AttemptCallID: physical.ID}},
		OutputOccurrenceID: "occurrence_000000000000000000000000000000d1", At: f.now.Add(4 * time.Second),
	}
	if err := record.Validate(); err != nil {
		t.Fatalf("precommit record envelope: %v", err)
	}
	if err := f.store.RecordMutation(ctx, record); err == nil {
		t.Fatal("uncommitted output authorized a mutation record")
	}
	var count int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM mutation_records`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected output left partial record: %d %v", count, err)
	}
	output := pending.Blob.Digest
	finish := domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 2, StageName: f.stage, AttemptID: f.attemptID,
		AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output,
		Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending}}, IdempotencyKey: meteringID("finish", "mutation-stage"), At: f.now.Add(3 * time.Second),
	}
	return f, record, finish
}

func mutationRecordFixture(t *testing.T, role domain.ArtifactRole, mismatch ...string) (meteringFixture, domain.MutationRecordRequest) {
	t.Helper()
	ctx := context.Background()
	f, record, finish := pendingMutationRecordFixture(t, role, mismatch...)
	var count int
	var beforeBytes, beforeVersion int64
	if err := f.store.db.QueryRow(`SELECT consumed_value,account_version FROM budget_accounts WHERE run_id=? AND dimension=?`, f.runID, domain.BudgetArtifactPhysicalNewBytes).Scan(&beforeBytes, &beforeVersion); err != nil {
		t.Fatal(err)
	}
	_, finishErr := f.store.FinishStage(ctx, finish)
	if len(mismatch) > 0 && mismatch[0] != "failed logical call" {
		if finishErr == nil {
			t.Fatalf("%s mismatch attached output", mismatch[0])
		}
		if err := f.store.db.QueryRow(`SELECT count(*) FROM artifact_occurrences WHERE run_id=?`, f.runID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected receipt left output: %d %v", count, err)
		}
		return f, record
	}
	if finishErr != nil {
		t.Fatal(finishErr)
	}
	if _, err := f.store.FinishStage(ctx, finish); err != nil {
		t.Fatalf("stage attachment replay: %v", err)
	}
	var afterBytes, afterVersion int64
	if err := f.store.db.QueryRow(`SELECT consumed_value,account_version FROM budget_accounts WHERE run_id=? AND dimension=?`, f.runID, domain.BudgetArtifactPhysicalNewBytes).Scan(&afterBytes, &afterVersion); err != nil || beforeBytes != afterBytes || beforeVersion != afterVersion {
		t.Fatalf("settled output was charged twice: before %d/%d after %d/%d %v", beforeBytes, beforeVersion, afterBytes, afterVersion, err)
	}
	if err := f.store.db.QueryRow(`SELECT occurrence_id FROM artifact_occurrences WHERE run_id=? AND stage_name=?`, f.runID, f.stage).Scan(&record.OutputOccurrenceID); err != nil {
		t.Fatal(err)
	}
	return f, record
}

func TestMutationRecordRejectsSettledOutputReceiptMismatch(t *testing.T) {
	for _, mismatch := range []string{"byte count", "receipt digest", "failed output"} {
		t.Run(mismatch, func(t *testing.T) { mutationRecordFixture(t, domain.ArtifactOutput, mismatch) })
	}
}

func TestArtifactAttachmentRejectsSubstitutedDeclaredProvenance(t *testing.T) {
	for _, mismatch := range []string{"provenance producer", "provenance schema", "provenance input"} {
		t.Run(mismatch, func(t *testing.T) { mutationRecordFixture(t, domain.ArtifactOutput, mismatch) })
	}
}

func TestArtifactAttachmentRetainsCompletedDiagnosticFromFailedLogicalCall(t *testing.T) {
	// A failed provider/compile may still publish valid diagnostic evidence.
	// Attachment validates the successful local write, not business acceptance.
	mutationRecordFixture(t, domain.ArtifactEvidence, "failed logical call")
}

func TestMutationRecordRetainsSettledOutputAndExactReplay(t *testing.T) {
	f, record := mutationRecordFixture(t, domain.ArtifactOutput)
	ctx := context.Background()
	if err := f.store.RecordMutation(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordMutation(ctx, record); err != nil {
		t.Fatalf("exact replay: %v", err)
	}
	for _, table := range []string{"mutation_records", "mutation_record_operations", "mutation_record_reservations", "mutation_record_output_occurrences"} {
		var count int
		if err := f.store.db.QueryRow(`SELECT count(*) FROM `+table+` WHERE record_id=?`, record.RecordID).Scan(&count); err != nil || count != 1 {
			t.Fatalf("%s count=%d %v", table, count, err)
		}
		if _, err := f.store.db.Exec(`DELETE FROM `+table+` WHERE record_id=?`, record.RecordID); err == nil {
			t.Fatalf("%s was mutable", table)
		}
	}
	record.At = record.At.Add(time.Nanosecond)
	if err := f.store.RecordMutation(ctx, record); err == nil {
		t.Fatal("changed mutation command replay admitted")
	}
}

func TestMutationRecordRejectsEvidenceAsOutput(t *testing.T) {
	f, record := mutationRecordFixture(t, domain.ArtifactEvidence)
	if err := f.store.RecordMutation(context.Background(), record); err == nil {
		t.Fatal("private evidence was accepted as typed mutation output")
	}
	var count int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM mutation_records`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected role left partial record: %d %v", count, err)
	}
}

func TestMutationRecordRejectsDuplicateEvidence(t *testing.T) {
	for _, repeated := range []string{"operation", "reservation"} {
		t.Run(repeated, func(t *testing.T) {
			f, record := mutationRecordFixture(t, domain.ArtifactOutput)
			if repeated == "operation" {
				record.Operations = append(record.Operations, record.Operations[0])
			} else {
				record.Reservations = append(record.Reservations, record.Reservations[0])
			}
			if err := f.store.RecordMutation(context.Background(), record); err == nil {
				t.Fatal("duplicate mutation evidence was persisted")
			}
		})
	}
}

func TestMutationRecordRejectsChangedImmutableGrantLimit(t *testing.T) {
	f, record := mutationRecordFixture(t, domain.ArtifactOutput)
	record.Grant.LimitSnapshot++
	var err error
	record.Grant.GrantDigest, err = mutationGrantDigest(record.Grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.RecordMutation(context.Background(), record); err == nil {
		t.Fatal("rehashed grant changed its immutable limit")
	}
	var count int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM mutation_records`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("changed grant left record: %d %v", count, err)
	}
}
