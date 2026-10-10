package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	artifactsession "cpgen/internal/artifact"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestArtifactSessionPreservesRecoveredSealedWriter(t *testing.T) {
	for _, action := range []string{"canceled finalize", "abort", "release unused"} {
		t.Run(action, func(t *testing.T) {
			f := newSealedArtifactFixture(t)
			session, err := artifactsession.NewPreparedArtifactSession(f.store, f.blobs, f.prepared)
			if err != nil {
				t.Fatal(err)
			}
			writer, err := session.Prepare(context.Background(), f.declaration.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch action {
			case "canceled finalize":
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, err := writer.Finalize(ctx); !errors.Is(err, context.Canceled) {
					t.Fatalf("Finalize = %v, want context.Canceled", err)
				}
			case "abort":
				if err := writer.Abort(context.Background()); err == nil {
					t.Fatal("Abort accepted a recovered SEALED writer")
				}
			case "release unused":
				if err := session.ReleaseUnused(context.Background()); err == nil {
					t.Fatal("ReleaseUnused accepted a recovered SEALED writer")
				}
			}
			f.assertRetained(t)
			f.assertFinalizes(t, writer)
		})
	}
}

func TestArtifactSessionPreservesSealedWriterWhenResumeIsCanceled(t *testing.T) {
	f := newSealedArtifactFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel at the durable-read/filesystem boundary, after Prepare has passed
	// its initial context check but before ResumeStaged verifies the bytes.
	ledger := &cancelAfterArtifactRead{Store: f.store, cancel: cancel}
	session, err := artifactsession.NewPreparedArtifactSession(ledger, f.blobs, f.prepared)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Prepare(ctx, f.declaration.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare = %v, want context.Canceled", err)
	}
	f.assertRetained(t)
	writer, err := session.Prepare(context.Background(), f.declaration.ID)
	if err != nil {
		t.Fatalf("retry canceled recovery: %v", err)
	}
	f.assertFinalizes(t, writer)
}

type cancelAfterArtifactRead struct {
	*Store
	cancel context.CancelFunc
}

func (s *cancelAfterArtifactRead) PrepareArtifact(ctx context.Context, id domain.ArtifactDeclarationID) (domain.ArtifactDeclarationRecord, domain.ArtifactWriterToken, error) {
	declaration, token, err := s.Store.PrepareArtifact(ctx, id)
	if err == nil {
		s.cancel()
	}
	return declaration, token, err
}

type sealedArtifactFixture struct {
	store       *Store
	blobs       *blob.Store
	prepared    domain.PreparedCalls
	declaration domain.ArtifactDeclarationRecord
	token       domain.ArtifactWriterToken
	stagingPath string
	data        []byte
}

func newSealedArtifactFixture(t *testing.T) sealedArtifactFixture {
	t.Helper()
	ctx := context.Background()
	metering := newMeteringFixture(t, "e9", testCreateRunRequest(testRunID, testNow, time.Minute).BudgetLimits)
	declaration, prepared := prepareReaderArtifact(t, metering, 1)
	_, token, err := metering.store.PrepareArtifact(ctx, declaration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := metering.store.OpenArtifactWriter(ctx, token.ID); err != nil {
		t.Fatal(err)
	}
	mustBeginDispatch(t, metering, prepared.Call.ID, declaration.AttemptCallID, "sealed_recovery")
	root := filepath.Join(t.TempDir(), "artifacts")
	blobs, err := blob.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := blobs.Prepare(ctx,
		port.ArtifactDeclaration{MediaType: declaration.MediaType, Role: declaration.Role, LogicalPath: declaration.LogicalPath, MaxBytes: declaration.MaxBytes, Provenance: declaration.Provenance},
		blob.WriterIdentity{CallID: declaration.AttemptCallID, ReservationID: declaration.ReservationID, WriterTokenID: token.ID, PinID: token.PinID})
	if err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"response":"retained provider output"}`)
	if _, err := writer.Write(data); err != nil {
		t.Fatal(err)
	}
	pending, err := blob.Stage(ctx, writer)
	if err != nil {
		t.Fatal(err)
	}
	if err := metering.store.SealArtifact(ctx, token.ID, pending.Blob); err != nil {
		t.Fatal(err)
	}
	prepared, err = metering.store.LoadCall(ctx, prepared.Call.ID)
	if err != nil {
		t.Fatal(err)
	}
	return sealedArtifactFixture{store: metering.store, blobs: blobs, prepared: prepared, declaration: declaration,
		token: token, stagingPath: filepath.Join(root, "tmp", string(token.ID)+".stage"), data: data}
}

func (f sealedArtifactFixture) assertRetained(t *testing.T) {
	t.Helper()
	_, token, err := f.store.ReadArtifactWriter(context.Background(), f.declaration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if token.State != domain.ArtifactWriterSealed {
		t.Fatalf("writer state = %s, want SEALED", token.State)
	}
	var pinState string
	if err := f.store.db.QueryRow(`SELECT state FROM blob_pins WHERE pin_id = ?`, f.token.PinID).Scan(&pinState); err != nil {
		t.Fatal(err)
	}
	if pinState != "ACTIVE" {
		t.Fatalf("pin state = %s, want ACTIVE", pinState)
	}
	data, err := os.ReadFile(f.stagingPath)
	if err != nil || string(data) != string(f.data) {
		t.Fatalf("retained staged bytes = %q, %v", data, err)
	}
}

func (f sealedArtifactFixture) assertFinalizes(t *testing.T, writer port.ArtifactWriter) {
	t.Helper()
	ctx := context.Background()
	pending, err := writer.Finalize(ctx)
	if err != nil {
		t.Fatalf("finalize retained writer: %v", err)
	}
	stored, err := f.store.ReadPendingArtifact(ctx, f.declaration.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.WriterTokenID != f.token.ID || stored.PinID != f.token.PinID || stored.Blob != pending.Blob {
		t.Fatalf("recovered receipt changed identity: %+v", stored)
	}
	data, err := artifactsession.ReadVerified(ctx, f.blobs, stored.Blob, f.declaration.MaxBytes)
	if err != nil || string(data) != string(f.data) {
		t.Fatalf("recovered bytes = %q, %v", data, err)
	}
}
