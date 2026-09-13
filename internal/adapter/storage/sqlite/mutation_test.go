package sqlite

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

func TestMutationClaimEnforcesImmutableStageLimitAndReplay(t *testing.T) {
	fixture := newMeteringFixture(t, "91", testCreateRunRequest(domain.RunID("run_00000000000000000000000000000091"), testNow, 30*time.Second).BudgetLimits)
	request := domain.MutationClaimRequest{RunID: fixture.runID, StageName: fixture.stage, ScopeDigest: domain.SumBytes([]byte("scope")), SourceBatchDigest: domain.SumBytes([]byte("sources")), Ordinal: 1, Limit: 2, Kind: domain.MutationContent, IntentDigest: domain.SumBytes([]byte("intent-1")), At: fixture.now}
	grant, err := fixture.store.ClaimMutation(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := fixture.store.ClaimMutation(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if grant != replay {
		t.Fatalf("replay grant = %+v, want %+v", replay, grant)
	}
	request.Ordinal, request.IntentDigest = 2, domain.SumBytes([]byte("intent-2"))
	if _, err := fixture.store.ClaimMutation(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	request.Ordinal, request.IntentDigest = 3, domain.SumBytes([]byte("intent-3"))
	if _, err := fixture.store.ClaimMutation(context.Background(), request); err == nil {
		t.Fatal("mutation limit was oversold")
	}
}

func TestMutationClaimConcurrentFinalSlotHasOneWinner(t *testing.T) {
	request := testCreateRunRequest(domain.RunID("run_00000000000000000000000000000092"), testNow, 30)
	request.BudgetLimits.MaxMutationsPerStage = 1
	request.SubmittedRequestJSON = canonicalSQLiteRunRequestJSON(request.BudgetLimits)
	request.SubmittedRequestDigest = domain.SumBytes(request.SubmittedRequestJSON)
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "mutation.db"), clock.NewFake(testNow))
	defer store.Close()
	mustCreateRun(t, store, request)
	mustBeginStage(t, store, request.RunID, domain.AttemptID("attempt_00000000000000000000000000000092"), 1, "prepare", domain.SumBytes([]byte("input")), testNow, meteringID("begin", "mutation race"))
	base := domain.MutationClaimRequest{RunID: request.RunID, StageName: "prepare", ScopeDigest: domain.SumBytes([]byte("scope")), SourceBatchDigest: domain.SumBytes([]byte("sources")), Ordinal: 1, Limit: 1, Kind: domain.MutationContent, IntentDigest: domain.SumBytes([]byte("seed")), At: testNow}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for index := 0; index < 2; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			claim := base
			claim.IntentDigest = domain.SumBytes([]byte{byte(index)})
			_, err := store.ClaimMutation(context.Background(), claim)
			results <- err
		}(index)
	}
	wg.Wait()
	close(results)
	var success int
	for err := range results {
		if err == nil {
			success++
		}
	}
	if success != 1 {
		t.Fatalf("concurrent mutation winners = %d, want 1", success)
	}
}

func TestMutationClaimUsesOneQuotaAcrossKinds(t *testing.T) {
	request := testCreateRunRequest(domain.RunID("run_00000000000000000000000000000093"), testNow, 30)
	request.BudgetLimits.MaxMutationsPerStage = 1
	request.SubmittedRequestJSON = canonicalSQLiteRunRequestJSON(request.BudgetLimits)
	request.SubmittedRequestDigest = domain.SumBytes(request.SubmittedRequestJSON)
	store := openRuntimeStore(t, filepath.Join(t.TempDir(), "mutation-total.db"), clock.NewFake(testNow))
	mustCreateRun(t, store, request)
	mustBeginStage(t, store, request.RunID, domain.AttemptID("attempt_00000000000000000000000000000093"), 1, "prepare", domain.SumBytes([]byte("input")), testNow, "begin_00000000000000000000000000000093")
	base := domain.MutationClaimRequest{RunID: request.RunID, StageName: "prepare", ScopeDigest: domain.SumBytes([]byte("scope")), SourceBatchDigest: domain.SumBytes([]byte("sources")), Ordinal: 1, Limit: 1, LimitSnapshot: 1, Kind: domain.MutationContent, IntentDigest: domain.SumBytes([]byte("content")), At: testNow}
	if _, err := store.ClaimMutation(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	base.Kind = domain.MutationMetadata
	base.Ordinal = 1
	base.IntentDigest = domain.SumBytes([]byte("metadata"))
	if _, err := store.ClaimMutation(context.Background(), base); err == nil {
		t.Fatal("mutation quota was allocated independently per kind")
	}
}

func TestMutationLedgerRowsAreImmutable(t *testing.T) {
	fixture := newMeteringFixture(t, "94", testCreateRunRequest(domain.RunID("run_00000000000000000000000000000094"), testNow, 30*time.Second).BudgetLimits)
	claim := domain.MutationClaimRequest{RunID: fixture.runID, StageName: fixture.stage, ScopeDigest: domain.SumBytes([]byte("scope")), SourceBatchDigest: domain.SumBytes([]byte("sources")), Ordinal: 1, Limit: 2, LimitSnapshot: 2, Kind: domain.MutationContent, IntentDigest: domain.SumBytes([]byte("immutable")), At: fixture.now}
	grant, err := fixture.store.ClaimMutation(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	for name, statement := range map[string]string{
		"claim update":  `UPDATE mutation_claims SET ordinal = ordinal + 1 WHERE claim_id = '` + grant.ClaimID + `'`,
		"claim delete":  `DELETE FROM mutation_claims WHERE claim_id = '` + grant.ClaimID + `'`,
		"intent update": `UPDATE mutation_intents SET command_digest = command_digest WHERE intent_digest = '` + string(claim.IntentDigest) + `'`,
		"intent delete": `DELETE FROM mutation_intents WHERE intent_digest = '` + string(claim.IntentDigest) + `'`,
	} {
		if _, err := fixture.store.db.ExecContext(context.Background(), statement); err == nil {
			t.Fatalf("mutation ledger %s succeeded", name)
		}
	}
}
