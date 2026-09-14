package application_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
)

func TestStructuredLLMCacheReusesCommittedRepairWithCurrentRunProvenance(t *testing.T) {
	f := newStructuredLLMOptionsFixture(t, structuredLLMOptions{stages: []domain.StageName{"prepare", "exercise", "finish"}})
	cache := structuredCacheService(t, f)
	result, err := f.structured.Generate(context.Background(), f.open, f.request)
	if err != nil || result.Outcome.Value == nil {
		t.Fatalf("generate=%+v err=%v", result, err)
	}
	if _, err := cache.Put(context.Background(), f.open, f.request); err == nil {
		t.Fatal("uncommitted response was cached")
	}
	open, request := commitStructuredSource(t, f, result)
	key, err := cache.Put(context.Background(), f.open, f.request)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.Validate(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		hit, err := cache.Reuse(context.Background(), open, request)
		if err != nil || !hit.Hit || hit.Outcome.Value == nil || hit.Outcome.CallTrace.DispatchKind != domain.DispatchCacheHit || len(hit.Reuses) != 1 || f.httpCalls.Load() != 2 {
			t.Fatalf("hit=%+v err=%v calls=%d", hit, err, f.httpCalls.Load())
		}
		if hit.Outcome.Value.RawBlob != nil || hit.Outcome.Value.Usage != (port.Usage{}) || len(hit.Outcome.CallTrace.PhysicalAttemptCallIDs) != 0 || string(hit.Outcome.Value.Structured) != string(result.Outcome.Value.Structured) {
			t.Fatalf("cache hit claims new provider work: %+v", hit)
		}
		if hit.Reuses[0].SourceCallRecordID != *hit.Outcome.CallTrace.CacheSourceCallRecordID || hit.Reuses[0].CurrentCallRecordID != open.ID || !strings.HasPrefix(string(hit.Reuses[0].LogicalPath), "private/llm/") {
			t.Fatal("cache provenance escaped its private source")
		}
		if i == 1 {
			output := domain.SumBytes(hit.Outcome.Value.Structured)
			finish := domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 4, StageName: "exercise", AttemptID: open.AttemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "finish", NextInputDigest: &output, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceCacheReuse, CacheReuse: &hit.Reuses[0]}}, IdempotencyKey: "finish_00000000000000000000000000000903", At: f.clock.Now()}
			if _, err := f.store.FinishStage(context.Background(), finish); err != nil {
				t.Fatal(err)
			}
		}
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var uses, occurrences, physical int
	if err := db.QueryRow(`SELECT count(*) FROM cache_reuse_records WHERE current_call_record_id=?`, open.ID).Scan(&uses); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM artifact_occurrences WHERE current_call_record_id=?`, open.ID).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM physical_calls WHERE call_record_id=?`, open.ID).Scan(&physical); err != nil {
		t.Fatal(err)
	}
	if uses != 1 || occurrences != 1 || physical != 0 || f.httpCalls.Load() != 2 {
		t.Fatalf("uses=%d occurrences=%d physical=%d HTTP=%d", uses, occurrences, physical, f.httpCalls.Load())
	}
}

func TestStructuredLLMCacheRejectsMissingPrivateBlobWithoutDispatch(t *testing.T) {
	f := newStructuredLLMFixture(t, false)
	cache := structuredCacheService(t, f)
	result, err := f.structured.Generate(context.Background(), f.open, f.request)
	if err != nil {
		t.Fatal(err)
	}
	open, request := commitStructuredSource(t, f, result)
	if _, err := cache.Put(context.Background(), f.open, f.request); err != nil {
		t.Fatal(err)
	}
	ref := result.Outcome.Value.RawBlob.Blob
	hex := strings.TrimPrefix(string(ref.Digest), "sha256:")
	if err := os.Remove(filepath.Join(f.blobRoot, "blobs", "sha256", hex[:2], hex)); err != nil {
		t.Fatal(err)
	}
	hit, err := cache.Reuse(context.Background(), open, request)
	if err == nil || hit.Hit || f.httpCalls.Load() != 2 {
		t.Fatalf("missing response admitted: hit=%+v err=%v", hit, err)
	}
}

