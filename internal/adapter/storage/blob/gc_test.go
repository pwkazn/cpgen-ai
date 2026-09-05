package blob

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestBlobTrashMoveAndDeleteArePrivateAndIdempotent(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.BlobRef{Digest: domain.SumBytes([]byte("gc")), Size: 2}
	if _, err := store.moveToTrash(context.Background(), ref); err == nil {
		t.Fatal("missing canonical blob moved without error")
	}
	if _, err := os.Stat(filepath.Join(store.root, "trash")); err != nil {
		t.Fatal(err)
	}
}

func TestBlobTrashMovesCanonicalBytesAndReconcilesIdempotently(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	declaration := port.ArtifactDeclaration{MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: "output.txt", MaxBytes: 32, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "gc-test"}}
	writer, err := store.Prepare(context.Background(), declaration, WriterIdentity{CallID: "call_00000000000000000000000000000011", ReservationID: "res_00000000000000000000000000000011", WriterTokenID: "writer_00000000000000000000000000000011", PinID: "pin_00000000000000000000000000000011"})
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
	moved, err := store.MoveToTrash(context.Background(), pending.Blob)
	if err != nil || !moved {
		t.Fatalf("move = %v, %v", moved, err)
	}
	if _, err := store.OpenVerified(context.Background(), pending.Blob); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("canonical still readable: %v", err)
	}
	trash, err := os.Open(store.TrashPath(pending.Blob))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(trash)
	_ = trash.Close()
	if err != nil || string(data) != "gc" {
		t.Fatalf("trash bytes = %q, %v", data, err)
	}
	if moved, err := store.MoveToTrash(context.Background(), pending.Blob); err != nil || moved {
		t.Fatalf("idempotent move = %v, %v", moved, err)
	}
	if err := store.RemoveTrash(context.Background(), pending.Blob); err != nil {
		t.Fatal(err)
	}
	if err := store.RemoveTrash(context.Background(), pending.Blob); err != nil {
		t.Fatal(err)
	}
}

func TestTrashReferencesRejectsPrivateTrashReplacement(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "outside")
	if err := os.Mkdir(external, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.trash); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, store.trash); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if _, err := store.TrashReferences(context.Background()); err == nil {
		t.Fatal("trash scan followed a replacement outside the private root")
	}
}
