package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	artifactsession "cpgen/internal/artifact"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/runlock"
)

type garbageFixture struct {
	meteringFixture
	root        string
	blobs       *blob.Store
	maintenance *artifactsession.Maintenance
	locks       *runlock.Manager
	next        int
}

func newGarbageFixture(t *testing.T) *garbageFixture {
	t.Helper()
	f := newMeteringFixture(t, "f6", testCreateRunRequest(domain.RunID("run_000000000000000000000000000000f6"), testNow, time.Minute).BudgetLimits)
	g := &garbageFixture{meteringFixture: f, root: t.TempDir()}
	g.openFiles(t)
	return g
}

func (g *garbageFixture) openFiles(t *testing.T) {
	t.Helper()
	var err error
	g.blobs, err = blob.NewStore(filepath.Join(g.root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	lockRoot := filepath.Join(g.root, "locks")
	if err := os.MkdirAll(lockRoot, 0700); err != nil {
		t.Fatal(err)
	}
	g.locks, err = runlock.NewManager(lockRoot, runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	locks := g.locks
	t.Cleanup(func() { _ = locks.Close() })
	g.maintenance, err = artifactsession.NewMaintenance(g.locks, g.store, g.blobs)
	if err != nil {
		t.Fatal(err)
	}
}

func (g *garbageFixture) declare(t *testing.T) (domain.ArtifactDeclarationID, domain.PreparedCalls) {
	t.Helper()
	g.next++
	call := mustOpenMeteringCall(t, g.meteringFixture, g.next, domain.CallSandboxRun)
	prepared, err := g.store.PrepareCalls(context.Background(), prepareOneRequest(g.meteringFixture, call, g.next, domain.PhysicalLocalArtifactWrite, domain.BudgetArtifactPhysicalNewBytes, 64))
	if err != nil {
		t.Fatal(err)
	}
	physical, reservation := prepared.PhysicalCalls[0], prepared.Reservations[0]
	id := domain.ArtifactDeclarationID(fmt.Sprintf("decl_%032x", g.next))
	declaration := domain.ArtifactDeclarationRecord{ID: id, RunID: g.runID, StageName: g.stage, AttemptID: g.attemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, ReservationID: reservation.ID, ReservationSubkey: reservation.Subkey, MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: domain.SafeRelPath(fmt.Sprintf("out-%d.txt", g.next)), MaxBytes: 64, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "gc-test"}, CreatedAt: g.now}
	if err := g.store.CreateArtifactDeclaration(context.Background(), declaration); err != nil {
		t.Fatal(err)
	}
	return id, prepared
}

func (g *garbageFixture) publish(t *testing.T, data string) domain.PendingArtifact {
	t.Helper()
	id, prepared := g.declare(t)
	session, err := artifactsession.NewPreparedArtifactSession(g.store, g.blobs, prepared)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.Prepare(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	pending, err := writer.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return pending
}

func (g *garbageFixture) release(t *testing.T, pending domain.PendingArtifact) {
	t.Helper()
	if err := g.store.ReleaseArtifact(context.Background(), pending.WriterTokenID); err != nil {
		t.Fatal(err)
	}
}

func (g *garbageFixture) assertRemoved(t *testing.T, ref domain.BlobRef, generation int64) {
	t.Helper()
	var state, gcState string
	var removed, verified sql.NullString
	var gotGeneration int64
	if err := g.store.db.QueryRow(`SELECT state,gc_state,verified_at,gc_removed_at,publication_generation FROM blobs WHERE digest=? AND size=?`, ref.Digest, ref.Size).Scan(&state, &gcState, &verified, &removed, &gotGeneration); err != nil {
		t.Fatal(err)
	}
	if state != "STAGING" || gcState != "NONE" || verified.Valid || !removed.Valid || gotGeneration != generation {
		t.Fatalf("removed blob: state=%s gc=%s verified=%v removed=%v generation=%d", state, gcState, verified, removed, gotGeneration)
	}
	canonical := filepath.Join(g.root, "artifacts", filepath.FromSlash(canonicalRelativePath(ref)))
	for _, path := range []string{canonical, g.blobs.TrashPath(ref)} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("removed file %s: %v", path, err)
		}
	}
	if err := g.store.checkIntegrity(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGarbageCollectionRetainsReleasedHistoryAndChargesEachPublication(t *testing.T) {
	ctx := context.Background()
	g := newGarbageFixture(t)
	first := g.publish(t, "reclaimed artifact")
	g.release(t, first)
	before := garbageHistory(t, g.store, first.WriterTokenID)
	report, err := g.maintenance.CollectGarbage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if report.Planned != 1 || report.Moved != 1 || report.Removed != 1 {
		t.Fatalf("collection report = %+v", report)
	}
	g.assertRemoved(t, first.Blob, 1)
	if got := garbageHistory(t, g.store, first.WriterTokenID); !reflect.DeepEqual(got, before) {
		t.Fatalf("collection changed released history:\nbefore=%v\nafter=%v", before, got)
	}
	if report, err := g.maintenance.CollectGarbage(ctx); err != nil || report != (domain.GCReport{}) {
		t.Fatalf("second collection = %+v, %v", report, err)
	}

	second := g.publish(t, "reclaimed artifact")
	duplicate := g.publish(t, "reclaimed artifact")
	for _, p := range []domain.PendingArtifact{second, duplicate} {
		if err := g.store.SealArtifact(ctx, p.WriterTokenID, p.Blob); err != nil {
			t.Fatalf("seal replay: %v", err)
		}
	}
	var generation int64
	var removed sql.NullString
	if err := g.store.db.QueryRow(`SELECT publication_generation,gc_removed_at FROM blobs WHERE digest=? AND size=?`, first.Blob.Digest, first.Blob.Size).Scan(&generation, &removed); err != nil {
		t.Fatal(err)
	}
	if generation != 2 || removed.Valid {
		t.Fatalf("republished generation=%d removed=%v", generation, removed)
	}
	for _, item := range []struct {
		pending domain.PendingArtifact
		want    int64
	}{{first, first.Blob.Size}, {second, first.Blob.Size}, {duplicate, 0}} {
		var got int64
		if err := g.store.db.QueryRow(`SELECT physical_new_bytes FROM blob_pins WHERE pin_id=?`, item.pending.PinID).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != item.want {
			t.Fatalf("pin %s physical bytes=%d, want %d", item.pending.PinID, got, item.want)
		}
	}
	var owners int
	if err := g.store.db.QueryRow(`SELECT count(*) FROM artifact_blob_publication_owners WHERE digest=? AND size=?`, first.Blob.Digest, first.Blob.Size).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if owners != 2 {
		t.Fatalf("publication owners=%d, want 2", owners)
	}
	if got := garbageHistory(t, g.store, first.WriterTokenID); !reflect.DeepEqual(got, before) {
		t.Fatal("republication changed original accounting history")
	}
	g.release(t, second)
	g.release(t, duplicate)
	if report, err := g.maintenance.CollectGarbage(ctx); err != nil || report.Removed != 1 {
		t.Fatalf("second generation collection=%+v, %v", report, err)
	}
	g.assertRemoved(t, first.Blob, 2)
}

func garbageHistory(t *testing.T, store *Store, token domain.ArtifactWriterTokenID) []string {
	t.Helper()
	var result []string
	queries := []string{
		`SELECT json_array(writer_token_id,declaration_id,run_id,state,final_digest,final_size,pin_id,created_at,opened_at,sealed_at,finalized_at,released_at) FROM artifact_writer_tokens WHERE writer_token_id=?`,
		`SELECT json_array(pin_id,writer_token_id,digest,size,state,created_at,released_at,physical_new_bytes) FROM blob_pins WHERE writer_token_id=?`,
		`SELECT json_array(h.pin_id,h.ordinal,h.state,h.changed_at) FROM blob_pin_history h JOIN blob_pins p ON p.pin_id=h.pin_id WHERE p.writer_token_id=? ORDER BY h.ordinal`,
		`SELECT json_array(r.reservation_id,r.upper_bound,r.settled_value,r.state,r.created_at,r.settled_at) FROM budget_reservations r JOIN artifact_declarations d ON d.reservation_id=r.reservation_id JOIN artifact_writer_tokens t ON t.declaration_id=d.declaration_id WHERE t.writer_token_id=?`,
	}
	for _, query := range queries {
		rows, err := store.db.Query(query, token)
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				t.Fatal(err)
			}
			result = append(result, value)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestGarbageCollectionCrashRecoveryAtEachDurableBoundary(t *testing.T) {
	for _, boundary := range []string{"plan", "rename", "unlink", "commit"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			g := newGarbageFixture(t)
			pending := g.publish(t, "crash recovery")
			g.release(t, pending)
			items, err := g.store.PlanGarbage(ctx)
			if err != nil || len(items) != 1 {
				t.Fatalf("plan=%v, %v", items, err)
			}
			if boundary != "plan" {
				if _, err := g.blobs.MoveToTrash(ctx, pending.Blob); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "unlink" || boundary == "commit" {
				if err := g.blobs.RemoveTrash(ctx, pending.Blob); err != nil {
					t.Fatal(err)
				}
			}
			if boundary == "commit" {
				if err := g.store.CommitGarbage(ctx, items[0], domain.GCCommitRemoved); err != nil {
					t.Fatal(err)
				}
			}
			// Reopen both adapters so recovery observes only durable database and
			// filesystem state, with no in-memory operation state carried over.
			path := g.store.config.Path
			if err := g.store.Close(); err != nil {
				t.Fatal(err)
			}
			if err := g.locks.Close(); err != nil {
				t.Fatal(err)
			}
			g.store = openRuntimeStore(t, path, clock.NewFake(testNow))
			g.openFiles(t)
			if _, err := g.maintenance.ReconcileTrash(ctx); err != nil {
				t.Fatal(err)
			}
			g.assertRemoved(t, pending.Blob, 1)
			if report, err := g.maintenance.ReconcileTrash(ctx); err != nil || report != (domain.GCReport{}) {
				t.Fatalf("repeated reconcile=%+v, %v", report, err)
			}
			newPending := g.publish(t, "crash recovery")
			g.release(t, newPending)
			if report, err := g.maintenance.CollectGarbage(ctx); err != nil || report.Removed != 1 {
				t.Fatalf("collection after recovery=%+v, %v", report, err)
			}
			g.assertRemoved(t, pending.Blob, 2)
		})
	}
}

func TestGarbageCollectionPreservesLiveReferences(t *testing.T) {
	for _, reference := range []string{"active pin", "releasable pin", "occurrence", "cache"} {
		t.Run(reference, func(t *testing.T) {
			ctx := context.Background()
			g := newGarbageFixture(t)
			pending := g.publish(t, "retained artifact")
			switch reference {
			case "releasable pin":
				if _, err := g.store.db.Exec(`UPDATE blob_pins SET state='RELEASABLE' WHERE pin_id=?`, pending.PinID); err != nil {
					t.Fatal(err)
				}
			case "occurrence":
				output := domain.SumBytes([]byte("gc stage output"))
				if _, err := g.store.FinishStage(ctx, domain.FinishStageCommand{RunID: g.runID, ExpectedRunVersion: 2, StageName: g.stage, AttemptID: g.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending}}, IdempotencyKey: meteringID("finish", "gc occurrence"), At: testNow.Add(time.Second)}); err != nil {
					t.Fatal(err)
				}
				g.release(t, pending)
			case "cache":
				key := domain.SumBytes([]byte("gc retained cache"))
				if _, err := g.store.db.Exec(`INSERT INTO cache_entries(cache_key_digest,kind,schema_version,policy_digest,input_digest,state,created_at) VALUES(?,'output','cpgen.cache/v1',?,?,'VALID',?)`, key, domain.SumBytes([]byte("policy")), pending.Blob.Digest, formatTime(testNow)); err != nil {
					t.Fatal(err)
				}
				if _, err := g.store.db.Exec(`INSERT INTO cache_blob_refs(cache_key_digest,digest,size,role,media_type,logical_path,provenance_json,provenance_digest) SELECT ?,?,?,role,media_type,logical_path,provenance_json,? FROM artifact_declarations WHERE declaration_id=(SELECT declaration_id FROM artifact_writer_tokens WHERE writer_token_id=?)`, key, pending.Blob.Digest, pending.Blob.Size, domain.SumBytes([]byte(`{"schema_version":"cpgen.artifact/v1","producer":"gc-test"}`)), pending.WriterTokenID); err != nil {
					t.Fatal(err)
				}
				g.release(t, pending)
			}
			if report, err := g.maintenance.CollectGarbage(ctx); err != nil || report != (domain.GCReport{}) {
				t.Fatalf("retained %s collection=%+v, %v", reference, report, err)
			}
			reader, err := g.blobs.OpenVerified(ctx, pending.Blob)
			if err != nil {
				t.Fatalf("retained blob unavailable: %v", err)
			}
			_ = reader.Close()
			if err := g.store.checkIntegrity(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestGarbageCommitRejectsWrongStateAndStalePublication(t *testing.T) {
	ctx := context.Background()
	g := newGarbageFixture(t)
	pending := g.publish(t, "generation fence")
	g.release(t, pending)
	items, err := g.store.PlanGarbage(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("plan=%v, %v", items, err)
	}
	original := items[0]
	wrongPath := original
	wrongPath.CanonicalRelativePath += ".wrong"
	if err := g.store.CommitGarbage(ctx, wrongPath, domain.GCCommitRemoved); err == nil {
		t.Fatal("accepted wrong canonical path")
	}
	if err := g.store.CommitGarbage(ctx, original, domain.GCCommit("invalid")); err == nil {
		t.Fatal("accepted invalid phase")
	}
	if err := g.store.CommitGarbage(ctx, original, domain.GCCommitRepair); err != nil {
		t.Fatal(err)
	}
	if err := g.store.CommitGarbage(ctx, original, domain.GCCommitRemoved); err == nil {
		t.Fatal("accepted non-DELETING blob")
	}
	if _, err := g.maintenance.CollectGarbage(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.store.CommitGarbage(ctx, original, domain.GCCommitRemoved); err != nil {
		t.Fatalf("same-generation removal replay: %v", err)
	}
	newPending := g.publish(t, "generation fence")
	for _, phase := range []domain.GCCommit{domain.GCCommitRemoved, domain.GCCommitRepair} {
		if err := g.store.CommitGarbage(ctx, original, phase); !errors.Is(err, ErrConsistency) {
			t.Fatalf("stale generation commit for READY blob and %s: %v", phase, err)
		}
	}
	g.release(t, newPending)
	items, err = g.store.PlanGarbage(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("new generation plan=%v, %v", items, err)
	}
	if items[0].PublicationGeneration != original.PublicationGeneration+1 {
		t.Fatalf("generation did not advance: old=%+v new=%+v", original, items[0])
	}
	for _, phase := range []domain.GCCommit{domain.GCCommitRemoved, domain.GCCommitRepair} {
		if err := g.store.CommitGarbage(ctx, original, phase); err == nil {
			t.Fatalf("accepted stale generation for %s", phase)
		}
	}
	if _, err := g.maintenance.ReconcileTrash(ctx); err != nil {
		t.Fatal(err)
	}
	g.assertRemoved(t, pending.Blob, 2)
}

func TestGarbageTombstoneRejectsInvalidDatabaseTransitions(t *testing.T) {
	ctx := context.Background()
	g := newGarbageFixture(t)
	pending := g.publish(t, "guarded garbage")
	mustReject := func(query string, args ...any) {
		t.Helper()
		if _, err := g.store.db.Exec(query, args...); err == nil {
			t.Fatalf("accepted invalid transition: %s", query)
		}
	}
	mustReject(`UPDATE blobs SET gc_state='DELETING' WHERE digest=? AND size=?`, pending.Blob.Digest, pending.Blob.Size)
	mustReject(`UPDATE blobs SET publication_generation=publication_generation+1 WHERE digest=? AND size=?`, pending.Blob.Digest, pending.Blob.Size)
	g.release(t, pending)
	items, err := g.store.PlanGarbage(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("plan=%v, %v", items, err)
	}
	id, _ := g.declare(t)
	_, token, err := g.store.PrepareArtifact(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := g.store.OpenArtifactWriter(ctx, token.ID); err != nil {
		t.Fatal(err)
	}
	if err := g.store.SealArtifact(ctx, token.ID, pending.Blob); err == nil {
		t.Fatal("writer sealed a DELETING blob")
	}
	if err := g.store.FinalizeArtifact(ctx, pending.WriterTokenID, pending.Blob); !errors.Is(err, ErrConsistency) {
		t.Fatalf("finalize DELETING blob: %v", err)
	}
	if err := g.store.QuarantineBlob(ctx, pending.Blob); !errors.Is(err, ErrConsistency) {
		t.Fatalf("quarantine DELETING blob: %v", err)
	}
	var tokenState string
	if err := g.store.db.QueryRow(`SELECT state FROM artifact_writer_tokens WHERE writer_token_id=?`, token.ID).Scan(&tokenState); err != nil {
		t.Fatal(err)
	}
	if tokenState != "OPEN" {
		t.Fatalf("rejected seal changed writer state to %s", tokenState)
	}
	if _, err := g.maintenance.ReconcileTrash(ctx); err != nil {
		t.Fatal(err)
	}
	if err := g.store.FinalizeArtifact(ctx, pending.WriterTokenID, pending.Blob); !errors.Is(err, ErrConsistency) {
		t.Fatalf("finalize tombstone: %v", err)
	}
	if err := g.store.QuarantineBlob(ctx, pending.Blob); !errors.Is(err, ErrConsistency) {
		t.Fatalf("quarantine tombstone: %v", err)
	}
	g.assertRemoved(t, pending.Blob, 1)
	mustReject(`UPDATE blobs SET state='READY',verified_at=? WHERE digest=? AND size=?`, formatTime(testNow), pending.Blob.Digest, pending.Blob.Size)
	mustReject(`UPDATE blobs SET gc_removed_at=NULL WHERE digest=? AND size=?`, pending.Blob.Digest, pending.Blob.Size)
	mustReject(`UPDATE blobs SET gc_state='DELETING' WHERE digest=? AND size=?`, pending.Blob.Digest, pending.Blob.Size)
	mustReject(`UPDATE blobs SET gc_removed_at=NULL,publication_generation=publication_generation+2 WHERE digest=? AND size=?`, pending.Blob.Digest, pending.Blob.Size)
	mustReject(`DELETE FROM blobs WHERE digest=? AND size=?`, pending.Blob.Digest, pending.Blob.Size)
	if err := g.store.SealArtifact(ctx, token.ID, pending.Blob); err != nil {
		t.Fatalf("cannot seal next publication: %v", err)
	}
	if err := g.store.SealArtifact(ctx, token.ID, pending.Blob); err != nil {
		t.Fatalf("SEALED replay: %v", err)
	}
	var generation int64
	if err := g.store.db.QueryRow(`SELECT publication_generation FROM blobs WHERE digest=? AND size=?`, pending.Blob.Digest, pending.Blob.Size).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("SEALED replay advanced generation to %d", generation)
	}
	if err := g.store.checkIntegrity(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestGarbageCollectionRejectsCacheReferenceAfterPlanning(t *testing.T) {
	ctx := context.Background()
	g := newGarbageFixture(t)
	pending := g.publish(t, "late cache reference")
	g.release(t, pending)
	key := domain.SumBytes([]byte("gc late cache"))
	if _, err := g.store.db.Exec(`INSERT INTO cache_entries(cache_key_digest,kind,schema_version,policy_digest,input_digest,state,created_at) VALUES(?,'output','cpgen.cache/v1',?,?,'VALID',?)`, key, domain.SumBytes([]byte("policy")), pending.Blob.Digest, formatTime(testNow)); err != nil {
		t.Fatal(err)
	}
	query := `INSERT INTO cache_blob_refs(cache_key_digest,digest,size,role,media_type,logical_path,provenance_json,provenance_digest)
		SELECT ?,?,?,role,media_type,logical_path,provenance_json,? FROM artifact_declarations
		WHERE declaration_id=(SELECT declaration_id FROM artifact_writer_tokens WHERE writer_token_id=?)`
	args := []any{key, pending.Blob.Digest, pending.Blob.Size, domain.SumBytes([]byte(`{"schema_version":"cpgen.artifact/v1","producer":"gc-test"}`)), pending.WriterTokenID}
	// Prove the reference is valid before collection, then roll it back so
	// the exact same insert can race a durable deletion plan.
	tx, err := g.store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		_ = tx.Rollback()
		t.Fatalf("control cache insert: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if items, err := g.store.PlanGarbage(ctx); err != nil || len(items) != 1 {
		t.Fatalf("plan=%v, %v", items, err)
	}
	if _, err := g.store.db.ExecContext(ctx, query, args...); err == nil {
		t.Fatal("cache reference attached after deletion plan")
	}
	if _, err := g.maintenance.ReconcileTrash(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.db.ExecContext(ctx, query, args...); err == nil {
		t.Fatal("cache reference attached to removed publication")
	}
	g.assertRemoved(t, pending.Blob, 1)
}