func TestStructuredLLMCacheStoresOriginalSuccessWithRepairDisabled(t *testing.T) {
	f := newStructuredLLMOptionsFixture(t, structuredLLMOptions{firstContent: `{"schema_version":"cpgen.idea/v1","title":"original"}`, maxCalls: 1})
	f.policy.MaxRepairs = 0
	var err error
	f.structured, err = durable.NewStructuredLLMCalls(f.calls, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	cache := structuredCacheService(t, f)
	result, err := f.structured.Generate(context.Background(), f.open, f.request)
	if err != nil || result.Outcome.Value == nil || len(result.Artifacts) != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	open, request := commitStructuredSource(t, f, result)
	if _, err := cache.Put(context.Background(), f.open, f.request); err != nil {
		t.Fatal(err)
	}
	hit, err := cache.Reuse(context.Background(), open, request)
	if err != nil || !hit.Hit || f.httpCalls.Load() != 1 || hit.Outcome.Value.Usage != (port.Usage{}) {
		t.Fatalf("hit=%+v err=%v calls=%d", hit, err, f.httpCalls.Load())
	}
}

func TestStructuredLLMCacheIdentityIncludesRunPolicyAndOutputLimits(t *testing.T) {
	f := newStructuredLLMFixture(t, false)
	cache := structuredCacheService(t, f)
	result, err := f.structured.Generate(context.Background(), f.open, f.request)
	if err != nil {
		t.Fatal(err)
	}
	open, request := commitStructuredSource(t, f, result)
	if _, err := cache.Put(context.Background(), f.open, f.request); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		mutate   func(*domain.OpenCallRequest, *port.GenerateRequest)
		rejected bool
	}{
		{"run", func(o *domain.OpenCallRequest, _ *port.GenerateRequest) {
			o.RunID = "run_000000000000000000000000000000ff"
		}, false},
		{"variables", func(_ *domain.OpenCallRequest, r *port.GenerateRequest) {
			r.Variables = []byte(`{"brief":"different"}`)
		}, false},
		{"sampling", func(_ *domain.OpenCallRequest, r *port.GenerateRequest) { r.Sampling.Temperature = 0.75 }, false},
		{"output-bytes", func(_ *domain.OpenCallRequest, r *port.GenerateRequest) { r.MaxOutput.Bytes++ }, false},
		{"output-tokens", func(_ *domain.OpenCallRequest, r *port.GenerateRequest) { r.MaxOutput.Tokens++ }, false},
		{"provider-policy", func(_ *domain.OpenCallRequest, r *port.GenerateRequest) {
			r.ProviderPolicyDigest = domain.SumBytes([]byte("other-policy"))
		}, false},
		{"shared-privacy", func(_ *domain.OpenCallRequest, r *port.GenerateRequest) { r.PrivacyClassification = "public" }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changedOpen, changedRequest := open, request
			tc.mutate(&changedOpen, &changedRequest)
			plan, err := f.model.PlanGenerate(changedRequest)
			if err != nil {
				t.Fatal(err)
			}
			changedOpen.Provider, changedOpen.RequestDigest, changedOpen.PolicyDigest = plan.Provider, plan.RequestDigest, changedRequest.ProviderPolicyDigest
			hit, err := cache.Reuse(context.Background(), changedOpen, changedRequest)
			if (err != nil) != tc.rejected || hit.Hit || f.httpCalls.Load() != 2 {
				t.Fatalf("identity crossed: hit=%+v err=%v", hit, err)
			}
		})
	}
}

func TestStructuredLLMCacheValidatesBindingBeforeRecordingAHit(t *testing.T) {
	f := newStructuredLLMFixture(t, false)
	cache := structuredCacheService(t, f)
	result, err := f.structured.Generate(context.Background(), f.open, f.request)
	if err != nil {
		t.Fatal(err)
	}
	open, request := commitStructuredSource(t, f, result)
	if _, err := cache.Put(context.Background(), f.open, f.request); err != nil {
		t.Fatal(err)
	}
	bad := structuredCacheServiceWithLedger(t, f, &changedLLMCacheLedger{Store: f.store})
	if hit, err := bad.Reuse(context.Background(), open, request); err == nil || hit.Hit {
		t.Fatalf("unverified cache metadata admitted: %+v %v", hit, err)
	}
	if _, err := f.store.LoadCall(context.Background(), open.ID); err == nil {
		t.Fatal("validation failure already created a cache call")
	}
	if hit, err := cache.Reuse(context.Background(), open, request); err != nil || !hit.Hit || f.httpCalls.Load() != 2 {
		t.Fatalf("verified retry=%+v err=%v", hit, err)
	}
}

type changedLLMCacheLedger struct{ *sqlite.Store }

