package sqlite

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	artifactsession "cpgen/internal/artifact"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestArtifactLedgerMigrationCreatesPrivateUnionTables(t *testing.T) {
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "artifact.db"), clock.NewFake(testNow))
	rows, err := store.db.QueryContext(context.Background(), `SELECT name FROM sqlite_master WHERE type = 'table' AND name IN
		('artifact_declarations','artifact_writer_tokens','blobs','blob_pins','blob_pin_history','cache_reuse_records','artifact_occurrences') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(names) != 7 {
		t.Fatalf("artifact tables = %v, want 7", names)
	}
}

func TestCorruptVerifiedBlobQuarantinesSQLiteRow(t *testing.T) {
	fixture := newMeteringFixture(t, "72", testCreateRunRequest(domain.RunID("run_00000000000000000000000000000072"), testNow, time.Minute).BudgetLimits)
	call := mustOpenMeteringCall(t, fixture, 1, domain.CallSandboxRun)
	prepared, err := fixture.store.PrepareCalls(context.Background(), prepareOneRequest(fixture, call, 1, domain.PhysicalLocalArtifactWrite, domain.BudgetArtifactPhysicalNewBytes, 64))
	if err != nil {
		t.Fatal(err)
	}
	physical, reservation := prepared.PhysicalCalls[0], prepared.Reservations[0]
	declarationID := domain.ArtifactDeclarationID("decl_00000000000000000000000000000072")
	declaration := domain.ArtifactDeclarationRecord{ID: declarationID, RunID: fixture.runID, StageName: fixture.stage, AttemptID: fixture.attemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, ReservationID: reservation.ID, ReservationSubkey: reservation.Subkey, MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: "output.txt", MaxBytes: 64, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}, CreatedAt: fixture.now}
	if err := fixture.store.CreateArtifactDeclaration(context.Background(), declaration); err != nil {
		t.Fatal(err)
	}
	blobRoot := filepath.Join(t.TempDir(), "blobs")
	blobs, err := blob.NewStore(blobRoot)
	if err != nil {
		t.Fatal(err)
	}
	session, err := artifactsession.NewPreparedArtifactSession(fixture.store, blobs, prepared)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.Prepare(context.Background(), declarationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("artifact")); err != nil {
		t.Fatal(err)
	}
	pending, err := writer.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hex := strings.TrimPrefix(string(pending.Blob.Digest), "sha256:")
	canonical := filepath.Join(blobRoot, "blobs", "sha256", hex[:2], hex)
	if err := os.WriteFile(canonical, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if reader, err := blobs.OpenVerified(context.Background(), pending.Blob); reader != nil {
		_ = reader.Close()
		if err == nil {
			t.Fatal("corrupted blob verified")
		}
	} else if err == nil {
		t.Fatal("corrupted blob verified")
	}
	var state string
	if err := fixture.store.db.QueryRow(`SELECT state FROM blobs WHERE digest = ? AND size = ?`, pending.Blob.Digest, pending.Blob.Size).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "CORRUPT" {
		t.Fatalf("blob state = %q, want CORRUPT", state)
	}
}

func TestStagingBlobQuarantineIsDurable(t *testing.T) {
	fixture := newMeteringFixture(t, "73", testCreateRunRequest(domain.RunID("run_00000000000000000000000000000073"), testNow, time.Minute).BudgetLimits)
	ref := domain.BlobRef{Digest: domain.SumBytes([]byte("staging-corrupt")), Size: int64(len("staging-corrupt"))}
	if _, err := fixture.store.db.Exec(`INSERT INTO blobs(digest, size, state, canonical_relative_path) VALUES (?, ?, 'STAGING', ?)`, ref.Digest, ref.Size, "blobs/sha256/staging-corrupt"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.QuarantineBlob(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := fixture.store.db.QueryRow(`SELECT state FROM blobs WHERE digest = ? AND size = ?`, ref.Digest, ref.Size).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "CORRUPT" {
		t.Fatalf("staging blob state = %q, want CORRUPT", state)
	}
}

func TestArtifactWriterSessionFinalizesAndFinishAttachesOccurrence(t *testing.T) {
	fixture := newMeteringFixture(t, "71", testCreateRunRequest(domain.RunID("run_00000000000000000000000000000071"), testNow, time.Minute).BudgetLimits)
	call := mustOpenMeteringCall(t, fixture, 1, domain.CallSandboxRun)
	prepared, err := fixture.store.PrepareCalls(context.Background(), prepareOneRequest(fixture, call, 1, domain.PhysicalLocalArtifactWrite, domain.BudgetArtifactPhysicalNewBytes, 64))
	if err != nil {
		t.Fatal(err)
	}
	physical := prepared.PhysicalCalls[0]
	reservation := prepared.Reservations[0]
	declarationID := domain.ArtifactDeclarationID("decl_00000000000000000000000000000071")
	declaration := domain.ArtifactDeclarationRecord{ID: declarationID, RunID: fixture.runID, StageName: fixture.stage, AttemptID: fixture.attemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, ReservationID: reservation.ID, ReservationSubkey: reservation.Subkey, MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: "output.txt", MaxBytes: 64, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}, CreatedAt: fixture.now}
	if err := fixture.store.CreateArtifactDeclaration(context.Background(), declaration); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := artifactsession.NewPreparedArtifactSession(fixture.store, blobs, prepared)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.Prepare(context.Background(), declarationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("artifact")); err != nil {
		t.Fatal(err)
	}
	pending, err := writer.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The attachment must use the writer/blob ledger's authoritative physical
	// byte count, not an untrusted caller-provided payload field.
	pending.PhysicalNewBytes = 0
	output := domain.SumBytes([]byte("stage-output"))
	if _, err := fixture.store.FinishStage(context.Background(), domain.FinishStageCommand{RunID: fixture.runID, ExpectedRunVersion: 2, StageName: fixture.stage, AttemptID: fixture.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending}}, IdempotencyKey: "finish_00000000000000000000000000000071", At: testNow.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	var occurrences, settled int
	if err := fixture.store.db.QueryRow(`SELECT count(*) FROM artifact_occurrences WHERE run_id = ?`, fixture.runID).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.db.QueryRow(`SELECT count(*) FROM budget_reservations WHERE reservation_id = ? AND state = 'SETTLED'`, reservation.ID).Scan(&settled); err != nil {
		t.Fatal(err)
	}
	if occurrences != 1 || settled != 1 {
		t.Fatalf("occurrences=%d settled=%d", occurrences, settled)
	}
	var consumed int64
	if err := fixture.store.db.QueryRow(`SELECT consumed_value FROM budget_accounts WHERE run_id = ? AND dimension = ?`, fixture.runID, domain.BudgetArtifactPhysicalNewBytes).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != int64(len("artifact")) {
		t.Fatalf("consumed physical bytes = %d, want %d", consumed, len("artifact"))
	}
	if err := session.ReleaseUnused(context.Background()); err != nil {
		t.Fatal(err)
	}
	var tokenState string
	if err := fixture.store.db.QueryRow(`SELECT state FROM artifact_writer_tokens WHERE writer_token_id = ?`, pending.WriterTokenID).Scan(&tokenState); err != nil {
		t.Fatal(err)
	}
	if tokenState != string(domain.ArtifactWriterFinalized) {
		t.Fatalf("used finalized writer released as %q", tokenState)
	}
}

func TestPreparedArtifactSessionDoesNotDoubleOpenDeterministicWriter(t *testing.T) {
	fixture := newMeteringFixture(t, "87", testCreateRunRequest(domain.RunID("run_00000000000000000000000000000087"), testNow, time.Minute).BudgetLimits)
	call := mustOpenMeteringCall(t, fixture, 1, domain.CallSandboxRun)
	prepared, err := fixture.store.PrepareCalls(context.Background(), prepareOneRequest(fixture, call, 1, domain.PhysicalLocalArtifactWrite, domain.BudgetArtifactPhysicalNewBytes, 64))
	if err != nil {
		t.Fatal(err)
	}
	physical, reservation := prepared.PhysicalCalls[0], prepared.Reservations[0]
	declarationID := domain.ArtifactDeclarationID("decl_00000000000000000000000000000087")
	declaration := domain.ArtifactDeclarationRecord{ID: declarationID, RunID: fixture.runID, StageName: fixture.stage, AttemptID: fixture.attemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, ReservationID: reservation.ID, ReservationSubkey: reservation.Subkey, MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: "output.txt", MaxBytes: 64, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}, CreatedAt: fixture.now}
	if err := fixture.store.CreateArtifactDeclaration(context.Background(), declaration); err != nil {
		t.Fatal(err)
	}
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := artifactsession.NewPreparedArtifactSession(fixture.store, blobs, prepared)
	if err != nil {
		t.Fatal(err)
	}
	second, err := artifactsession.NewPreparedArtifactSession(fixture.store, blobs, prepared)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	writers := make(chan port.ArtifactWriter, 2)
	var wg sync.WaitGroup
	for _, session := range []artifactsession.PreparedArtifactSession{first, second} {
		wg.Add(1)
		go func(session artifactsession.PreparedArtifactSession) {
			defer wg.Done()
			writer, prepareErr := session.Prepare(context.Background(), declarationID)
			if prepareErr == nil {
				writers <- writer
			}
			results <- prepareErr
		}(session)
	}
	wg.Wait()
	close(results)
	close(writers)
	var successes, failures int
	for prepareErr := range results {
		if prepareErr == nil {
			successes++
		} else {
			failures++
		}
	}
	if successes != 1 || failures != 1 {
		t.Fatalf("concurrent Prepare results = successes %d failures %d, want one each", successes, failures)
	}
	for writer := range writers {
		if _, err := writer.Write([]byte("exclusive")); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Finalize(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
