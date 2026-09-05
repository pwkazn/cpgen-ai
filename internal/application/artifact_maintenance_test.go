package application

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
)

type fakeGCMetadata struct {
	items   []domain.GCItem
	commits []domain.GCCommit
}

func (f *fakeGCMetadata) PlanGarbage(context.Context) ([]domain.GCItem, error) {
	return append([]domain.GCItem(nil), f.items...), nil
}
func (f *fakeGCMetadata) ListDeleting(context.Context) ([]domain.GCItem, error) {
	return append([]domain.GCItem(nil), f.items...), nil
}
func (f *fakeGCMetadata) CommitGarbage(_ context.Context, _ domain.GCItem, phase domain.GCCommit) error {
	f.commits = append(f.commits, phase)
	return nil
}

func TestArtifactMaintenanceHoldsExclusiveGuardThroughFilesystemCommit(t *testing.T) {
	root := t.TempDir()
	store, err := blob.NewStore(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	declaration := port.ArtifactDeclaration{MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: "out", MaxBytes: 8, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "maintenance-test"}}
	writer, err := store.Prepare(context.Background(), declaration, blob.WriterIdentity{CallID: "call_00000000000000000000000000000031", ReservationID: "res_00000000000000000000000000000031", WriterTokenID: "writer_00000000000000000000000000000031", PinID: "pin_00000000000000000000000000000031"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("gc")); err != nil {
		t.Fatal(err)
	}
	pending, err := writer.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	hex := string(pending.Blob.Digest)[len("sha256:"):]
	lockRoot := filepath.Join(root, "locks")
	if err := os.Mkdir(lockRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	locks, err := runlock.NewManager(lockRoot, runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer locks.Close()
	metadata := &fakeGCMetadata{items: []domain.GCItem{{Ref: pending.Blob, CanonicalRelativePath: filepath.ToSlash(filepath.Join("blobs", "sha256", hex[:2], hex))}}}
	maintenance, err := NewArtifactMaintenance(locks, metadata, store)
	if err != nil {
		t.Fatal(err)
	}
	report, err := maintenance.CollectGarbage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Removed != 1 || len(metadata.commits) != 1 || metadata.commits[0] != domain.GCCommitRemoved {
		t.Fatalf("report=%+v commits=%v", report, metadata.commits)
	}
	if _, err := store.OpenVerified(context.Background(), pending.Blob); err == nil {
		t.Fatal("garbage blob remains readable")
	}
}

func TestArtifactMaintenanceReconcileTrashRejectsReplacementWithoutFollowingIt(t *testing.T) {
	store, err := blob.NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.BlobRef{Digest: domain.SumBytes([]byte("reconcile replacement")), Size: int64(len("reconcile replacement"))}
	external := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(external, []byte("must not be deleted"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, store.TrashPath(ref)); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	lockRoot := filepath.Join(t.TempDir(), "locks")
	if err := os.Mkdir(lockRoot, 0700); err != nil {
		t.Fatal(err)
	}
	locks, err := runlock.NewManager(lockRoot, runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer locks.Close()
	metadata := &fakeGCMetadata{items: []domain.GCItem{{Ref: ref}}}
	maintenance, err := NewArtifactMaintenance(locks, metadata, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.ReconcileTrash(context.Background()); err == nil {
		t.Fatal("ReconcileTrash followed a replaced trash symlink")
	}
	if data, err := os.ReadFile(external); err != nil || string(data) != "must not be deleted" {
		t.Fatalf("replacement target = %q, %v", data, err)
	}
	if len(metadata.commits) != 0 {
		t.Fatalf("replacement attack committed metadata phases: %v", metadata.commits)
	}
}
