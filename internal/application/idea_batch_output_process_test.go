package application_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/agent"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/runlock"
)

type ideaOutputProcessConfig struct {
	RunID                                               domain.RunID
	Database, BlobRoot, LockRoot, LLMEndpoint, Boundary string
	Content                                             application.GenerationReaderOptions
	Retry                                               domain.RetryPolicy
	Now                                                 time.Time
}

func TestIdeaBatchOutputSurvivesRealPublicationProcessExit(t *testing.T) {
	for _, boundary := range []string{"sealed", "finalized"} {
		t.Run(boundary, func(t *testing.T) {
			f := newGenerationExecutorFixture(t, 4, false)
			f.begin(t, "idea", f.snapshot.SnapshotDigest, 1)
			cfg := ideaOutputProcessConfig{RunID: f.runID, Database: f.path, BlobRoot: f.blobRoot, LockRoot: f.lockRoot,
				LLMEndpoint: f.endpoint, Content: f.options, Retry: f.executorConfig.RetryPolicy, Now: f.clock.Now(), Boundary: boundary}
			if err := f.runGuard.Close(); err != nil {
				t.Fatal(err)
			}
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			runChild := func(mode string, want int) {
				t.Helper()
				cfg.Boundary = mode
				cfg.Now = cfg.Now.Add(time.Second)
				raw, err := json.Marshal(cfg)
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIdeaBatchOutputProcessHelper$", "-test.timeout=25s")
				child.Env = append(os.Environ(), "CPGEN_IDEA_OUTPUT_PROCESS="+string(raw))
				out, err := child.CombinedOutput()
				if ctx.Err() != nil || child.ProcessState == nil || child.ProcessState.ExitCode() != want {
					t.Fatalf("typed output child %s: %v %s", mode, err, out)
				}
			}
			runChild(boundary, 73)
			runChild("", 0)
			runChild("", 0)
			if f.httpCalls.Load() != 1 {
				t.Fatalf("output recovery sent %d provider requests", f.httpCalls.Load())
			}
			db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.path))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var count int
			if err := db.QueryRow(`SELECT count(*) FROM artifact_occurrences`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("output restart occurrences=%d %v", count, err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM call_records WHERE state='TERMINAL'`).Scan(&count); err != nil || count != 3 {
				t.Fatalf("output restart terminal operations=%d %v", count, err)
			}
			if err := db.QueryRow(`SELECT count(*) FROM artifact_occurrences occurrence JOIN budget_reservations reservation ON reservation.reservation_id=occurrence.reservation_id JOIN blob_pins pin ON pin.pin_id=occurrence.pin_id WHERE occurrence.role='OUTPUT' AND reservation.state='SETTLED' AND reservation.settled_value=pin.physical_new_bytes`).Scan(&count); err != nil || count != 1 {
				t.Fatalf("output restart settlement=%d %v", count, err)
			}
		})
	}
}

func TestIdeaBatchOutputProcessHelper(t *testing.T) {
	raw := os.Getenv("CPGEN_IDEA_OUTPUT_PROCESS")
	if raw == "" {
		t.Skip("typed output process helper")
	}
	var cfg ideaOutputProcessConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source := newRecordingClock(cfg.Now)
	store, err := sqlite.OpenWithClock(ctx, sqlite.Config{Path: cfg.Database, BusyTimeout: time.Second, MaxReaders: 4}, source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	locks, err := runlock.NewManager(cfg.LockRoot, runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer locks.Close()
	guard, err := locks.AcquireRun(ctx, cfg.RunID, runlock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	artifactGuard, err := locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		t.Fatal(err)
	}
	defer artifactGuard.Close()
	run, err := store.GetRun(ctx, cfg.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CurrentStage == "statement" {
		if run.ActiveStartedAt != nil {
			t.Fatal("committed output retained active accounting")
		}
		return
	}
	blobs, err := blob.NewStore(cfg.BlobRoot)
	if err != nil {
		t.Fatal(err)
	}
	configuration := llmApplicationConfig(t)
	mapped, _, err := application.BuildLLMConfig(configuration)
	if err != nil {
		t.Fatal(err)
	}
	mapped.Endpoint, mapped.AllowInsecureHTTP = cfg.LLMEndpoint, true
	model, err := agent.NewLangChain(mapped)
	if err != nil {
		t.Fatal(err)
	}
	ideaRepair, _ := application.BuildFormatRepairPolicy(configuration, "idea.draft")
	statementRepair, _ := application.BuildFormatRepairPolicy(configuration, "statement.draft")
	ledger := &ideaOutputCrashStore{Store: store, boundary: cfg.Boundary}
	generation, err := application.NewGenerationExecutor(application.GenerationExecutorConfig{Store: ledger, Blobs: blobs, LLM: model,
		Clock: source, Locks: locks, Content: cfg.Content, IdeaRepair: ideaRepair, StatementRepair: statementRepair,
		RetryPolicy: cfg.Retry, CostUpperBoundMicroUSD: 100})
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := store.CurrentStageAttempt(ctx, cfg.RunID, "idea")
	if err != nil {
		t.Fatal(err)
	}
	budget, err := store.BudgetSnapshot(ctx, cfg.RunID)
	if err != nil {
		t.Fatal(err)
	}
	view, err := domain.NewRunView(domain.RunViewData{RunID: run.RunID, AttemptID: attempt.AttemptID, WorkflowRevision: run.WorkflowRevision,
		SchemaVersion: run.SchemaVersion, RequestDigest: run.RequestDigest, ConfigDigest: run.ConfigDigest, WorkflowDigest: run.WorkflowDigest,
		State: run.State, CurrentStage: run.CurrentStage, Version: run.Version, Budget: budget})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.ReadGenerationSnapshot(ctx, cfg.RunID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := generation.CollectIdeaCandidatesWithOutput(ctx, view, snapshot)
	if err != nil || result.Outcome.Value == nil {
		t.Fatalf("process collection=%+v %v", result, err)
	}
	if cfg.Boundary != "" {
		t.Fatal("typed output crash boundary was not reached")
	}
	active, err := application.NewActiveTime(store, source, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	run, _ = store.GetRun(ctx, cfg.RunID)
	if _, err := active.Stop(ctx, run.RunID, run.Version); err != nil {
		t.Fatal(err)
	}
	run, _ = store.GetRun(ctx, cfg.RunID)
	batch := result.Outcome.Value.BatchDigest
	if _, err := store.FinishStage(ctx, domain.FinishStageCommand{RunID: run.RunID, ExpectedRunVersion: run.Version, StageName: "idea", AttemptID: attempt.AttemptID,
		AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &batch, NextStage: "statement", NextInputDigest: &batch,
		Occurrences: result.Occurrences, IdempotencyKey: coordinatorID("finish", "typed output process"), At: source.Now()}); err != nil {
		t.Fatal(err)
	}
}

type ideaOutputCrashStore struct {
	*sqlite.Store
	boundary         string
	seals, finalizes int
}

func (s *ideaOutputCrashStore) SealArtifact(ctx context.Context, id domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	err := s.Store.SealArtifact(ctx, id, ref)
	s.seals++
	if err == nil && s.seals == 2 && s.boundary == "sealed" {
		os.Exit(73)
	}
	return err
}

func (s *ideaOutputCrashStore) FinalizeArtifact(ctx context.Context, id domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	err := s.Store.FinalizeArtifact(ctx, id, ref)
	s.finalizes++
	if err == nil && s.finalizes == 2 && s.boundary == "finalized" {
		os.Exit(73)
	}
	return err
}
