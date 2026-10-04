package application_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type artifactSettlementStore struct {
	*sqlite.Store
	finalizeError error
	readError     error
	afterSeal     func() error
}

func (s *artifactSettlementStore) FinalizeArtifact(ctx context.Context, id domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	if err := s.finalizeError; err != nil {
		s.finalizeError = nil
		return err
	}
	return s.Store.FinalizeArtifact(ctx, id, ref)
}

func (s *artifactSettlementStore) ReadPendingArtifact(ctx context.Context, id domain.ArtifactDeclarationID) (domain.PendingArtifact, error) {
	if err := s.readError; err != nil {
		s.readError = nil
		return domain.PendingArtifact{}, err
	}
	return s.Store.ReadPendingArtifact(ctx, id)
}

func (s *artifactSettlementStore) SealArtifact(ctx context.Context, id domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	if err := s.Store.SealArtifact(ctx, id, ref); err != nil {
		return err
	}
	if hook := s.afterSeal; hook != nil {
		s.afterSeal = nil
		return hook()
	}
	return nil
}

func TestSandboxArtifactSettlementSurvivesPublicationRetry(t *testing.T) {
	for _, boundary := range []string{"metadata finalization", "durable receipt read"} {
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			f, store, sink, decl := newArtifactSettlementFixture(t)
			writer, err := sink.Prepare(ctx, decl)
			if err != nil {
				t.Fatal(err)
			}
			raw := []byte("physical-byte-owner")
			if _, err := writer.Write(raw); err != nil {
				t.Fatal(err)
			}
			injected := errors.New("transient artifact metadata failure")
			if boundary == "metadata finalization" {
				store.finalizeError = injected
			} else {
				store.readError = injected
			}
			if _, err := writer.Finalize(ctx); !errors.Is(err, injected) {
				t.Fatalf("first finalize = %v, want injected failure", err)
			}
			pending, err := writer.Finalize(ctx)
			if err != nil {
				t.Fatalf("retry finalize: %v", err)
			}
			if pending.PhysicalNewBytes != int64(len(raw)) {
				t.Fatalf("retry charged %d bytes, want %d", pending.PhysicalNewBytes, len(raw))
			}
			replayed, err := writer.Finalize(ctx)
			if err != nil || !reflect.DeepEqual(replayed, pending) {
				t.Fatalf("repeated finalize changed receipt: %+v, %v", replayed, err)
			}
			assertArtifactSettlementStage(t, f, []domain.PendingArtifact{pending}, int64(len(raw)))
		})
	}
}

func TestSandboxArtifactSettlementFollowsSealOwnership(t *testing.T) {
	ctx := context.Background()
	f, store, sink, firstDecl := newArtifactSettlementFixture(t)
	secondDecl := firstDecl
	secondDecl.LogicalPath = "compile/second-output"
	first, err := sink.Prepare(ctx, firstDecl)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sink.Prepare(ctx, secondDecl)
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte("identical concurrent output")
	for _, writer := range []port.ArtifactWriter{first, second} {
		if _, err := writer.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	// Reproduce an interleaving in which the first writer owns the durable
	// publication but the second writer links the same bytes first.
	var secondPending domain.PendingArtifact
	store.afterSeal = func() error {
		var err error
		secondPending, err = second.Finalize(ctx)
		return err
	}
	firstPending, err := first.Finalize(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if firstPending.PhysicalNewBytes != int64(len(raw)) || secondPending.PhysicalNewBytes != 0 {
		t.Fatalf("publication ownership charges = first %d, second %d", firstPending.PhysicalNewBytes, secondPending.PhysicalNewBytes)
	}
	assertArtifactSettlementStage(t, f, []domain.PendingArtifact{firstPending, secondPending}, int64(len(raw)))
}

func newArtifactSettlementFixture(t *testing.T) (coordinatorFixture, *artifactSettlementStore, *sandboxexec.ArtifactSink, port.ArtifactDeclaration) {
	t.Helper()
	f := newCoordinatorFixtureWithStages(t, "f4", domain.BudgetLimits{MaxArtifactBytes: 4096, MaxActiveTimeMilliseconds: 100000}, []domain.StageName{"prepare", "exercise"})
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	store := &artifactSettlementStore{Store: f.store}
	identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000f4", LogicalOperationID: "compile", Kind: domain.CallSandboxCompile, ScopeDigest: domain.SumBytes([]byte("compile")), ExpectedRunVersion: 2}
	sink, err := sandboxexec.NewArtifactSink(store, blobs, f.clock, identity)
	if err != nil {
		t.Fatal(err)
	}
	decl := port.ArtifactDeclaration{MediaType: "text/plain", Role: domain.ArtifactOutput, LogicalPath: "compile/output", MaxBytes: 1024, Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "docker"}}
	return f, store, sink, decl
}

func assertArtifactSettlementStage(t *testing.T, f coordinatorFixture, artifacts []domain.PendingArtifact, bytes int64) {
	t.Helper()
	ctx := context.Background()
	run, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	occurrences := make([]domain.PendingOccurrence, len(artifacts))
	for i := range artifacts {
		occurrences[i] = domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &artifacts[i]}
	}
	output := domain.SumBytes([]byte("settled artifact stage"))
	if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: run.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, Occurrences: occurrences, IdempotencyKey: coordinatorID("finish", "settled sandbox artifacts"), At: f.clock.Now()}); err != nil {
		t.Fatalf("attach settled artifacts: %v", err)
	}
	budget, err := f.store.BudgetSnapshot(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if budget.Remaining[domain.BudgetArtifactPhysicalNewBytes] != 4096-bytes {
		t.Fatalf("wrong artifact byte settlement: %+v", budget)
	}
}
