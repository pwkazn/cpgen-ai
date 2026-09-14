package artifact

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestPreparedArtifactSessionRejectsInvalidPreparedBundle(t *testing.T) {
	store, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewPreparedArtifactSession(&fakeArtifactLedger{}, store, domain.PreparedCalls{}); err == nil {
		t.Fatal("invalid prepared calls accepted")
	}
}

func TestPreparedArtifactSessionSealsBeforePublishing(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	runID := domain.RunID("run_00000000000000000000000000000081")
	stage := domain.StageName("generate")
	attempt := domain.AttemptID("attempt_00000000000000000000000000000081")
	callID := domain.CallRecordID("call_00000000000000000000000000000081")
	physicalID := domain.AttemptCallID("physical_00000000000000000000000000000081")
	reservationID := domain.ReservationID("res_00000000000000000000000000000081")
	declarationID := domain.ArtifactDeclarationID("decl_00000000000000000000000000000081")
	writerID := domain.ArtifactWriterTokenID("writer_00000000000000000000000000000081")
	pinID := domain.BlobPinID("pin_00000000000000000000000000000081")
	digest := domain.SumBytes([]byte("request"))
	policy := domain.SumBytes([]byte("policy"))
	retry := domain.RetryPolicy{MaxAttempts: 1, InitialBackoff: time.Second, MaxBackoff: time.Second, JitterSeedDigest: digest}
	preparedAt := now.Add(time.Second)
	prepared := domain.PreparedCalls{Call: domain.CallRecord{ID: callID, RunID: runID, StageName: stage, AttemptID: attempt, LogicalOperationID: "artifact", Kind: domain.CallSandboxRun, Provider: "test", RequestDigest: digest, PolicyDigest: policy, RetryPolicy: retry, IdempotencyKey: "logical_00000000000000000000000000000081", State: domain.CallRecordPrepared, OpenedAt: now, PreparedAt: &preparedAt}, PhysicalCalls: []domain.PhysicalCall{{ID: physicalID, CallRecordID: callID, RunID: runID, StageName: stage, AttemptID: attempt, Ordinal: 1, RetryGroup: "artifact", RetryOrdinal: 1, Kind: domain.PhysicalLocalArtifactWrite, Provider: "blob", RequestDigest: digest, IdempotencyKey: "physical_00000000000000000000000000000081", State: domain.PhysicalPrepared, PreparedAt: preparedAt}}, Reservations: []domain.BudgetReservation{{ID: reservationID, RunID: runID, StageName: stage, AttemptID: attempt, CallRecordID: callID, AttemptCallID: physicalID, Dimension: domain.BudgetArtifactPhysicalNewBytes, Subkey: "artifact", UpperBound: 32, State: domain.ReservationReserved, CreatedAt: preparedAt}}}
	declaration := domain.ArtifactDeclarationRecord{ID: declarationID, RunID: runID, StageName: stage, AttemptID: attempt, CallRecordID: callID, AttemptCallID: physicalID, ReservationID: reservationID, ReservationSubkey: "artifact", MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: "output.txt", MaxBytes: 32, Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.artifact/v1", Producer: "test"}, CreatedAt: preparedAt}
	ledger := &orderingArtifactLedger{declaration: declaration, token: domain.ArtifactWriterToken{ID: writerID, DeclarationID: declarationID, RunID: runID, State: domain.ArtifactWriterPrepared, PinID: pinID, CreatedAt: preparedAt}}
	store, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	ledger.store = store
	session, err := NewPreparedArtifactSession(ledger, store, prepared)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := session.Prepare(context.Background(), declarationID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Finalize(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ledger.events) != 3 || ledger.events[0] != "open" || ledger.events[1] != "seal" || ledger.events[2] != "finalize" {
		t.Fatalf("ledger events = %v", ledger.events)
	}
}

type orderingArtifactLedger struct {
	declaration domain.ArtifactDeclarationRecord
	token       domain.ArtifactWriterToken
	store       *blob.Store
	events      []string
}

func (l *orderingArtifactLedger) PrepareArtifact(context.Context, domain.ArtifactDeclarationID) (domain.ArtifactDeclarationRecord, domain.ArtifactWriterToken, error) {
	return l.declaration, l.token, nil
}
func (l *orderingArtifactLedger) OpenArtifactWriter(context.Context, domain.ArtifactWriterTokenID) error {
	l.events = append(l.events, "open")
	l.token.State = domain.ArtifactWriterOpen
	return nil
}
func (l *orderingArtifactLedger) SealArtifact(ctx context.Context, _ domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	l.events = append(l.events, "seal")
	if _, err := l.store.OpenVerified(ctx, ref); !errors.Is(err, blob.ErrBlobNotFound) {
		return errors.New("canonical artifact was published before seal")
	}
	return nil
}
func (l *orderingArtifactLedger) FinalizeArtifact(ctx context.Context, _ domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	l.events = append(l.events, "finalize")
	reader, err := l.store.OpenVerified(ctx, ref)
	if err != nil {
		return err
	}
	return reader.Close()
}
func (l *orderingArtifactLedger) ReleaseArtifact(context.Context, domain.ArtifactWriterTokenID) error {
	return nil
}

var _ port.ArtifactLedger = (*fakeArtifactLedger)(nil)

type fakeArtifactLedger struct{}

func (*fakeArtifactLedger) PrepareArtifact(context.Context, domain.ArtifactDeclarationID) (domain.ArtifactDeclarationRecord, domain.ArtifactWriterToken, error) {
	return domain.ArtifactDeclarationRecord{}, domain.ArtifactWriterToken{}, nil
}
func (*fakeArtifactLedger) OpenArtifactWriter(context.Context, domain.ArtifactWriterTokenID) error {
	return nil
}
func (*fakeArtifactLedger) SealArtifact(context.Context, domain.ArtifactWriterTokenID, domain.BlobRef) error {
	return nil
}
func (*fakeArtifactLedger) FinalizeArtifact(context.Context, domain.ArtifactWriterTokenID, domain.BlobRef) error {
	return nil
}
func (*fakeArtifactLedger) ReleaseArtifact(context.Context, domain.ArtifactWriterTokenID) error {
	return nil
}
