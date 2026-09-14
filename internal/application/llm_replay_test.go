package application_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"cpgen/internal/port"
)

func TestLLMReplaySurvivesRestartAndAttachesPrivateOccurrence(t *testing.T) {
	f := newLLMReplayFixture(t)
	first := f.generate(t, f.service)
	if first.Value.RawBlob == nil {
		t.Fatal("successful response lacks a private artifact")
	}
	pending := *first.Value.RawBlob
	if !strings.HasPrefix(string(pending.LogicalPath), "private/llm/") || pending.Role != domain.ArtifactEvidence {
		t.Fatalf("private declaration: %+v", pending)
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
	if strings.Contains(string(raw), "fixture-key") || strings.Contains(string(raw), "private prompt") {
		t.Fatal("response receipt includes credentials or prompt variables")
	}
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.OpenWithClock(context.Background(), sqlite.Config{Path: f.path, BusyTimeout: time.Second, MaxReaders: 4}, f.clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	blobs, err := blob.NewStore(f.blobRoot)
	if err != nil {
		t.Fatal(err)
	}
	service, err := durable.NewReplayableLLMCalls(reopened, f.model, blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	replayed := f.generate(t, service)
	if f.httpCalls.Load() != 1 || !first.CallTrace.Equal(replayed.CallTrace) || first.Value.Usage != replayed.Value.Usage || string(first.Value.Structured) != string(replayed.Value.Structured) || !reflect.DeepEqual(*replayed.Value.RawBlob, pending) {
		t.Fatalf("replay changed response or sent again: calls=%d", f.httpCalls.Load())
	}
	// A failed stage commit must leave the finalized writer available for retry.
	output := domain.SumBytes(first.Value.Structured)
	finish := domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 99, StageName: "prepare", AttemptID: f.attemptID,
		AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output,
		Occurrences:    []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: replayed.Value.RawBlob}},
		IdempotencyKey: "finish_00000000000000000000000000000801", At: f.clock.Now()}
	if _, err := reopened.FinishStage(context.Background(), finish); !errors.Is(err, sqlite.ErrVersionConflict) {
		t.Fatalf("bad commit: %v", err)
	}
	f.generate(t, service)
	finish.ExpectedRunVersion = 2
	if _, err := reopened.FinishStage(context.Background(), finish); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM artifact_occurrences WHERE writer_token_id = ?`, pending.WriterTokenID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	var consumed, reserved int64
	if err := db.QueryRow(`SELECT consumed_value, reserved_value FROM budget_accounts WHERE run_id = ? AND dimension = 'ARTIFACT_PHYSICAL_NEW_BYTES'`, f.runID).Scan(&consumed, &reserved); err != nil {
		t.Fatal(err)
	}
	if count != 1 || consumed != pending.Blob.Size || reserved != 0 || f.httpCalls.Load() != 1 {
		t.Fatalf("count=%d consumed=%d reserved=%d calls=%d", count, consumed, reserved, f.httpCalls.Load())
	}
	var artifactCallID domain.CallRecordID
	if err := db.QueryRow(`SELECT current_call_record_id FROM artifact_occurrences WHERE writer_token_id=?`, pending.WriterTokenID).Scan(&artifactCallID); err != nil {
		t.Fatal(err)
	}
	artifactCall, err := reopened.LoadCall(context.Background(), artifactCallID)
	if err != nil || artifactCall.Call.State != domain.CallRecordTerminal || artifactCall.Call.DispatchKind == nil || *artifactCall.Call.DispatchKind != domain.DispatchDispatched || artifactCall.Call.Failure != nil {
		t.Fatalf("attached artifact call is not a successful terminal cache source: %+v err=%v", artifactCall.Call, err)
	}
	for _, physical := range artifactCall.PhysicalCalls {
		if physical.ID == pending.CallID && (physical.State != domain.PhysicalCompleted || physical.Outcome == nil || *physical.Outcome != domain.PhysicalOutcomeSuccess || physical.ResponseDigest == nil || *physical.ResponseDigest != pending.Blob.Digest) {
			t.Fatalf("artifact physical completion lacks published receipt: %+v", physical)
		}
	}
}

func TestLLMReplayRejectsMissingOrCorruptBlobWithoutResend(t *testing.T) {
	for _, mode := range []string{"missing", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			f := newLLMReplayFixture(t)
			first := f.generate(t, f.service)
			ref := first.Value.RawBlob.Blob
			hex := strings.TrimPrefix(string(ref.Digest), "sha256:")
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
				outcome, err := f.service.Generate(context.Background(), f.open, f.request)
				if err == nil || outcome.Value != nil || f.httpCalls.Load() != 1 {
					t.Fatalf("unverified replay: outcome=%+v err=%v calls=%d", outcome, err, f.httpCalls.Load())
				}
			}
		})
	}
}

func TestLLMReplayArtifactCompletionRollsBackWithStage(t *testing.T) {
	f := newLLMReplayFixture(t)
	result := f.generate(t, f.service)
	pending := result.Value.RawBlob
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_llm_artifact_completion BEFORE UPDATE ON call_records
		WHEN NEW.provider='private-blob' AND NEW.state='TERMINAL'
		BEGIN SELECT RAISE(ABORT,'injected local completion failure'); END;`); err != nil {
		t.Fatal(err)
	}
	output := domain.SumBytes(result.Value.Structured)
	finish := domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &output, NextStage: "exercise", NextInputDigest: &output, Occurrences: []domain.PendingOccurrence{{Kind: domain.PendingOccurrenceNewWrite, NewWrite: pending}}, IdempotencyKey: "finish_00000000000000000000000000000802", At: f.clock.Now()}
	if _, err := f.store.FinishStage(context.Background(), finish); err == nil {
		t.Fatal("completion fault did not abort stage commit")
	}
	var occurrences int
	var physicalState string
	var consumed, reserved int64
	if err := db.QueryRow(`SELECT count(*) FROM artifact_occurrences WHERE run_id=?`, f.runID).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT state FROM physical_calls WHERE attempt_call_id=?`, pending.CallID).Scan(&physicalState); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT consumed_value,reserved_value FROM budget_accounts WHERE run_id=? AND dimension='ARTIFACT_PHYSICAL_NEW_BYTES'`, f.runID).Scan(&consumed, &reserved); err != nil {
		t.Fatal(err)
	}
	if occurrences != 0 || physicalState != "SENT" || consumed != 0 || reserved <= 0 || f.httpCalls.Load() != 1 {
		t.Fatalf("partial commit: occurrences=%d state=%s consumed=%d reserved=%d calls=%d", occurrences, physicalState, consumed, reserved, f.httpCalls.Load())
	}
	if _, err := db.Exec(`DROP TRIGGER reject_llm_artifact_completion`); err != nil {
		t.Fatal(err)
	}
	f.generate(t, f.service)
	for i := 0; i < 2; i++ {
		if _, err := f.store.FinishStage(context.Background(), finish); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.QueryRow(`SELECT count(*) FROM artifact_occurrences WHERE run_id=?`, f.runID).Scan(&occurrences); err != nil {
		t.Fatal(err)
	}
	if occurrences != 1 || f.httpCalls.Load() != 1 {
		t.Fatalf("retry duplicated effects: occurrences=%d calls=%d", occurrences, f.httpCalls.Load())
	}
}

