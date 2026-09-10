package application_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestSlice2RunServiceGeneratesCommittedChainToUnfinishedCheckpoint(t *testing.T) {
	f := newSimilarityExecutorFixture(t, 3)
	service := slice2FixtureService(t, f)
	seed := f.snapshot.EffectiveSeed
	request := domain.RunRequest(f.snapshot.Request)
	request.Seed = &seed
	result, err := service.Generate(context.Background(), request)
	if err != nil || result.State != domain.RunNeedsReview || result.CurrentStage != "slice2_checkpoint" || result.ActiveStartedAt != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	content, err := f.service.ReadCommitted(context.Background(), result.RunID)
	if err != nil || !content.Decision.IsAccept() || content.Statement.Idea.Snapshot.EffectiveSeed != seed || f.httpCalls.Load() != 2 || f.sends.Load() != 1 {
		t.Fatalf("chain=%+v err=%v HTTP=%d/%d", content, err, f.httpCalls.Load(), f.sends.Load())
	}
	resumed, err := slice2FixtureService(t, f).Resume(context.Background(), result.RunID)
	if err != nil || resumed.Version != result.Version || f.httpCalls.Load() != 2 || f.sends.Load() != 1 {
		t.Fatalf("review resume=%+v err=%v", resumed, err)
	}
}

func TestSlice2RunServiceResumesSameAttemptAcrossPrivateReceiptInterruption(t *testing.T) {
	for _, boundary := range []string{"before_call", "sealed", "completed"} {
		t.Run(boundary, func(t *testing.T) {
			f := newSimilarityExecutorFixture(t, 3)
			view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
			if boundary != "before_call" {
				cfg := f.executorConfig
				if boundary == "sealed" {
					cfg.Store = &llmPublicationFailureLedger{f.store}
				}
				executor, err := application.NewGenerationExecutor(cfg)
				if err != nil {
					t.Fatal(err)
				}
				result, err := executor.RunIdea(context.Background(), view, f.snapshot)
				if boundary == "sealed" && err == nil {
					t.Fatal("receipt interruption did not occur")
				}
				if boundary == "completed" && (err != nil || result.Outcome.Value == nil) {
					t.Fatalf("completed=%+v %v", result, err)
				}
				if f.httpCalls.Load() != 1 {
					t.Fatal("initial request count differs")
				}
			}
			if err := f.runGuard.Close(); err != nil {
				t.Fatal(err)
			}
			result, err := slice2FixtureService(t, f).Resume(context.Background(), f.runID)
			if err != nil || result.State != domain.RunNeedsReview || result.CurrentStage != "slice2_checkpoint" || f.httpCalls.Load() != 2 || f.sends.Load() != 1 {
				t.Fatalf("resume=%+v err=%v HTTP=%d/%d", result, err, f.httpCalls.Load(), f.sends.Load())
			}
			attempt, err := f.store.CurrentStageAttempt(context.Background(), f.runID, "idea")
			if err != nil || attempt.AttemptID != view.AttemptID() || attempt.Ordinal != 1 || attempt.State != domain.StageAttemptSucceeded {
				t.Fatalf("attempt replaced: %+v %v", attempt, err)
			}
		})
	}
}

func TestSlice2RunServiceCancelReconcilesCurrentProvidersWithoutNewWork(t *testing.T) {
	for _, stage := range []string{"idea", "similarity"} {
		for _, boundary := range []string{"absent", "open", "sealed", "completed"} {
			t.Run(stage+"/"+boundary, func(t *testing.T) {
				f := newSimilarityExecutorFixture(t, 3)
				var view domain.RunView
				var input domain.SimilarityInputV1
				if stage == "idea" {
					view = f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
				} else {
					input, view = f.beginSimilarity(t)
				}
				if boundary != "absent" {
					cfg := f.executorConfig
					switch boundary {
					case "open":
						cfg.Store = &stoppedReconcilePreparation{Store: f.store}
					case "sealed":
						cfg.Store = &llmPublicationFailureLedger{f.store}
					}
					executor, err := application.NewGenerationExecutor(cfg)
					if err != nil {
						t.Fatal(err)
					}
					if stage == "idea" {
						_, err = executor.RunIdea(context.Background(), view, f.snapshot)
					} else {
						cfg := f.config
						cfg.Generation = executor
						sim, createErr := application.NewSimilarityExecutor(cfg)
						if createErr != nil {
							t.Fatal(createErr)
						}
						_, err = sim.RunSimilarity(context.Background(), view, input)
					}
					if boundary != "completed" && err == nil {
						t.Fatal("expected injected interruption")
					}
					if boundary == "completed" && err != nil {
						t.Fatal(err)
					}
				}
				beforeLLM, beforeSim := f.httpCalls.Load(), f.sends.Load()
				if err := f.runGuard.Close(); err != nil {
					t.Fatal(err)
				}
				service := slice2FixtureService(t, f)
				current, err := f.store.GetRun(context.Background(), f.runID)
				if err != nil {
					t.Fatal(err)
				}
				result, err := service.Cancel(context.Background(), domain.CancelRequest{ID: "control_00000000000000000000000000002601", RunID: f.runID, ExpectedRunVersion: current.Version, IdempotencyKey: coordinatorID("cancel", "slice2"), At: f.clock.Now(), Reason: "fixture cancel"})
				if err != nil || result.State != domain.RunCancelled || result.ActiveStartedAt != nil || f.httpCalls.Load() != beforeLLM || f.sends.Load() != beforeSim {
					t.Fatalf("cancel=%+v err=%v HTTP=%d/%d", result, err, f.httpCalls.Load(), f.sends.Load())
				}
				db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
				if err != nil {
					t.Fatal(err)
				}
				defer db.Close()
				var live int
				if err := db.QueryRow(`SELECT count(*) FROM call_records WHERE run_id=? AND state<>'TERMINAL'`, f.runID).Scan(&live); err != nil || live != 0 {
					t.Fatalf("live calls=%d %v", live, err)
				}
				again, err := service.Resume(context.Background(), f.runID)
				if err != nil || again.Version != result.Version || f.httpCalls.Load() != beforeLLM || f.sends.Load() != beforeSim {
					t.Fatalf("cancel resume=%+v %v", again, err)
				}
			})
		}
	}
}

