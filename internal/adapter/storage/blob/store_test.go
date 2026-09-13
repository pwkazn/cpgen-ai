package blob

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestBlobStoreRejectsUnsafeRootedPathAndVerifiesPublishedBytes(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	declaration := port.ArtifactDeclaration{
		MediaType: "text/plain", Role: domain.ArtifactOutput,
		LogicalPath: "output.txt", MaxBytes: 32,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"},
	}
	writer, err := store.Prepare(context.Background(), declaration, WriterIdentity{
		CallID: "call_00000000000000000000000000000001", ReservationID: "res_00000000000000000000000000000001",
		WriterTokenID: "writer_00000000000000000000000000000001", PinID: "pin_00000000000000000000000000000001",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	pending, err := writer.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.OpenVerified(context.Background(), pending.Blob)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(data) != "hello" {
		t.Fatalf("read verified = %q, %v", data, err)
	}
}

func TestOpenVerifiedRejectsSymlinkedCanonicalParent(t *testing.T) {
	if runtimeGOOS := os.Getenv("GOOS"); runtimeGOOS == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.BlobRef{Digest: domain.SumBytes([]byte("outside")), Size: int64(len("outside"))}
	hex := strings.TrimPrefix(string(ref.Digest), "sha256:")
	shard := filepath.Join(store.blobs, "sha256", hex[:2])
	if err := os.MkdirAll(filepath.Dir(shard), 0700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, hex), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, shard); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	reader, err := store.OpenVerified(context.Background(), ref)
	if reader != nil {
		_ = reader.Close()
	}
	if err == nil {
		t.Fatal("OpenVerified followed a symlinked canonical parent")
	}
}

func TestBlobWriterRejectsLimitAndReuse(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	declaration := port.ArtifactDeclaration{MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: "output.txt", MaxBytes: 3, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}}
	writer, err := store.Prepare(context.Background(), declaration, WriterIdentity{CallID: "call_00000000000000000000000000000002", ReservationID: "res_00000000000000000000000000000002", WriterTokenID: "writer_00000000000000000000000000000002", PinID: "pin_00000000000000000000000000000002"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("1234")); err == nil {
		t.Fatal("limit+1 write succeeded")
	}
	if _, err := writer.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Finalize(context.Background()); err == nil {
		t.Fatal("writer finalized twice")
	}
}

func testArtifactDeclaration() port.ArtifactDeclaration {
	return port.ArtifactDeclaration{MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: "output.txt", MaxBytes: 64,
		Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}}
}

func testWriterIdentity(suffix string) WriterIdentity {
	value := strings.Repeat("0", 30) + suffix
	return WriterIdentity{
		CallID:        domain.AttemptCallID("call_" + value),
		ReservationID: domain.ReservationID("res_" + value),
		WriterTokenID: domain.ArtifactWriterTokenID("writer_" + value),
		PinID:         domain.BlobPinID("pin_" + value),
	}
}