func TestLLMReplayRejectedStageClosesPublishedPrivateArtifact(t *testing.T) {
	f := newLLMReplayFixture(t)
	result := f.generate(t, f.service)
	pending := result.Value.RawBlob
	finish := domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: 2, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptFailed, RunState: domain.RunFailed, IdempotencyKey: "finish_00000000000000000000000000000803", At: f.clock.Now()}
	if _, err := f.store.FinishStage(context.Background(), finish); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var reserved, consumed int64
	if err := db.QueryRow(`SELECT reserved_value,consumed_value FROM budget_accounts WHERE run_id=? AND dimension='ARTIFACT_PHYSICAL_NEW_BYTES'`, f.runID).Scan(&reserved, &consumed); err != nil {
		t.Fatal(err)
	}
	var state, callState string
	if err := db.QueryRow(`SELECT physical.state,call.state FROM physical_calls physical JOIN call_records call ON call.call_record_id=physical.call_record_id WHERE physical.attempt_call_id=?`, pending.CallID).Scan(&state, &callState); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 || consumed != pending.PhysicalNewBytes || state != "COMPLETED" || callState != "TERMINAL" || f.httpCalls.Load() != 1 {
		t.Fatalf("discarded publication remained pending: reserved=%d consumed=%d physical=%s call=%s", reserved, consumed, state, callState)
	}
}

func TestLLMReplayRecoversReceiptAfterLedgerWriteFailure(t *testing.T) {
	for _, boundary := range []string{"sent", "complete", "finish"} {
		t.Run(boundary, func(t *testing.T) {
			f := newLLMReplayFixture(t)
			ledger := &interruptedLLMReplayLedger{Store: f.store, boundary: boundary}
			service, err := durable.NewReplayableLLMCalls(ledger, f.model, f.blobs, f.clock, 100)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Generate(context.Background(), f.open, f.request); err == nil {
				t.Fatal("expected injected ledger failure")
			}
			outcome := f.generate(t, f.service)
			if f.httpCalls.Load() != 1 || outcome.Value.RawBlob == nil {
				t.Fatalf("recovery calls=%d", f.httpCalls.Load())
			}
		})
	}
}

