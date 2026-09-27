package sqlite

import (
	"context"
	"testing"

	"cpgen/internal/domain"
)

func TestReadWorkbenchRunReturnsCompleteSingleSnapshot(t *testing.T) {
	f := newMeteringFixture(t, "e1", domain.BudgetLimits{MaxLLMCalls: 7, MaxPackageBytes: 4096, MaxMutationsPerStage: 2})
	got, err := f.store.ReadWorkbenchRun(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Run.RunID != f.runID || got.Run.Version != 2 || got.Budget.Limits.MaxLLMCalls != 7 || got.Budget.Limits.MaxPackageBytes != 4096 || got.Budget.Limits.MaxMutationsPerStage != 2 {
		t.Fatalf("run/budget not read as expected: run=%+v budget=%+v", got.Run, got.Budget)
	}
	if len(got.Stages) == 0 || len(got.RecentEvents) != 2 || got.BeforeVersion != 1 || got.RecentEvents[0].Version != 1 || got.RecentEvents[1].Version != 2 {
		t.Fatalf("stage or event projection incomplete: stages=%+v events=%+v cursor=%d", got.Stages, got.RecentEvents, got.BeforeVersion)
	}
	if got.RecentEvents[0].OccurredAt.IsZero() || !got.RecentEvents[0].OccurredAt.Equal(testNow.UTC()) {
		t.Fatalf("event timestamp lost: %+v", got.RecentEvents[0])
	}
}

func TestReadWorkbenchArtifactRejectsCrossRunAndUnpublishedOccurrence(t *testing.T) {
	f := newMeteringFixture(t, "e3", domain.BudgetLimits{MaxLLMCalls: 1})
	otherID := domain.RunID("run_000000000000000000000000000000e2")
	if otherID == f.runID {
		t.Fatal("invalid test run ids")
	}
	if _, err := f.store.ReadWorkbenchArtifact(context.Background(), otherID, "occurrence_00000000000000000000000000000001"); err == nil {
		t.Fatal("cross-run/nonexistent occurrence was accepted")
	}
	// A fabricated ID must never fall through to a blob lookup by path/digest.
	if _, err := f.store.ReadWorkbenchArtifact(context.Background(), f.runID, domain.ArtifactOccurrenceID("occurrence_00000000000000000000000000000002")); err == nil {
		t.Fatal("uncommitted occurrence was accepted")
	}
}