func TestResumeStagedWriterReplaysAfterCrash(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	identity := testWriterIdentity("81")
	original, err := store.Prepare(context.Background(), testArtifactDeclaration(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := original.Write([]byte("recoverable")); err != nil {
		t.Fatal(err)
	}
	staged, ok := original.(*writer)
	if !ok {
		t.Fatal("Prepare did not return the private staged writer")
	}
	pending, err := staged.stage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := store.ResumeStaged(context.Background(), testArtifactDeclaration(), identity, pending.Blob)
	if err != nil {
		t.Fatal(err)
	}
	finished, err := resumed.Finalize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if finished.Blob != pending.Blob {
		t.Fatalf("resumed blob = %+v, want %+v", finished.Blob, pending.Blob)
	}
}

func TestResumeStagedWriterRecoversLinkBeforeTemporaryCleanup(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	identity := testWriterIdentity("85")
	original, err := store.Prepare(context.Background(), testArtifactDeclaration(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := original.Write([]byte("two links")); err != nil {
		t.Fatal(err)
	}
	staged := original.(*writer)
	pending, err := staged.stage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := store.canonicalPath(pending.Blob)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(staged.tempPath, target); err != nil {
		t.Fatal(err)
	}
	resumed, err := store.ResumeStaged(context.Background(), testArtifactDeclaration(), identity, pending.Blob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(staged.tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovered staging path error = %v, want not exist", err)
	}
	reader, err := store.OpenVerified(context.Background(), pending.Blob)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
}

func TestPrepareRejectsPreexistingStagingSymlink(t *testing.T) {
	if os.Getenv("GOOS") == "windows" {
		t.Skip("symlink creation requires elevated privileges on Windows")
	}
	store, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	identity := testWriterIdentity("86")
	tempPath := store.stagingPath(identity.WriterTokenID)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("must not be truncated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, tempPath); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := store.Prepare(context.Background(), testArtifactDeclaration(), identity); err == nil {
		t.Fatal("Prepare followed pre-existing staging symlink")
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "must not be truncated" {
		t.Fatalf("outside symlink target changed to %q", data)
	}
}

func TestPrepareRejectsReplacedStagingRoot(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	identity := testWriterIdentity("87")
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.MkdirAll(outside, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(store.temporary, store.temporary+".private"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, store.temporary); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := store.Prepare(context.Background(), testArtifactDeclaration(), identity); err == nil {
		t.Fatal("Prepare followed a replaced temporary root")
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("outside temporary root was modified: %v", entries)
	}
}

func TestQuarantineSerializesPublicationForDigest(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	ref := domain.BlobRef{Digest: domain.SumBytes([]byte("replacement")), Size: int64(len("replacement"))}
	target := store.canonicalPath(ref)
	if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("corrupt!!!!"), 0600); err != nil {
		t.Fatal(err)
	}
	firstLedgerCall := make(chan struct{})
	releaseLedger := make(chan struct{})
	var ledgerCalls int
	if err := store.AttachCorruptionLedger(corruptionLedgerFunc(func(context.Context, domain.BlobRef) error {
		ledgerCalls++
		if ledgerCalls == 1 {
			close(firstLedgerCall)
			<-releaseLedger
		}
		return nil
	})); err != nil {
		t.Fatal(err)
	}
	readerDone := make(chan error, 1)
	go func() {
		reader, openErr := store.OpenVerified(context.Background(), ref)
		if reader != nil {
			_ = reader.Close()
		}
		readerDone <- openErr
	}()
	<-firstLedgerCall

	writerValue, err := store.Prepare(context.Background(), testArtifactDeclaration(), testWriterIdentity("88"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writerValue.Write([]byte("replacement")); err != nil {
		t.Fatal(err)
	}
	staged := writerValue.(*writer)
	if _, err := staged.stage(context.Background()); err != nil {
		t.Fatal(err)
	}
	publishDone := make(chan error, 1)
	go func() {
		_, publishErr := staged.publish(context.Background())
		publishDone <- publishErr
	}()
	select {
	case err := <-publishDone:
		t.Fatalf("publication completed while quarantine ledger was blocked: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseLedger)
	if err := <-readerDone; !errors.Is(err, ErrBlobCorrupt) {
		t.Fatalf("OpenVerified error = %v, want ErrBlobCorrupt", err)
	}
	if err := <-publishDone; err != nil {
		t.Fatalf("replacement publication: %v", err)
	}
	verified, err := store.OpenVerified(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	_ = verified.Close()
}

type corruptionLedgerFunc func(context.Context, domain.BlobRef) error

func (f corruptionLedgerFunc) QuarantineBlob(ctx context.Context, ref domain.BlobRef) error {
	return f(ctx, ref)
}

func TestConcurrentSameDigestPublishersDoNotQuarantineTarget(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	decl := testArtifactDeclaration()
	identities := []WriterIdentity{testWriterIdentity("82"), testWriterIdentity("83")}
	writers := make([]*writer, 2)
	for i, identity := range identities {
		artifactWriter, prepareErr := store.Prepare(context.Background(), decl, identity)
		if prepareErr != nil {
			t.Fatal(prepareErr)
		}
		if _, writeErr := artifactWriter.Write([]byte("same digest")); writeErr != nil {
			t.Fatal(writeErr)
		}
		writers[i] = artifactWriter.(*writer)
		if _, stageErr := writers[i].stage(context.Background()); stageErr != nil {
			t.Fatal(stageErr)
		}
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(writers))
	for _, item := range writers {
		wg.Add(1)
		go func(item *writer) {
			defer wg.Done()
			_, publishErr := item.publish(context.Background())
			errs <- publishErr
		}(item)
	}
	wg.Wait()
	close(errs)
	for publishErr := range errs {
		if publishErr != nil {
			t.Fatal(publishErr)
		}
	}
	ref := domain.BlobRef{Digest: domain.SumBytes([]byte("same digest")), Size: int64(len("same digest"))}
	reader, err := store.OpenVerified(context.Background(), ref)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
}

func TestPublishFsyncFailureRetainsRecoverableCanonical(t *testing.T) {
	store, err := NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	identity := testWriterIdentity("84")
	artifactWriter, err := store.Prepare(context.Background(), testArtifactDeclaration(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := artifactWriter.Write([]byte("fsync boundary")); err != nil {
		t.Fatal(err)
	}
	staged := artifactWriter.(*writer)
	pending, err := staged.stage(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	originalSync := syncDirectoryFn
	sentinel := errors.New("injected directory fsync failure")
	syncDirectoryFn = func(string) error { return sentinel }
	_, err = staged.publish(context.Background())
	syncDirectoryFn = originalSync
	if !errors.Is(err, sentinel) {
		t.Fatalf("publish error = %v, want fsync failure", err)
	}
	resumed, err := store.ResumeStaged(context.Background(), testArtifactDeclaration(), identity, pending.Blob)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resumed.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
}
