package sqlite

import (
	"context"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestRunViewBudgetSnapshotRetainsNonPhysicalLimits(t *testing.T) {
	limits := testCreateRunRequest(testRunID, testNow, 30*time.Second).BudgetLimits
	limits.MaxPackageBytes = 1234567
	limits.MaxMutationsPerStage = 3
	f := newMeteringFixture(t, "c3", limits)
	snapshot, err := f.store.BudgetSnapshot(context.Background(), f.runID)
	if err != nil || snapshot.Limits != limits {
		t.Fatalf("budget snapshot dropped frozen request limits: got=%+v want=%+v err=%v", snapshot.Limits, limits, err)
	}
	if _, exists := snapshot.Remaining[domain.BudgetDimension("MUTATIONS")]; exists {
		t.Fatal("logical mutation quota became a physical call dimension")
	}
}