func TestSlice2RunServiceBlockedResumeChecksDependencyWithinNewAttempt(t *testing.T) {
	var calls atomic.Int32
	f := newSimilarityExecutorFixture(t, 3, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		similarityExecutorSuccess(w, r)
	})
	if err := f.runGuard.Close(); err != nil {
		t.Fatal(err)
	}
	service := slice2FixtureService(t, f)
	blocked, err := service.Resume(context.Background(), f.runID)
	if err != nil || blocked.State != domain.RunBlocked || blocked.CurrentStage != "similarity" || blocked.ActiveStartedAt != nil {
		t.Fatalf("blocked=%+v %v", blocked, err)
	}
	before, err := f.store.CurrentStageAttempt(context.Background(), f.runID, "similarity")
	if err != nil {
		t.Fatal(err)
	}
	result, err := slice2FixtureService(t, f).Resume(context.Background(), f.runID)
	if err != nil || result.State != domain.RunNeedsReview || result.CurrentStage != "slice2_checkpoint" || f.sends.Load() != 2 || f.httpCalls.Load() != 2 {
		t.Fatalf("rechecked=%+v %v HTTP=%d/%d", result, err, f.httpCalls.Load(), f.sends.Load())
	}
	after, err := f.store.CurrentStageAttempt(context.Background(), f.runID, "similarity")
	if err != nil || after.Ordinal != before.Ordinal+1 || after.AttemptID == before.AttemptID {
		t.Fatalf("dependency attempt reused: %+v %v", after, err)
	}
}

