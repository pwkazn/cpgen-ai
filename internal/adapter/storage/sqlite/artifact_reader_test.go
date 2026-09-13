package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	artifactsession "cpgen/internal/artifact"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func TestArtifactDeclarationReplayIsExactAndReaderDoesNotGrantWriter(t *testing.T) {
	fixture := newMeteringFixture(t, "d1", domain.BudgetLimits{MaxArtifactBytes: 4096})
	decl, _ := prepareReaderArtifact(t, fixture, 1)
	if err := fixture.store.CreateArtifactDeclaration(context.Background(), decl); err != nil {
		t.Fatalf("identical declaration: %v", err)
	}
	for _, mutate := range []func(*domain.ArtifactDeclarationRecord){
		func(d *domain.ArtifactDeclarationRecord) { d.MaxBytes++ },
		func(d *domain.ArtifactDeclarationRecord) { d.LogicalPath = "other.json" },
		func(d *domain.ArtifactDeclarationRecord) { d.Provenance.Producer = "other" },
	} {
		changed := decl
		mutate(&changed)
		if err := fixture.store.CreateArtifactDeclaration(context.Background(), changed); !errors.Is(err, ErrConsistency) {
			t.Fatalf("changed declaration: %v", err)
		}
	}
	if _, _, err := fixture.store.ReadArtifactWriter(context.Background(), decl.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing writer: %v", err)
	}
	var tokens int
	if err := fixture.store.db.QueryRow(`SELECT count(*) FROM artifact_writer_tokens`).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if tokens != 0 {
		t.Fatal("reader granted a writer token")
	}
}

func TestArtifactEarlyReleaseMigrationPreservesReferencedWriters(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	migrations, err := loadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, migration := range migrations[:19] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("historical migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,sha256,applied_at) VALUES(?,?,?,?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{db: db, config: Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}, clock: clock.NewFake(testNow)}
	runID := domain.RunID("run_000000000000000000000000000000d2")
	attemptID := domain.AttemptID("attempt_000000000000000000000000000000d2")
	request := testCreateRunRequest(runID, testNow, time.Minute)
	mustCreateRun(t, store, request)
	mustBeginStage(t, store, runID, attemptID, 1, "prepare", domain.SumBytes([]byte("legacy")), testNow, meteringID("begin", "reader-migration"))
	fixture := meteringFixture{store: store, runID: runID, attemptID: attemptID, stage: "prepare", now: testNow}
	var earlyTokens []domain.ArtifactWriterTokenID
	for i := 1; i <= 2; i++ {
		decl, _ := prepareReaderArtifact(t, fixture, i)
		_, token, err := store.PrepareArtifact(ctx, decl.ID)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			if err := store.OpenArtifactWriter(ctx, token.ID); err != nil {
				t.Fatal(err)
			}
		}
		// The historical CHECK contradicts its state-transition trigger.
		if err := store.ReleaseArtifact(ctx, token.ID); err == nil {
			t.Fatal("historical early-release defect was not reproduced")
		}
		earlyTokens = append(earlyTokens, token.ID)
	}
	decl, prepared := prepareReaderArtifact(t, fixture, 3)
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	session, err := artifactsession.NewPreparedArtifactSession(store, blobs, prepared)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.Prepare(ctx, decl.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("retained-response")); err != nil {
		t.Fatal(err)
	}
	pending, err := writer.Finalize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	output := domain.SumBytes([]byte("stage-output"))
	if _, err := store.FinishStage(ctx, domain.FinishStageCommand{RunID: runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending}}, IdempotencyKey: meteringID("finish", "reader-migration"), At: testNow}); err != nil {
		t.Fatal(err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("upgrade populated M19: %v", err)
	}
	restored, err := store.ReadPendingArtifact(ctx, decl.ID)
	if err != nil || !reflect.DeepEqual(restored, pending) {
		t.Fatalf("referenced artifact changed: got=%+v want=%+v err=%v", restored, pending, err)
	}
	for _, id := range earlyTokens {
		if err := store.ReleaseArtifact(ctx, id); err != nil {
			t.Fatalf("early release after migration: %v", err)
		}
		var sealed sql.NullString
		if err := db.QueryRow(`SELECT sealed_at FROM artifact_writer_tokens WHERE writer_token_id=?`, id).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if sealed.Valid {
			t.Fatal("release invented a seal timestamp")
		}
	}
	rows, err := db.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Error("migration broke a foreign key")
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	var foreignKeys, legacy, deferred int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA legacy_alter_table`).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`PRAGMA defer_foreign_keys`).Scan(&deferred); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 || legacy != 0 || deferred != 0 {
		t.Fatalf("migration leaked connection options: foreignKeys=%d legacy=%d deferred=%d", foreignKeys, legacy, deferred)
	}
	if _, err := db.Exec(`UPDATE artifact_writer_tokens SET state='OPEN' WHERE writer_token_id=?`, pending.WriterTokenID); err == nil {
		t.Fatal("migration dropped writer transition guard")
	}
}

func prepareReaderArtifact(t *testing.T, fixture meteringFixture, ordinal int) (domain.ArtifactDeclarationRecord, domain.PreparedCalls) {
	t.Helper()
	call := mustOpenMeteringCall(t, fixture, ordinal, domain.CallLLMGenerate)
	prepared, err := fixture.store.PrepareCalls(context.Background(), prepareOneRequest(fixture, call, ordinal, domain.PhysicalLocalArtifactWrite, domain.BudgetArtifactPhysicalNewBytes, 128))
	if err != nil {
		t.Fatal(err)
	}
	physical, reservation := prepared.PhysicalCalls[0], prepared.Reservations[0]
	decl := domain.ArtifactDeclarationRecord{ID: domain.ArtifactDeclarationID(fmt.Sprintf("decl_%032x", ordinal)), RunID: fixture.runID, StageName: fixture.stage, AttemptID: fixture.attemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, ReservationID: reservation.ID, ReservationSubkey: reservation.Subkey, MediaType: "application/json", Role: domain.ArtifactEvidence, LogicalPath: domain.SafeRelPath(fmt.Sprintf("private/llm/%d.json", ordinal)), MaxBytes: 128, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.llm-response/v1", Producer: "fixture"}, CreatedAt: fixture.now}
	if err := fixture.store.CreateArtifactDeclaration(context.Background(), decl); err != nil {
		t.Fatal(err)
	}
	return decl, prepared
}
