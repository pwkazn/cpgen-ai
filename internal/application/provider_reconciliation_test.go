package application_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
)

type providerReconcileFixture struct {
	coordinatorFixture
	open   domain.OpenCallRequest
	invoke func(durable.ArtifactCallLedger, bool) (domain.CallTrace, error)
	sends  *atomic.Int32
}

func newProviderReconcileFixture(t *testing.T, kind string) providerReconcileFixture {
	t.Helper()
	if kind == "similarity" {
		f := newSimilarityReplayFixture(t)
		invoke := func(ledger durable.ArtifactCallLedger, reconcile bool) (domain.CallTrace, error) {
			service, err := durable.NewReplayableSimilarityCalls(ledger, f.provider, f.blobs, f.clock, 100)
			if err != nil {
				return domain.CallTrace{}, err
			}
			call := service.SearchWithArtifacts
			if reconcile {
				call = service.Reconcile
			}
			result, err := call(context.Background(), f.open, f.request)
			return result.Outcome.CallTrace, err
		}
		return providerReconcileFixture{f.coordinatorFixture, f.open, invoke, f.httpCalls}
	}
	f := newLLMReplayFixture(t)
	invoke := func(ledger durable.ArtifactCallLedger, reconcile bool) (domain.CallTrace, error) {
		service, err := durable.NewReplayableLLMCalls(ledger, f.model, f.blobs, f.clock, 100)
		if err != nil {
			return domain.CallTrace{}, err
		}
		call := service.Generate
		if reconcile {
			call = service.Reconcile
		}
		result, err := call(context.Background(), f.open, f.request)
		return result.CallTrace, err
	}
	return providerReconcileFixture{f.coordinatorFixture, f.open, invoke, f.httpCalls}
}