func TestLLMReplayBudgetRejectionDoesNotSendOrHoldUnusedBytes(t *testing.T) {
	for _, budget := range []string{"artifact", "provider"} {
		t.Run(budget, func(t *testing.T) {
			f := newLLMReplayFixture(t)
			service := f.service
			if budget == "artifact" {
				f.request.MaxOutput.Bytes = 100000
				plan, err := f.model.PlanGenerate(f.request)
				if err != nil {
					t.Fatal(err)
				}
				f.open.RequestDigest = plan.RequestDigest
			} else {
				var err error
				service, err = durable.NewReplayableLLMCalls(f.store, f.model, f.blobs, f.clock, 100000)
				if err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < 2; i++ {
				outcome, err := service.Generate(context.Background(), f.open, f.request)
				if err != nil || outcome.Failure == nil || outcome.Failure.Code != domain.FailureBudgetExhausted || f.httpCalls.Load() != 0 {
					t.Fatalf("rejected outcome=%+v err=%v calls=%d", outcome, err, f.httpCalls.Load())
				}
			}
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var held int64
			if err := db.QueryRow(`SELECT sum(reserved_value) FROM budget_accounts WHERE run_id = ?`, f.runID).Scan(&held); err != nil {
				t.Fatal(err)
			}
			if held != 0 {
				t.Fatalf("rejected request retains %d budget", held)
			}
		})
	}
}

func TestLLMReplayBindsPrivateClassificationAndOutputLimits(t *testing.T) {
	f := newLLMReplayFixture(t)
	f.generate(t, f.service)
	for _, change := range []func(*port.GenerateRequest){func(r *port.GenerateRequest) { r.PrivacyClassification = "public" }, func(r *port.GenerateRequest) { r.MaxOutput.Bytes++ }} {
		request := f.request
		change(&request)
		outcome, err := f.service.Generate(context.Background(), f.open, request)
		if err == nil || outcome.Value != nil || f.httpCalls.Load() != 1 {
			t.Fatalf("changed binding admitted: err=%v calls=%d", err, f.httpCalls.Load())
		}
	}
}

func TestLLMReplayPersistsReceiptAfterPendingCancellation(t *testing.T) {
	f := newLLMReplayFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	provider := llmAcceptanceProviderHook{PhysicalLLM: f.model, after: func(context.Context, port.PhysicalLLMResult, error) {
		_, err := f.store.RequestCancel(context.Background(), domain.CancelRequest{ID: "control_00000000000000000000000000000801", RunID: f.runID, ExpectedRunVersion: 2, Reason: "fixture cancel", IdempotencyKey: "cancel_00000000000000000000000000000801", At: f.clock.Now()})
		if err != nil {
			t.Fatal(err)
		}
		cancel()
	}}
	service, err := durable.NewReplayableLLMCalls(f.store, provider, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Generate(ctx, f.open, f.request)
	if err != nil || result.Value == nil || result.Value.RawBlob == nil {
		t.Fatalf("cancel receipt=%+v err=%v", result, err)
	}
	replay := f.generate(t, f.service)
	if f.httpCalls.Load() != 1 || !replay.CallTrace.Equal(result.CallTrace) {
		t.Fatal("cancel replay resends or changes trace")
	}
}

type interruptedLLMReplayLedger struct {
	*sqlite.Store
	boundary string
}

func (l *interruptedLLMReplayLedger) MarkSent(ctx context.Context, grant domain.DispatchGrant, at time.Time) error {
	if l.boundary == "sent" {
		return errors.New("injected MarkSent failure")
	}
	return l.Store.MarkSent(ctx, grant, at)
}
func (l *interruptedLLMReplayLedger) CompletePhysical(ctx context.Context, request domain.CompletePhysicalRequest) error {
	if l.boundary == "complete" {
		return errors.New("injected CompletePhysical failure")
	}
	return l.Store.CompletePhysical(ctx, request)
}
func (l *interruptedLLMReplayLedger) FinishCall(ctx context.Context, request domain.FinishCallRequest) (domain.CallTrace, error) {
	if l.boundary == "finish" {
		return domain.CallTrace{}, errors.New("injected FinishCall failure")
	}
	return l.Store.FinishCall(ctx, request)
}

type llmReplayFixture struct {
	coordinatorFixture
	model     port.PhysicalLLM
	request   port.GenerateRequest
	open      domain.OpenCallRequest
	blobs     *blob.Store
	blobRoot  string
	service   *durable.LLMCalls
	httpCalls *atomic.Int32
	endpoint  string
}

func newLLMReplayFixture(t *testing.T, statuses ...int) llmReplayFixture {
	return newLLMReplayContentFixture(t, `{"schema_version":"cpgen.idea/v1","title":"replay"}`, statuses...)
}

func newLLMReplayContentFixture(t *testing.T, content string, statuses ...int) llmReplayFixture {
	t.Helper()
	t.Setenv("CPGEN_DURABLE_TEST_KEY", "fixture-key")
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ordinal := calls.Add(1)
		if int(ordinal) <= len(statuses) {
			w.WriteHeader(statuses[ordinal-1])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "replay-provider", "choices": []any{map[string]any{"message": map[string]string{"content": content}}}, "usage": map[string]int{"prompt_tokens": 3, "completion_tokens": 4}})
	}))
	t.Cleanup(server.Close)
	model, request := durableTestProvider(t, server.URL)
	fixture := newCoordinatorFixtureWithStages(t, "b1", domain.BudgetLimits{MaxLLMCalls: 3, MaxLLMInputTokens: 10000, MaxLLMOutputTokens: 1000, MaxLLMCostMicroUSD: 1000, MaxArtifactBytes: 100000}, []domain.StageName{"prepare", "exercise"})
	open := fixture.openRequest(801)
	request.LogicalIdempotencyKey = open.LogicalOperationID
	plan, err := model.PlanGenerate(request)
	if err != nil {
		t.Fatal(err)
	}
	open.Provider, open.RequestDigest, open.PolicyDigest = plan.Provider, plan.RequestDigest, request.ProviderPolicyDigest
	root := filepath.Join(t.TempDir(), "private")
	blobs, err := blob.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	service, err := durable.NewReplayableLLMCalls(fixture.store, model, blobs, fixture.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	return llmReplayFixture{fixture, model, request, open, blobs, root, service, calls, server.URL}
}

