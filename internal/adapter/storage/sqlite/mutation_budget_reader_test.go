package sqlite

import (
	"context"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestMutationBudgetReaderUsesSharedStageQuotaWithoutCreatingAccounts(t *testing.T) {
	f := newMeteringFixture(t, "c1", testCreateRunRequest(testRunID, testNow, 30*time.Second).BudgetLimits)
	ctx := context.Background()
	initial, err := f.store.ReadMutationBudget(ctx, f.runID, f.stage)
	if err != nil || initial.Validate() != nil || initial.Limit != 2 || initial.Claimed != 0 || initial.AccountVersion != 0 || initial.Remaining() != 2 {
		t.Fatalf("initial=%+v %v", initial, err)
	}
	var accounts int
	if err := f.store.db.QueryRow(`SELECT count(*) FROM mutation_stage_accounts WHERE run_id=?`, f.runID).Scan(&accounts); err != nil || accounts != 0 {
		t.Fatalf("read created account: %d %v", accounts, err)
	}
	for i, kind := range []domain.MutationKind{domain.MutationContent, domain.MutationMetadata} {
		claim := domain.MutationClaimRequest{RunID: f.runID, StageName: f.stage, ScopeDigest: domain.SumBytes([]byte("scope")), SourceBatchDigest: domain.SumBytes([]byte("batch")), Ordinal: int64(i + 1), LimitSnapshot: 2, Kind: kind, IntentDigest: domain.SumBytes([]byte(kind)), At: f.now}
		if _, err := f.store.ClaimMutation(ctx, claim); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.ClaimMutation(ctx, claim); err != nil {
			t.Fatal(err)
		}
		read, err := f.store.ReadMutationBudget(ctx, f.runID, f.stage)
		if err != nil || read.Validate() != nil || read.Claimed != int64(i+1) || read.Remaining() != int64(1-i) || read.AccountVersion <= 0 || read.RunVersion != initial.RunVersion {
			t.Fatalf("quota=%+v %v", read, err)
		}
	}
	if _, err := f.store.ReadMutationBudget(ctx, f.runID, "not_in_graph"); err == nil {
		t.Fatal("foreign stage read admitted")
	}
}

func TestMutationBudgetReaderRejectsDivergentQuotaAndClaimCounts(t *testing.T) {
	for _, table := range []string{"mutation_stage_accounts", "mutation_accounts"} {
		t.Run(table, func(t *testing.T) {
			f := newMeteringFixture(t, "c2", testCreateRunRequest(testRunID, testNow, 30*time.Second).BudgetLimits)
			claim := domain.MutationClaimRequest{RunID: f.runID, StageName: f.stage, ScopeDigest: domain.SumBytes([]byte("scope")), SourceBatchDigest: domain.SumBytes([]byte("batch")), Ordinal: 1, LimitSnapshot: 2, Kind: domain.MutationContent, IntentDigest: domain.SumBytes([]byte("intent")), At: f.now}
			if _, err := f.store.ClaimMutation(context.Background(), claim); err != nil {
				t.Fatal(err)
			}
			// The controlled table names are the two persisted quota projections.
			if _, err := f.store.db.Exec(`UPDATE `+table+` SET claimed_value=2,account_version=account_version+1 WHERE run_id=?`, f.runID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.ReadMutationBudget(context.Background(), f.runID, f.stage); err == nil {
				t.Fatal("inconsistent quota was accepted")
			}
		})
	}
}