func TestProviderReconcileNeverCreatesMissingCallsOrUnstartedPrivateSlots(t *testing.T) {
	for _, kind := range []string{"llm", "similarity"} {
		for _, boundary := range []string{"absent", "parent_open", "local_open", "provider_prepare"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				f := newProviderReconcileFixture(t, kind)
				ctx := context.Background()
				if boundary == "absent" {
					if _, err := f.invoke(f.store, true); !errors.Is(err, sqlite.ErrNotFound) || f.sends.Load() != 0 {
						t.Fatalf("missing operation created work: %v", err)
					}
					if _, err := f.store.ReadLogicalCall(ctx, f.open.ID); !errors.Is(err, sqlite.ErrNotFound) {
						t.Fatalf("missing operation was opened: %v", err)
					}
					return
				}
				if boundary == "parent_open" {
					if _, err := f.store.OpenCall(ctx, f.open); err != nil {
						t.Fatal(err)
					}
				} else {
					ledger := &stoppedReconcilePreparation{Store: f.store, allowLocal: boundary == "provider_prepare"}
					if _, err := f.invoke(ledger, false); err == nil || f.sends.Load() != 0 {
						t.Fatalf("preparation interruption failed: %v", err)
					}
				}
				requestProviderCleanupCancel(t, f)
				first, err := f.invoke(f.store, true)
				if err != nil || first.DispatchKind != domain.DispatchNone || f.sends.Load() != 0 {
					t.Fatalf("unstarted cleanup=%+v err=%v", first, err)
				}
				second, err := f.invoke(f.store, true)
				if err != nil || !first.Equal(second) {
					t.Fatalf("unstarted cleanup replay: %v", err)
				}
				db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var live, reserved int
				if err := db.QueryRow(`SELECT count(*) FROM call_records WHERE run_id=? AND state<>'TERMINAL'`, f.runID).Scan(&live); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT count(*) FROM budget_reservations WHERE run_id=? AND state='RESERVED'`, f.runID).Scan(&reserved); err != nil {
					t.Fatal(err)
				}
				if live != 0 || reserved != 0 {
					t.Fatalf("cleanup left calls=%d reservations=%d", live, reserved)
				}
			})
		}
	}
}

type stoppedReconcilePreparation struct {
	*sqlite.Store
	allowLocal bool
}

func (s *stoppedReconcilePreparation) PrepareCalls(ctx context.Context, request domain.PrepareCallsRequest) (domain.PreparedCalls, error) {
	call, err := s.ReadLogicalCall(ctx, request.CallRecordID)
	if err != nil {
		return domain.PreparedCalls{}, err
	}
	if !s.allowLocal || call.Provider != "private-blob" {
		return domain.PreparedCalls{}, errors.New("injected pre-dispatch interruption")
	}
	return s.Store.PrepareCalls(ctx, request)
}

func TestProviderReconcileRestoresSealedAndCompletedReceiptsAfterCancel(t *testing.T) {
	for _, kind := range []string{"llm", "similarity"} {
		for _, boundary := range []string{"sealed", "complete", "finish"} {
			t.Run(kind+"/"+boundary, func(t *testing.T) {
				f := newProviderReconcileFixture(t, kind)
				var ledger durable.ArtifactCallLedger = &interruptedLLMReplayLedger{Store: f.store, boundary: boundary}
				if boundary == "sealed" {
					ledger = &llmPublicationFailureLedger{Store: f.store}
				}
				if _, err := f.invoke(ledger, false); err == nil || f.sends.Load() != 1 {
					t.Fatalf("receipt interruption: %v HTTP=%d", err, f.sends.Load())
				}
				requestProviderCleanupCancel(t, f)
				first, err := f.invoke(f.store, true)
				if err != nil || first.DispatchKind != domain.DispatchDispatched || len(first.PhysicalAttemptCallIDs) != 1 || f.sends.Load() != 1 {
					t.Fatalf("receipt reconciliation=%+v err=%v HTTP=%d", first, err, f.sends.Load())
				}
				second, err := f.invoke(f.store, true)
				if err != nil || !reflect.DeepEqual(first, second) || f.sends.Load() != 1 {
					t.Fatalf("reconciled response changed: %v", err)
				}
			})
		}
	}
}

func TestProviderReceiptCannotBeReleasedBeforeParentReconciliation(t *testing.T) {
	for _, kind := range []string{"llm", "similarity"} {
		for _, action := range []string{"cancel", "interrupt"} {
			t.Run(kind+"/"+action, func(t *testing.T) {
				f := newProviderReconcileFixture(t, kind)
				ctx := context.Background()
				if _, err := f.invoke(&llmPublicationFailureLedger{Store: f.store}, false); err == nil || f.sends.Load() != 1 {
					t.Fatalf("sealed receipt fixture: %v", err)
				}
				version := int64(2)
				if action == "cancel" {
					requestProviderCleanupCancel(t, f)
					version = 3
				}
				finish := func() error {
					if action == "interrupt" {
						_, err := f.store.InterruptStage(ctx, domain.InterruptStageCommand{RunID: f.runID, ExpectedRunVersion: version, StageName: f.open.StageName, AttemptID: f.attemptID, Cause: domain.CauseStepDeadline, IdempotencyKey: coordinatorID("interrupt", "unresolved-provider"), At: f.clock.Now()})
						return err
					}
					cause := domain.CauseUserCancel
					_, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: version, StageName: f.open.StageName, AttemptID: f.attemptID, AttemptState: domain.StageAttemptCancelled, RunState: domain.RunCancelled, Cause: &cause, IdempotencyKey: coordinatorID("finish", "unresolved-provider"), At: f.clock.Now()})
					return err
				}
				if err := finish(); !errors.Is(err, sqlite.ErrInvalidTransition) {
					t.Fatalf("unresolved receipt release was not refused: %v", err)
				}
				current, err := f.store.GetRun(ctx, f.runID)
				if err != nil || current.Version != version || current.State != domain.RunRunning {
					t.Fatalf("failed release changed run: %+v %v", current, err)
				}
				db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var sealed, released int
				if err := db.QueryRow(`SELECT count(*) FROM artifact_writer_tokens WHERE run_id=? AND state='SEALED'`, f.runID).Scan(&sealed); err != nil {
					t.Fatal(err)
				}
				if err := db.QueryRow(`SELECT count(*) FROM artifact_writer_tokens WHERE run_id=? AND state='RELEASED'`, f.runID).Scan(&released); err != nil {
					t.Fatal(err)
				}
				if sealed != 1 || released != 0 {
					t.Fatalf("guard lost sealed receipt: sealed=%d released=%d", sealed, released)
				}
				if _, err := f.invoke(f.store, true); err != nil || f.sends.Load() != 1 {
					t.Fatalf("original receipt reconciliation: %v", err)
				}
				if err := finish(); err != nil {
					t.Fatalf("settled receipt release failed: %v", err)
				}
			})
		}
	}
}

func TestStructuredReconcileDoesNotStartAnUnopenedFormatRepair(t *testing.T) {
	f := newStructuredLLMFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls, err := durable.NewReplayableLLMCalls(&cancelStructuredLLMLedger{Store: f.store, cancel: cancel}, f.model, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	service, err := durable.NewStructuredLLMCalls(calls, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	first, err := service.Generate(ctx, f.open, f.request)
	if !errors.Is(err, context.Canceled) || len(first.Artifacts) != 1 || f.httpCalls.Load() != 1 {
		t.Fatalf("repair boundary=%+v err=%v", first, err)
	}
	requestProviderCleanupCancel(t, providerReconcileFixture{coordinatorFixture: f.coordinatorFixture})
	for i := 0; i < 2; i++ {
		reconciled, err := f.structured.Reconcile(context.Background(), f.open, f.request)
		if err != nil || reconciled.Outcome.Failure == nil || len(reconciled.Artifacts) != 1 || len(reconciled.CallTraces) != 1 || f.httpCalls.Load() != 1 {
			t.Fatalf("cleanup started format repair: %+v err=%v HTTP=%d", reconciled, err, f.httpCalls.Load())
		}
	}
}

func TestStructuredReconcileRestoresExistingRepairAndPreservesMissingReceiptError(t *testing.T) {
	f := newStructuredLLMFixture(t, false)
	ctx := context.Background()
	first, err := f.structured.Generate(ctx, f.open, f.request)
	if err != nil || first.Outcome.Value == nil || len(first.Artifacts) != 2 || f.httpCalls.Load() != 2 {
		t.Fatalf("repair fixture=%+v err=%v", first, err)
	}
	requestProviderCleanupCancel(t, providerReconcileFixture{coordinatorFixture: f.coordinatorFixture})
	ledger := &missingRepairPendingArtifact{Store: f.store, token: first.Artifacts[1].WriterTokenID}
	calls, err := durable.NewReplayableLLMCalls(ledger, f.model, f.blobs, f.clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	service, err := durable.NewStructuredLLMCalls(calls, f.policy)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Reconcile(ctx, f.open, f.request); !errors.Is(err, sqlite.ErrNotFound) || f.httpCalls.Load() != 2 {
		t.Fatalf("existing repair's missing receipt was treated as an unopened repair: %v", err)
	}
	replayed, err := f.structured.Reconcile(ctx, f.open, f.request)
	if err != nil || !reflect.DeepEqual(first, replayed) || f.httpCalls.Load() != 2 {
		t.Fatalf("existing repair reconciliation changed result: %v", err)
	}
}

type missingRepairPendingArtifact struct {
	*sqlite.Store
	token domain.ArtifactWriterTokenID
}

func (l *missingRepairPendingArtifact) ReadPendingArtifact(ctx context.Context, id domain.ArtifactDeclarationID) (domain.PendingArtifact, error) {
	artifact, err := l.Store.ReadPendingArtifact(ctx, id)
	if err == nil && artifact.WriterTokenID == l.token {
		return domain.PendingArtifact{}, sqlite.ErrNotFound
	}
	return artifact, err
}

func requestProviderCleanupCancel(t *testing.T, f providerReconcileFixture) {
	t.Helper()
	_, err := f.store.RequestCancel(context.Background(), domain.CancelRequest{ID: "control_00000000000000000000000000002601", RunID: f.runID, ExpectedRunVersion: 2, Reason: "settle original provider calls", IdempotencyKey: coordinatorID("cancel", "provider-reconciliation"), At: f.clock.Now()})
	if err != nil {
		t.Fatal(err)
	}
}