func (l *changedLLMCacheLedger) Lookup(ctx context.Context, lookup domain.CacheLookup) (domain.CacheCandidate, bool, error) {
	candidate, hit, err := l.Store.Lookup(ctx, lookup)
	if err == nil && hit {
		digest := domain.SumBytes([]byte("substituted binding"))
		candidate.Entry.Blobs[0].Provenance.InputDigest = &digest
	}
	return candidate, hit, err
}

func TestStructuredLLMCacheResumesFailedReuseCommitWithoutAnotherCall(t *testing.T) {
	f := newStructuredLLMFixture(t, false)
	cache := structuredCacheService(t, f)
	result, err := f.structured.Generate(context.Background(), f.open, f.request)
	if err != nil {
		t.Fatal(err)
	}
	open, request := commitStructuredSource(t, f, result)
	if _, err := cache.Put(context.Background(), f.open, f.request); err != nil {
		t.Fatal(err)
	}
	broken := structuredCacheServiceWithLedger(t, f, &interruptedLLMCacheLedger{Store: f.store})
	if _, err := broken.Reuse(context.Background(), open, request); err == nil {
		t.Fatal("expected cache reuse commit fault")
	}
	prepared, err := f.store.LoadCall(context.Background(), open.ID)
	if err != nil || prepared.CallTrace == nil || prepared.CallTrace.DispatchKind != domain.DispatchCacheHit {
		t.Fatalf("cache call was not persisted before fault: %+v %v", prepared, err)
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.OpenWithClock(context.Background(), sqlite.Config{Path: f.path, BusyTimeout: time.Second, MaxReaders: 4}, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	f.store = reopened
	f.calls, err = durable.NewReplayableLLMCalls(reopened, f.model, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	f.structured, err = durable.NewStructuredLLMCalls(f.calls, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	cache = structuredCacheService(t, f)
	for i := 0; i < 2; i++ {
		hit, err := cache.Reuse(context.Background(), open, request)
		if err != nil || !hit.Hit || len(hit.Reuses) != 1 || f.httpCalls.Load() != 2 {
			t.Fatalf("recovered hit=%+v err=%v", hit, err)
		}
	}
}

type interruptedLLMCacheLedger struct{ *sqlite.Store }

func (l *interruptedLLMCacheLedger) CommitReuseCollection(context.Context, domain.CommitCacheReuse) ([]domain.PendingCacheReuse, error) {
	return nil, errors.New("injected reuse commit failure")
}

func structuredCacheService(t *testing.T, f structuredLLMFixture) *durable.StructuredLLMCache {
	return structuredCacheServiceWithLedger(t, f, f.store)
}

func structuredCacheServiceWithLedger(t *testing.T, f structuredLLMFixture, ledger durable.LLMCacheLedger) *durable.StructuredLLMCache {
	t.Helper()
	locks, err := runlock.NewManager(t.TempDir(), runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = locks.Close() })
	cache, err := durable.NewStructuredLLMCache(f.structured, ledger, locks)
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func commitStructuredSource(t *testing.T, f structuredLLMFixture, result durable.StructuredLLMResult) (domain.OpenCallRequest, port.GenerateRequest) {
	t.Helper()
	output := domain.SumBytes(result.Outcome.Value.Structured)
	finish := domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, IdempotencyKey: "finish_00000000000000000000000000000902", At: f.clock.Now()}
	for i := range result.Artifacts {
		finish.Occurrences = append(finish.Occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &result.Artifacts[i]})
	}
	if _, err := f.store.FinishStage(context.Background(), finish); err != nil {
		t.Fatal(err)
	}
	attempt := domain.AttemptID("attempt_00000000000000000000000000000902")
	if _, err := f.store.BeginStage(context.Background(), domain.BeginStageCommand{RunID: f.runID, ExpectedRunVersion: 3, StageName: "exercise", AttemptID: attempt, InputDigest: output, IdempotencyKey: "begin_00000000000000000000000000000902", At: f.clock.Now()}); err != nil {
		t.Fatal(err)
	}
	open := f.openRequest(903)
	open.ExpectedRunVersion, open.StageName, open.AttemptID = 4, "exercise", attempt
	open.RetryPolicy = f.open.RetryPolicy
	request := f.request
	request.LogicalIdempotencyKey = open.LogicalOperationID
	plan, err := f.model.PlanGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	open.Provider, open.RequestDigest, open.PolicyDigest = plan.Provider, plan.RequestDigest, request.ProviderPolicyDigest
	return open, request
}
