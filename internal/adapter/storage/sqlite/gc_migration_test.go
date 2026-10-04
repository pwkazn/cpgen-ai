package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func TestGarbageMigrationPreservesM33PublicationOwnerAndReleasedHistory(t *testing.T) {
	for _, test := range []struct {
		name, gcState string
		retainPin     bool
	}{
		{"READY", "NONE", false},
		{"DELETING", "DELETING", false},
		{"DELETING_with_late_active_pin", "DELETING", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testGarbageMigrationPreservesM33History(t, test.gcState, test.retainPin)
		})
	}
}

func testGarbageMigrationPreservesM33History(t *testing.T, gcState string, retainPin bool) {
	t.Helper()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "m33.db")
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
	for _, migration := range migrations[:33] {
		if _, err := db.ExecContext(ctx, migration.sql); err != nil {
			t.Fatalf("migration %d: %v", migration.version, err)
		}
		if _, err := db.ExecContext(ctx, `INSERT INTO schema_migrations(version,name,sha256,applied_at) VALUES(?,?,?,?)`, migration.version, migration.name, migration.hash, formatTime(testNow)); err != nil {
			t.Fatal(err)
		}
	}
	store := &Store{db: db, config: Config{Path: path, BusyTimeout: time.Second, MaxReaders: 1}, clock: clock.NewFake(testNow)}
	runID := domain.RunID("run_000000000000000000000000000000f7")
	attemptID := domain.AttemptID("attempt_000000000000000000000000000000f7")
	mustCreateRun(t, store, testCreateRunRequest(runID, testNow, time.Minute))
	mustBeginStage(t, store, runID, attemptID, 1, "prepare", domain.SumBytes([]byte("gc migration")), testNow, meteringID("begin", "gc migration"))
	g := &garbageFixture{meteringFixture: meteringFixture{store: store, runID: runID, attemptID: attemptID, stage: "prepare", now: testNow}}
	ref := domain.BlobRef{Digest: domain.SumBytes([]byte("historical garbage")), Size: int64(len("historical garbage"))}
	if _, err := db.Exec(`INSERT INTO blobs(digest,size,state,canonical_relative_path,verified_at) VALUES(?,?,'READY',?,?)`, ref.Digest, ref.Size, canonicalRelativePath(ref), formatTime(testNow)); err != nil {
		t.Fatal(err)
	}
	var tokens []domain.ArtifactWriterToken
	for i := 0; i < 2; i++ {
		if i == 1 && retainPin {
			// M33 allowed a new writer to seal after the old collector had
			// planned deletion and crashed, releasing its exclusive lock.
			if _, err := db.Exec(`UPDATE blobs SET gc_state='DELETING' WHERE digest=? AND size=?`, ref.Digest, ref.Size); err != nil {
				t.Fatal(err)
			}
		}
		id, _ := g.declare(t)
		_, token, err := store.PrepareArtifact(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.OpenArtifactWriter(ctx, token.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE artifact_writer_tokens SET state='SEALED',final_digest=?,final_size=?,sealed_at=? WHERE writer_token_id=?`, ref.Digest, ref.Size, formatTime(testNow), token.ID); err != nil {
			t.Fatal(err)
		}
		charge := int64(0)
		if i == 0 {
			charge = ref.Size
		}
		if _, err := db.Exec(`INSERT INTO blob_pins(pin_id,writer_token_id,digest,size,state,created_at,physical_new_bytes) VALUES(?,?,?,?,'ACTIVE',?,?)`, token.PinID, token.ID, ref.Digest, ref.Size, formatTime(testNow), charge); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			if _, err := db.Exec(`INSERT INTO artifact_blob_publication_owners(digest,size,pin_id) VALUES(?,?,?)`, ref.Digest, ref.Size, token.PinID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := db.Exec(`INSERT INTO blob_pin_history(pin_id,ordinal,state,changed_at) VALUES(?,1,'ACTIVE',?)`, token.PinID, formatTime(testNow)); err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE artifact_writer_tokens SET state='FINALIZED',finalized_at=? WHERE writer_token_id=?`, formatTime(testNow), token.ID); err != nil {
			t.Fatal(err)
		}
		if !retainPin || i == 0 {
			if err := store.ReleaseArtifact(ctx, token.ID); err != nil {
				t.Fatal(err)
			}
		}
		tokens = append(tokens, token)
	}
	if gcState == "DELETING" {
		if _, err := db.Exec(`UPDATE blobs SET gc_state='DELETING' WHERE digest=? AND size=?`, ref.Digest, ref.Size); err != nil {
			t.Fatal(err)
		}
		// The legacy collector could already have renamed canonical bytes,
		// then fail its metadata DELETE on these released historical FKs.
		if _, err := db.Exec(`DELETE FROM blobs WHERE digest=? AND size=?`, ref.Digest, ref.Size); err == nil {
			t.Fatal("M33 fixture did not reproduce the blocked garbage deletion")
		}
	}
	before := make([][]string, len(tokens))
	for i, token := range tokens {
		before[i] = garbageHistory(t, store, token.ID)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("M33 upgrade with live historical references: %v", err)
	}
	for i, token := range tokens {
		if got := garbageHistory(t, store, token.ID); !reflect.DeepEqual(got, before[i]) {
			t.Fatalf("migration rewrote token/pin history: before=%v after=%v", before[i], got)
		}
	}
	var pinID string
	var generation int64
	if err := db.QueryRow(`SELECT pin_id,generation FROM artifact_blob_publication_owners WHERE digest=? AND size=?`, ref.Digest, ref.Size).Scan(&pinID, &generation); err != nil {
		t.Fatal(err)
	}
	if pinID != string(tokens[0].PinID) || generation != 1 {
		t.Fatalf("migrated publication owner=%s generation=%d", pinID, generation)
	}
	var stateAfterMigration string
	if err := db.QueryRow(`SELECT gc_state FROM blobs WHERE digest=? AND size=?`, ref.Digest, ref.Size).Scan(&stateAfterMigration); err != nil {
		t.Fatal(err)
	}
	if stateAfterMigration != gcState {
		t.Fatalf("migration changed GC state from %s to %s", gcState, stateAfterMigration)
	}
	var items []domain.GCItem
	if gcState == "DELETING" {
		items, err = store.ListDeleting(ctx)
	} else {
		items, err = store.PlanGarbage(ctx)
	}
	if retainPin {
		if !errors.Is(err, ErrConsistency) || len(items) != 0 {
			t.Fatalf("legacy DELETING blob with active pin authorized filesystem deletion: items=%v, err=%v", items, err)
		}
	} else {
		if err != nil || len(items) != 1 {
			t.Fatalf("migrated garbage plan=%v, %v", items, err)
		}
		if err := store.CommitGarbage(ctx, items[0], domain.GCCommitRemoved); err != nil {
			t.Fatalf("migrated released references still block garbage commit: %v", err)
		}
	}
	for i, token := range tokens {
		if got := garbageHistory(t, store, token.ID); !reflect.DeepEqual(got, before[i]) {
			t.Fatal("garbage commit rewrote migrated history")
		}
	}
	if err := store.checkIntegrity(ctx); err != nil {
		t.Fatal(err)
	}
	var foreignKeys int
	if err := db.QueryRow(`PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatal(err)
	}
	if foreignKeys != 1 {
		t.Fatal("migration disabled foreign key enforcement")
	}
	if _, err := db.Exec(`UPDATE artifact_blob_publication_owners SET generation=2 WHERE digest=? AND size=?`, ref.Digest, ref.Size); err == nil {
		t.Fatal("migrated owner generation is mutable")
	}
	if _, err := db.Exec(`DELETE FROM artifact_blob_publication_owners WHERE digest=? AND size=?`, ref.Digest, ref.Size); err == nil {
		t.Fatal("migrated owner can be deleted")
	}
}