func TestSlice2RunServiceExhaustedBudgetReconcilesSealedReceiptWithoutContinuation(t *testing.T) {
	for _, stage := range []string{"idea", "similarity"} {
		t.Run(stage, func(t *testing.T) {
			f := newSimilarityExecutorFixture(t, 3)
			cfg := f.executorConfig
			cfg.Store = &llmPublicationFailureLedger{f.store}
			executor, err := application.NewGenerationExecutor(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if stage == "idea" {
				view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
				_, err = executor.RunIdea(context.Background(), view, f.snapshot)
			} else {
				input, view := f.beginSimilarity(t)
				cfg := f.config
				cfg.Generation = executor
				sim, createErr := application.NewSimilarityExecutor(cfg)
				if createErr != nil {
					t.Fatal(createErr)
				}
				_, err = sim.RunSimilarity(context.Background(), view, input)
			}
			if err == nil {
				t.Fatal("expected sealed receipt interruption")
			}
			beforeLLM, beforeSim := f.httpCalls.Load(), f.sends.Load()
			f.clock.mu.Lock()
			f.clock.now = f.clock.now.Add(2 * time.Minute)
			f.clock.mu.Unlock()
			current, err := f.store.GetRun(context.Background(), f.runID)
			if err != nil {
				t.Fatal(err)
			}
			accounted, err := f.active.Heartbeat(context.Background(), f.runID, current.Version)
			if err != nil || !accounted.Exhausted {
				t.Fatalf("budget not exhausted: %+v %v", accounted, err)
			}
			if err := f.runGuard.Close(); err != nil {
				t.Fatal(err)
			}
			result, err := slice2FixtureService(t, f).Resume(context.Background(), f.runID)
			if err != nil || result.State != domain.RunNeedsReview || result.CurrentStage != domain.StageName(stage) || result.ActiveStartedAt != nil || f.httpCalls.Load() != beforeLLM || f.sends.Load() != beforeSim {
				t.Fatalf("exhausted cleanup=%+v %v HTTP=%d/%d", result, err, f.httpCalls.Load(), f.sends.Load())
			}
		})
	}
}

func TestSlice2RunServicePendingCancelSurvivesRestartWithSealedReceipt(t *testing.T) {
	f := newSimilarityExecutorFixture(t, 3)
	view := f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
	cfg := f.executorConfig
	cfg.Store = &llmPublicationFailureLedger{f.store}
	executor, err := application.NewGenerationExecutor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := executor.RunIdea(context.Background(), view, f.snapshot); err == nil {
		t.Fatal("expected receipt interruption")
	}
	current, err := f.store.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RequestCancel(context.Background(), domain.CancelRequest{ID: "control_00000000000000000000000000002602", RunID: f.runID, ExpectedRunVersion: current.Version, IdempotencyKey: coordinatorID("cancel", "restart-slice2"), At: f.clock.Now(), Reason: "cancel before owner restart"}); err != nil {
		t.Fatal(err)
	}
	if err := f.runGuard.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := slice2FixtureService(t, f).Resume(context.Background(), f.runID)
	if err != nil || result.State != domain.RunCancelled || f.httpCalls.Load() != 1 || f.sends.Load() != 0 {
		t.Fatalf("pending cancel=%+v %v", result, err)
	}
	attempt, err := f.store.CurrentStageAttempt(context.Background(), f.runID, "idea")
	if err != nil || attempt.AttemptID != view.AttemptID() || attempt.State != domain.StageAttemptCancelled {
		t.Fatalf("cancel replaced attempt: %+v %v", attempt, err)
	}
}

func TestSlice2RunServiceRejectsChangedConfigBeforeResumeOrCancelMutation(t *testing.T) {
	f := newSimilarityExecutorFixture(t, 3)
	if err := f.runGuard.Close(); err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"changed":"configuration"}`)
	cfg := f.executorConfig
	cfg.Content.ProviderPolicyDigest = domain.SumBytes(raw)
	generation, err := application.NewGenerationExecutor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	simCfg := f.config
	simCfg.Generation = generation
	sim, err := application.NewSimilarityExecutor(simCfg)
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewSlice2RunService(application.Slice2RunServiceConfig{Generation: generation, Similarity: sim, EffectiveConfigJSON: raw})
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.store.GetRun(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resume(context.Background(), f.runID); err == nil {
		t.Fatal("changed configuration resumed")
	}
	if _, err := service.Cancel(context.Background(), domain.CancelRequest{ID: "control_00000000000000000000000000002603", RunID: f.runID, ExpectedRunVersion: before.Version, Reason: "config mismatch", IdempotencyKey: coordinatorID("cancel", "changed-slice2"), At: f.clock.Now()}); err == nil {
		t.Fatal("changed configuration wrote cancellation")
	}
	after, err := f.store.GetRun(context.Background(), f.runID)
	if err != nil || after.Version != before.Version || f.httpCalls.Load() != 0 || f.sends.Load() != 0 {
		t.Fatalf("mismatch changed state: %+v %v", after, err)
	}
	if pending, err := f.store.PendingCancel(context.Background(), f.runID); err != nil || pending != nil {
		t.Fatalf("mismatch created pending cancel: %+v %v", pending, err)
	}
}

func TestSlice2RunServiceOwnerObservesExternalCancelDuringProviderRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	generation := newGenerationExecutorFixtureWithWorkflow(t, 4, false, workflow.Slice2CheckpointWorkflowRevision, 3, func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	f := similarityExecutorFixtureFromGeneration(t, generation)
	if err := f.runGuard.Close(); err != nil {
		t.Fatal(err)
	}
	// Resume performs durable preparation before entering the provider. This
	// is a synchronization test, not a ten-second startup benchmark; SQLite
	// race instrumentation is substantially slower on shared CI runners.
	ctx, stop := context.WithTimeout(context.Background(), 2*time.Minute)
	defer stop()
	// Use the normal accounting cadence. The provider-start channel fixes the
	// cancellation boundary; a 100 Hz heartbeat is unrelated to this contract.
	// Version-conflict retries are exercised separately with injected conflicts.
	owner := slice2FixtureService(t, f)
	type completion struct {
		snapshot domain.RunSnapshot
		err      error
	}
	done := make(chan completion, 1)
	go func() { result, err := owner.Resume(ctx, f.runID); done <- completion{result, err} }()
	select {
	case <-started:
	case result := <-done:
		t.Fatalf("owner exited before provider started: snapshot=%+v err=%v", result.snapshot, result.err)
	case <-ctx.Done():
		t.Fatalf("provider did not start: %v", ctx.Err())
	}
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	other := slice2FixtureService(t, f)
	for tries := 0; tries < 16; tries++ {
		_, err = other.Cancel(ctx, domain.CancelRequest{ID: "control_00000000000000000000000000002604", RunID: f.runID, ExpectedRunVersion: current.Version, Reason: "cancel active owner", IdempotencyKey: coordinatorID("cancel", "live-owner"), At: f.clock.Now()})
		if !errors.Is(err, sqlite.ErrVersionConflict) {
			break
		}
		current, err = f.store.GetRun(ctx, f.runID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil || result.snapshot.State != domain.RunCancelled || result.snapshot.ActiveStartedAt != nil || f.httpCalls.Load() != 1 || f.sends.Load() != 0 {
			t.Fatalf("owner cancel=%+v %v HTTP=%d/%d", result.snapshot, result.err, f.httpCalls.Load(), f.sends.Load())
		}
	case <-ctx.Done():
		t.Fatal("owner did not finish cancellation")
	}
}

func TestSlice2RunServiceCommitErrorsPreserveProviderIdentityAndOccurrences(t *testing.T) {
	for _, stage := range []domain.StageName{"idea", "statement", "similarity"} {
		for _, afterCommit := range []bool{false, true} {
			boundary := "before"
			if afterCommit {
				boundary = "after"
			}
			t.Run(string(stage)+"/"+boundary, func(t *testing.T) {
				f := newSimilarityExecutorFixture(t, 3)
				runtime := &graphRuntime{Store: f.store}
				if afterCommit {
					runtime.interruptAfterStage = stage
				} else {
					runtime.failStage = stage
				}
				f.executorConfig.Store = runtime
				if err := f.runGuard.Close(); err != nil {
					t.Fatal(err)
				}
				_, err := slice2FixtureService(t, f).Resume(context.Background(), f.runID)
				if !errors.Is(err, errGraphStageCommit) {
					t.Fatalf("commit interruption was not reached: %v", err)
				}
				before, err := f.store.CurrentStageAttempt(context.Background(), f.runID, stage)
				if err != nil {
					t.Fatal(err)
				}
				if afterCommit && before.State != domain.StageAttemptSucceeded {
					t.Fatal("committed result was lost")
				}
				if !afterCommit && before.State != domain.StageAttemptRunning {
					t.Fatal("rejected commit changed attempt projection")
				}
				runtime.failStage, runtime.interruptAfterStage = "", ""
				result, err := slice2FixtureService(t, f).Resume(context.Background(), f.runID)
				if err != nil || result.State != domain.RunNeedsReview || result.CurrentStage != "slice2_checkpoint" || f.httpCalls.Load() != 2 || f.sends.Load() != 1 {
					t.Fatalf("commit recovery=%+v %v HTTP=%d/%d", result, err, f.httpCalls.Load(), f.sends.Load())
				}
				after, err := f.store.CurrentStageAttempt(context.Background(), f.runID, stage)
				if err != nil || after.AttemptID != before.AttemptID || after.Ordinal != 1 {
					t.Fatalf("commit recovery replaced attempt: %+v %v", after, err)
				}
				if _, err := f.service.ReadCommitted(context.Background(), f.runID); err != nil {
					t.Fatal(err)
				}
				refs, err := f.store.CommittedArtifactReferences(context.Background(), f.runID)
				if err != nil || len(refs) != 3 {
					t.Fatalf("occurrences duplicated or lost: %d %v", len(refs), err)
				}
			})
		}
	}
}

func slice2FixtureService(t *testing.T, f *similarityExecutorFixture, intervals ...time.Duration) *application.LocalRunService {
	t.Helper()
	generationConfig := f.executorConfig
	generationConfig.Clock = serviceFixtureClock{generationConfig.Clock}
	generation, err := application.NewGenerationExecutor(generationConfig)
	if err != nil {
		t.Fatal(err)
	}
	similarityConfig := f.config
	similarityConfig.Generation = generation
	similarityExecutor, err := application.NewSimilarityExecutor(similarityConfig)
	if err != nil {
		t.Fatal(err)
	}
	_, raw, err := f.store.RunViewDocuments(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	interval := time.Second
	if len(intervals) != 0 {
		interval = intervals[0]
	}
	service, err := application.NewSlice2RunService(application.Slice2RunServiceConfig{
		Generation: generation, Similarity: similarityExecutor, Reviews: f.store,
		ActiveTimeInterval: interval, EffectiveConfigJSON: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

// Provider fixtures advance Now deterministically, but foreground pollers must
// wait instead of spinning through the whole active-time allowance.
type serviceFixtureClock struct{ clock.Clock }

func (c serviceFixtureClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
