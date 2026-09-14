package application_test

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/similarity"
)

func TestSimilarityReplaySurvivesRestartAndAtomicAttachment(t *testing.T) {
	f := newSimilarityReplayFixture(t)
	first := f.search(t, f.service)
	pending := first.Artifact
	if pending == nil || pending.Role != domain.ArtifactEvidence || !strings.HasPrefix(string(pending.LogicalPath), "private/similarity/") {
		t.Fatalf("missing private evidence: %+v", pending)
	}
	reader, err := f.blobs.OpenVerified(context.Background(), pending.Blob)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "fixture-key") || strings.Contains(string(raw), "private-request-title") {
		t.Fatal("private receipt persisted credentials or original query")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.OpenWithClock(context.Background(), sqlite.Config{Path: f.path, BusyTimeout: time.Second, MaxReaders: 4}, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	service, err := durable.NewReplayableSimilarityCalls(reopened, f.provider, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	replayed := f.search(t, service)
	if !reflect.DeepEqual(first, replayed) || f.httpCalls.Load() != 1 {
		t.Fatalf("restart changed evidence or resent: HTTP=%d", f.httpCalls.Load())
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// Fail inside the same transaction, after occurrence insertion and byte
	// settlement, to prove all three effects roll back together.
	if _, err := db.Exec(`CREATE TRIGGER reject_similarity_completion BEFORE UPDATE ON call_records
		WHEN NEW.provider='private-blob' AND NEW.state='TERMINAL'
		BEGIN SELECT RAISE(ABORT,'injected receipt completion failure'); END;`); err != nil {
		t.Fatal(err)
	}
	finish := f.finish(first)
	if _, err := reopened.FinishStage(context.Background(), finish); err == nil {
		t.Fatal("expected commit failure")
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM artifact_occurrences WHERE writer_token_id=?`, pending.WriterTokenID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed completion retained an occurrence")
	}
	f.search(t, service)
	if _, err := db.Exec(`DROP TRIGGER reject_similarity_completion`); err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.FinishStage(context.Background(), finish); err != nil {
		t.Fatal(err)
	}
	var consumed, reserved int64
	if err := db.QueryRow(`SELECT consumed_value,reserved_value FROM budget_accounts WHERE run_id=? AND dimension='ARTIFACT_PHYSICAL_NEW_BYTES'`, f.runID).Scan(&consumed, &reserved); err != nil {
		t.Fatal(err)
	}
	if consumed != pending.Blob.Size || reserved != 0 || f.httpCalls.Load() != 1 {
		t.Fatalf("consumed=%d reserved=%d HTTP=%d", consumed, reserved, f.httpCalls.Load())
	}
	var id domain.CallRecordID
	if err := db.QueryRow(`SELECT current_call_record_id FROM artifact_occurrences WHERE writer_token_id=?`, pending.WriterTokenID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	local, err := reopened.LoadCall(context.Background(), id)
	if err != nil || local.Call.State != domain.CallRecordTerminal || local.Call.Kind != domain.CallSimilaritySearch || local.Call.Failure != nil {
		t.Fatalf("local completion=%+v err=%v", local.Call, err)
	}
	for _, physical := range local.PhysicalCalls {
		if physical.ID == pending.CallID && (physical.State != domain.PhysicalCompleted || physical.ResponseDigest == nil || *physical.ResponseDigest != pending.Blob.Digest) {
			t.Fatalf("local receipt completion=%+v", physical)
		}
	}
}

func TestSimilarityReplayRecoversPublishedAndSealedReceipts(t *testing.T) {
	for _, boundary := range []string{"sent", "complete", "finish", "sealed"} {
		t.Run(boundary, func(t *testing.T) {
			f := newSimilarityReplayFixture(t)
			var ledger durable.ArtifactCallLedger = &interruptedLLMReplayLedger{Store: f.store, boundary: boundary}
			if boundary == "sealed" {
				ledger = &llmPublicationFailureLedger{Store: f.store}
			}
			service, err := durable.NewReplayableSimilarityCalls(ledger, f.provider, f.blobs, f.clock, 100)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.SearchWithArtifacts(context.Background(), f.open, f.request); err == nil {
				t.Fatal("expected injected failure")
			}
			pending, err := f.store.LoadCall(context.Background(), f.open.ID)
			if err != nil || pending.Call.State == domain.CallRecordTerminal {
				t.Fatalf("recoverable parent closed: %+v %v", pending.Call, err)
			}
			result := f.search(t, f.service)
			if result.Artifact == nil || f.httpCalls.Load() != 1 {
				t.Fatalf("receipt recovery HTTP=%d", f.httpCalls.Load())
			}
			replayed := f.search(t, f.service)
			if !reflect.DeepEqual(result, replayed) || f.httpCalls.Load() != 1 {
				t.Fatal("recovery changed original evidence")
			}
		})
	}
}

func TestSimilarityReplayMissingOrCorruptEvidenceNeverResends(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			f := newSimilarityReplayFixture(t)
			first := f.search(t, f.service)
			hex := strings.TrimPrefix(string(first.Artifact.Blob.Digest), "sha256:")
			path := filepath.Join(f.blobRoot, "blobs", "sha256", hex[:2], hex)
			var err error
			if mode == "missing" {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte("corrupt"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				result, err := f.service.SearchWithArtifacts(context.Background(), f.open, f.request)
				if err == nil || result.Outcome.Value != nil || f.httpCalls.Load() != 1 {
					t.Fatalf("unverified evidence replay err=%v HTTP=%d", err, f.httpCalls.Load())
				}
			}
		})
	}
}

func TestSimilarityReplayRetryRetainsOnlyProducingReceipt(t *testing.T) {
	f := newSimilarityReplayFixture(t, http.StatusTooManyRequests)
	result := f.search(t, f.service)
	if len(result.Outcome.CallTrace.PhysicalAttemptCallIDs) != 2 || f.httpCalls.Load() != 2 {
		t.Fatal("ledger did not own both transport attempts")
	}
	replayed := f.search(t, f.service)
	if !reflect.DeepEqual(result, replayed) || f.httpCalls.Load() != 2 {
		t.Fatal("retry replay changed evidence")
	}
	if _, err := f.store.FinishStage(context.Background(), f.finish(result)); err != nil {
		t.Fatal(err)
	}
	assertSimilarityPrivateSlotsClosed(t, f, result.Artifact.Blob.Size)
}

func TestSimilarityReplayBudgetFailureReleasesUnusedSlots(t *testing.T) {
	for _, mode := range []string{"artifact", "provider"} {
		t.Run(mode, func(t *testing.T) {
			f := newSimilarityReplayFixture(t)
			request, open := f.request, f.open
			cost := int64(100)
			if mode == "provider" {
				cost = 100000
			} else {
				open.RetryPolicy.MaxAttempts = 8
			}
			service, err := durable.NewReplayableSimilarityCalls(f.store, f.provider, f.blobs, f.clock, cost)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				result, err := service.SearchWithArtifacts(context.Background(), open, request)
				if err != nil || result.Outcome.Failure == nil || result.Outcome.Failure.Code != domain.FailureBudgetExhausted || result.Artifact != nil || f.httpCalls.Load() != 0 {
					t.Fatalf("budget result=%+v err=%v HTTP=%d", result, err, f.httpCalls.Load())
				}
			}
			assertSimilarityPrivateSlotsClosed(t, f, 0)
		})
	}
}

func TestSimilarityReplayRejectedStageChargesPublishedBytes(t *testing.T) {
	f := newSimilarityReplayFixture(t)
	result := f.search(t, f.service)
	finish := f.finish(result)
	finish.AttemptState, finish.RunState = domain.StageAttemptFailed, domain.RunFailed
	finish.NextStage, finish.NextInputDigest, finish.OutputDigest, finish.Occurrences = "", nil, nil, nil
	if _, err := f.store.FinishStage(context.Background(), finish); err != nil {
		t.Fatal(err)
	}
	assertSimilarityPrivateSlotsClosed(t, f, result.Artifact.Blob.Size)
}

func TestSimilarityReplayPersistsResponseAfterCancellation(t *testing.T) {
	f := newSimilarityReplayFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := similarityAcceptanceHook{PhysicalProvider: f.provider, after: func() {
		_, err := f.store.RequestCancel(context.Background(), domain.CancelRequest{ID: "control_00000000000000000000000000001801", RunID: f.runID, ExpectedRunVersion: 2, Reason: "receipt cancellation", IdempotencyKey: coordinatorID("cancel", "similarity-receipt"), At: f.clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
	}}
	service, err := durable.NewReplayableSimilarityCalls(f.store, provider, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.SearchWithArtifacts(ctx, f.open, f.request)
	if err != nil || result.Outcome.Value == nil || result.Artifact == nil {
		t.Fatalf("canceled receipt=%+v err=%v", result, err)
	}
	replayed := f.search(t, f.service)
	if !reflect.DeepEqual(result, replayed) || f.httpCalls.Load() != 1 {
		t.Fatal("cancellation lost or replaced its completed response")
	}
}

func TestSimilarityReplayUnknownBoundaryReleasesOnlyUnwrittenBytes(t *testing.T) {
	f := newSimilarityReplayFixture(t)
	provider := similarityAcceptanceHook{PhysicalProvider: f.provider, unknown: true}
	service, err := durable.NewReplayableSimilarityCalls(f.store, provider, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		result, err := service.SearchWithArtifacts(context.Background(), f.open, f.request)
		if err != nil || result.Outcome.Failure == nil || result.Outcome.Failure.Class != domain.FailureUnknown || result.Artifact != nil || f.httpCalls.Load() != 1 {
			t.Fatalf("unknown result=%+v err=%v HTTP=%d", result, err, f.httpCalls.Load())
		}
	}
	assertSimilarityPrivateSlotsClosed(t, f, 0)
	prepared, err := f.store.LoadCall(context.Background(), f.open.ID)
	if err != nil {
		t.Fatal(err)
	}
	var cost, count int64
	for _, reservation := range prepared.Reservations {
		if reservation.State == domain.ReservationReserved || reservation.SettledValue == nil {
			t.Fatalf("live unknown account: %+v", reservation)
		}
		if reservation.Dimension == domain.BudgetSimilarityCalls {
			count += *reservation.SettledValue
		}
		if reservation.Dimension == domain.BudgetSimilarityCostMicroUSD {
			cost += *reservation.SettledValue
		}
	}
	if cost != 100 || count != 1 {
		t.Fatalf("unknown spend cost=%d calls=%d", cost, count)
	}
}

type similarityAcceptanceHook struct {
	similarity.PhysicalProvider
	after   func()
	unknown bool
}

func (p similarityAcceptanceHook) SearchPhysical(ctx context.Context, request similarity.Request, id domain.AttemptCallID) (similarity.PhysicalSearchResult, error) {
	result, err := p.PhysicalProvider.SearchPhysical(ctx, request, id)
	if p.after != nil {
		p.after()
	}
	if p.unknown {
		result = similarity.PhysicalSearchResult{Execution: domain.PhysicalExecution[similarity.Evidence]{Boundary: domain.BoundaryUnknown, Failure: &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}}}
	}
	return result, err
}

func assertSimilarityPrivateSlotsClosed(t *testing.T, f similarityReplayFixture, expectedBytes int64) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var consumed, reserved int64
	if err := db.QueryRow(`SELECT consumed_value,reserved_value FROM budget_accounts WHERE run_id=? AND dimension='ARTIFACT_PHYSICAL_NEW_BYTES'`, f.runID).Scan(&consumed, &reserved); err != nil {
		t.Fatal(err)
	}
	var active int
	if err := db.QueryRow(`SELECT count(*) FROM call_records WHERE run_id=? AND provider='private-blob' AND state<>'TERMINAL'`, f.runID).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if consumed != expectedBytes || reserved != 0 || active != 0 {
		t.Fatalf("private byte consumption=%d reserved=%d active=%d", consumed, reserved, active)
	}
}

type similarityReplayFixture struct {
	coordinatorFixture
	provider  *similarity.HTTPAdapter
	request   similarity.Request
	open      domain.OpenCallRequest
	blobs     *blob.Store
	blobRoot  string
	service   *durable.SimilarityCalls
	httpCalls *atomic.Int32
}

func newSimilarityReplayFixture(t *testing.T, statuses ...int) similarityReplayFixture {
	t.Helper()
	t.Setenv("CPGEN_DURABLE_SIMILARITY_KEY", "fixture-key")
	sends := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ordinal := sends.Add(1)
		if int(ordinal) <= len(statuses) {
			w.WriteHeader(statuses[ordinal-1])
		}
		_, _ = w.Write([]byte(`{"provider_identity":"fixture","hits":[],"usage":{"input_tokens":2,"output_tokens":3,"cost_micro_usd":11}}`))
	}))
	t.Cleanup(server.Close)
	provider, request := durableSimilarityProvider(t, server.URL)
	projection, err := similarity.NewPackageSafeProjection("private-request-title", "private-request-statement", []string{"graphs"}, "en")
	if err != nil {
		t.Fatal(err)
	}
	request, err = similarity.NewRequest(projection, request.PolicyRef, request.PolicyDigest, request.LogicalIdempotencyKey)
	if err != nil {
		t.Fatal(err)
	}
	request.LogicalIdempotencyKey = "private-similarity-replay"
	inputDigest, err := request.Digest()
	if err != nil {
		t.Fatal(err)
	}
	f := newCoordinatorFixtureWithStageInput(t, "b4", domain.BudgetLimits{MaxSimilarityCalls: 3, MaxSimilarityCostMicroUSD: 300, MaxArtifactBytes: 300000}, []domain.StageName{"prepare", "exercise"}, inputDigest)
	open := f.openRequest(1801)
	open.LogicalOperationID = request.LogicalIdempotencyKey
	plan, err := provider.PlanSearch(request)
	if err != nil {
		t.Fatal(err)
	}
	open.Kind, open.Provider, open.RequestDigest, open.PolicyDigest = domain.CallSimilaritySearch, plan.Provider, plan.RequestDigest, plan.PolicyDigest
	root := filepath.Join(t.TempDir(), "private")
	blobs, err := blob.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service, err := durable.NewReplayableSimilarityCalls(f.store, provider, blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	return similarityReplayFixture{f, provider, request, open, blobs, root, service, sends}
}

func (f similarityReplayFixture) search(t *testing.T, service *durable.SimilarityCalls) durable.SimilarityCallResult {
	t.Helper()
	result, err := service.SearchWithArtifacts(context.Background(), f.open, f.request)
	if err != nil || result.Outcome.Value == nil || result.Artifact == nil || result.Outcome.Value.Validate() != nil {
		t.Fatalf("similarity result=%+v err=%v", result, err)
	}
	return result
}

func (f similarityReplayFixture) finish(result durable.SimilarityCallResult) domain.FinishStageCommand {
	output := result.Outcome.Value.EvidenceDigest
	return domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: f.attemptID,
		AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output,
		Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: result.Artifact}}, IdempotencyKey: coordinatorID("finish", "similarity-receipt"), At: f.clock.Now()}
}