func TestLLMReplayTransportRetryReleasesFailedResponseWriter(t *testing.T) {
	f := newLLMReplayFixture(t, http.StatusTooManyRequests)
	result := f.generate(t, f.service)
	if f.httpCalls.Load() != 2 || len(result.CallTrace.PhysicalAttemptCallIDs) != 2 {
		t.Fatalf("HTTP=%d trace=%+v", f.httpCalls.Load(), result.CallTrace)
	}
	second := f.generate(t, f.service)
	if f.httpCalls.Load() != 2 || !result.CallTrace.Equal(second.CallTrace) || result.Value.Usage != second.Value.Usage {
		t.Fatal("retry replay changed accounting")
	}
}

func TestLLMReplayRetainsSealedReceiptOnPublicationError(t *testing.T) {
	f := newLLMReplayFixture(t)
	ledger := &llmPublicationFailureLedger{Store: f.store}
	service, err := durable.NewReplayableLLMCalls(ledger, f.model, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := service.Generate(context.Background(), f.open, f.request); err == nil {
			t.Fatal("expected publication failure")
		}
	}
	prepared, err := f.store.LoadCall(context.Background(), f.open.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Call.State == domain.CallRecordTerminal {
		t.Fatal("recoverable receipt was discarded as a terminal failure")
	}
	result := f.generate(t, f.service)
	if f.httpCalls.Load() != 1 || result.Value.RawBlob == nil {
		t.Fatal("sealed receipt recovery failed")
	}
}

type llmPublicationFailureLedger struct{ *sqlite.Store }

func (l *llmPublicationFailureLedger) FinalizeArtifact(context.Context, domain.ArtifactWriterTokenID, domain.BlobRef) error {
	return errors.New("injected publication receipt failure")
}

func TestLLMReplayCannotReleaseSlotsWhileProviderCallIsPending(t *testing.T) {
	f := newLLMReplayFixture(t)
	ledger := &interruptedLLMReplayLedger{Store: f.store, boundary: "sent"}
	service, err := durable.NewReplayableLLMCalls(ledger, f.model, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Generate(context.Background(), f.open, f.request); err == nil {
		t.Fatal("expected pending provider receipt")
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var id domain.CallRecordID
	if err := db.QueryRow(`SELECT call_record_id FROM call_records WHERE provider='private-blob'`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	prepared, err := f.store.LoadCall(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	request := f.open
	request.ID, request.Provider, request.RequestDigest = id, prepared.Call.Provider, prepared.Call.RequestDigest
	request.LogicalOperationID = prepared.Call.LogicalOperationID
	if err := f.store.ReleaseUnwrittenArtifactReservations(context.Background(), request); !errors.Is(err, sqlite.ErrInvalidTransition) {
		t.Fatalf("pending parent release: %v", err)
	}
	after, err := f.store.LoadCall(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.Reservations, after.Reservations) {
		t.Fatal("pending provider slots were released")
	}
	f.generate(t, f.service)
}

func (f llmReplayFixture) generate(t *testing.T, service *durable.LLMCalls) domain.MeteredOutcome[port.GenerateResponse] {
	t.Helper()
	result, err := service.Generate(context.Background(), f.open, f.request)
	if err != nil || result.Value == nil {
		t.Fatalf("Generate = %+v, %v", result, err)
	}
	return result
}
