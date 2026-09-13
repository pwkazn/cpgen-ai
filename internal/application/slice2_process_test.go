package application_test

import (
	"context"
	"encoding/json"
	"errors"
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
	"cpgen/internal/similarity"
)

type slice2ProcessConfig struct {
	LLMEndpoint, SimilarityEndpoint, Database, BlobRoot, LockRoot, Boundary, ResultPath string
	RunID                                                                               domain.RunID
	Stage                                                                               domain.StageName
	Now                                                                                 time.Time
	Content                                                                             application.GenerationReaderOptions
	Retry                                                                               domain.RetryPolicy
	IdeaRepairs, StatementRepairs                                                       int64
	SimilarityPolicy                                                                    similarity.DecisionPolicy
	EffectiveConfig                                                                     json.RawMessage
}

func TestSlice2RunServiceProcessCrashBoundaries(t *testing.T) {
	for _, stage := range []domain.StageName{"idea", "statement", "similarity"} {
		for _, boundary := range []string{"attempt-started", "before-seal", "sealed", "provider-completed", "stage-committed"} {
			t.Run(string(stage)+"/"+boundary, func(t *testing.T) {
				f := newSimilarityExecutorFixture(t, 3)
				_, raw, err := f.store.RunViewDocuments(context.Background(), f.runID)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.runGuard.Close(); err != nil {
					t.Fatal(err)
				}
				configuration := slice2ProcessConfig{
					LLMEndpoint: f.generationExecutorFixture.endpoint, SimilarityEndpoint: f.endpoint, Database: f.path,
					BlobRoot: f.blobRoot, LockRoot: f.lockRoot, Boundary: boundary, ResultPath: filepath.Join(t.TempDir(), "resumed.json"),
					RunID: f.runID, Stage: stage, Now: f.clock.Now().Add(time.Second), Content: f.options, Retry: f.executorConfig.RetryPolicy,
					IdeaRepairs: f.executorConfig.IdeaRepair.MaxRepairs, StatementRepairs: f.executorConfig.StatementRepair.MaxRepairs,
					SimilarityPolicy: f.config.Policy, EffectiveConfig: raw,
				}
				runSlice2Process(t, configuration, 73)
				before, err := f.store.CurrentStageAttempt(context.Background(), f.runID, stage)
				if err != nil {
					t.Fatal(err)
				}
				if before.Ordinal != 1 {
					t.Fatal("crash process replaced the initial attempt")
				}
				beforeLLM, beforeSim := f.httpCalls.Load(), f.sends.Load()
				configuration.Boundary = ""
				configuration.Now = configuration.Now.Add(time.Minute)
				runSlice2Process(t, configuration, 0)
				resultRaw, err := os.ReadFile(configuration.ResultPath)
				if err != nil {
					t.Fatal(err)
				}
				var result domain.RunSnapshot
				if err := json.Unmarshal(resultRaw, &result); err != nil {
					t.Fatal(err)
				}
				if result.State != domain.RunNeedsReview || result.ActiveStartedAt != nil {
					t.Fatalf("resumed=%+v", result)
				}
				if boundary == "before-seal" {
					if result.CurrentStage != stage || f.httpCalls.Load() != beforeLLM || f.sends.Load() != beforeSim {
						t.Fatalf("unknown boundary resent or advanced: %+v HTTP=%d/%d", result, f.httpCalls.Load(), f.sends.Load())
					}
				} else {
					if result.CurrentStage != "slice2_checkpoint" || f.httpCalls.Load() != 2 || f.sends.Load() != 1 {
						t.Fatalf("resumed chain=%+v HTTP=%d/%d", result, f.httpCalls.Load(), f.sends.Load())
					}
					content, err := f.service.ReadCommitted(context.Background(), f.runID)
					if err != nil || content.Statement.Idea.Snapshot.SnapshotDigest != f.snapshot.SnapshotDigest || !content.Decision.IsAccept() {
						t.Fatalf("persisted chain=%+v %v", content, err)
					}
				}
				after, err := f.store.CurrentStageAttempt(context.Background(), f.runID, stage)
				if err != nil || after.AttemptID != before.AttemptID || after.Ordinal != 1 {
					t.Fatalf("restart replaced durable attempt: %+v %v", after, err)
				}
				again, err := slice2FixtureService(t, f).Resume(context.Background(), f.runID)
				if err != nil || again.Version != result.Version {
					t.Fatalf("settled restart changed state: %+v %v", again, err)
				}
			})
		}
	}
}

func runSlice2Process(t *testing.T, configuration slice2ProcessConfig, exitCode int) {
	t.Helper()
	raw, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSlice2RunServiceProcessHelper$")
	command.Env = append(os.Environ(), "CPGEN_SLICE2_PROCESS_FIXTURE="+string(raw))
	output, err := command.CombinedOutput()
	if exitCode == 0 {
		if err != nil {
			t.Fatalf("resume process: %v\n%s", err, output)
		}
		return
	}
	var exited *exec.ExitError
	if !errors.As(err, &exited) || exited.ExitCode() != exitCode {
		t.Fatalf("crash process: %v\n%s", err, output)
	}
}

func TestSlice2RunServiceProcessHelper(t *testing.T) {
	raw := os.Getenv("CPGEN_SLICE2_PROCESS_FIXTURE")
	if raw == "" {
		t.Skip("only runs as a full run-service crash/restart process")
	}
	var cfg slice2ProcessConfig
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	source := serviceFixtureClock{newRecordingClock(cfg.Now)}
	store, err := sqlite.OpenWithClock(ctx, sqlite.Config{Path: cfg.Database, BusyTimeout: time.Second, MaxReaders: 4}, source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	blobs, err := blob.NewStore(cfg.BlobRoot)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := runlock.NewManager(cfg.LockRoot, runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer locks.Close()
	applicationConfig := llmApplicationConfig(t)
	llmCfg, _, err := application.BuildLLMConfig(applicationConfig)
	if err != nil {
		t.Fatal(err)
	}
	llmCfg.Endpoint, llmCfg.AllowInsecureHTTP = cfg.LLMEndpoint, true
	model, err := agent.NewLangChain(llmCfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger := &slice2CrashStore{Store: store, configuration: cfg}
	applicationConfig.LLM.MaxFormatRepairs = cfg.IdeaRepairs
	ideaRepair, err := application.BuildFormatRepairPolicy(applicationConfig, "idea.draft")
	if err != nil {
		t.Fatal(err)
	}
	applicationConfig.LLM.MaxFormatRepairs = cfg.StatementRepairs
	statementRepair, err := application.BuildFormatRepairPolicy(applicationConfig, "statement.draft")
	if err != nil {
		t.Fatal(err)
	}
	generation, err := application.NewGenerationExecutor(application.GenerationExecutorConfig{Store: ledger, Blobs: blobs, LLM: model, Clock: source, Locks: locks, Content: cfg.Content, IdeaRepair: ideaRepair, StatementRepair: statementRepair, RetryPolicy: cfg.Retry, CostUpperBoundMicroUSD: 100})
	if err != nil {
		t.Fatal(err)
	}
	provider, _ := durableSimilarityProvider(t, cfg.SimilarityEndpoint)
	sim, err := application.NewSimilarityExecutor(application.SimilarityExecutorConfig{Generation: generation, Provider: provider, Policy: cfg.SimilarityPolicy, Limit: 10, RetryPolicy: cfg.Retry, CostUpperBoundMicroUSD: 100})
	if err != nil {
		t.Fatal(err)
	}
	service, err := application.NewSlice2RunService(application.Slice2RunServiceConfig{Generation: generation, Similarity: sim, Reviews: store, ActiveTimeInterval: time.Second, EffectiveConfigJSON: cfg.EffectiveConfig})
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Resume(ctx, cfg.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Boundary != "" {
		t.Fatal("run-service crash boundary was not reached")
	}
	resultRaw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.ResultPath, resultRaw, 0600); err != nil {
		t.Fatal(err)
	}
}

type slice2CrashStore struct {
	*sqlite.Store
	configuration slice2ProcessConfig
}

func (s *slice2CrashStore) crash(stage domain.StageName, boundary string) {
	if s.configuration.Stage == stage && s.configuration.Boundary == boundary {
		os.Exit(73)
	}
}

func (s *slice2CrashStore) BeginStage(ctx context.Context, command domain.BeginStageCommand) (domain.StageAttempt, error) {
	result, err := s.Store.BeginStage(ctx, command)
	if err == nil {
		s.crash(command.StageName, "attempt-started")
	}
	return result, err
}

func (s *slice2CrashStore) SealArtifact(ctx context.Context, id domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	current, err := s.Store.GetRun(ctx, s.configuration.RunID)
	if err != nil {
		return err
	}
	s.crash(current.CurrentStage, "before-seal")
	err = s.Store.SealArtifact(ctx, id, ref)
	if err == nil {
		s.crash(current.CurrentStage, "sealed")
	}
	return err
}

func (s *slice2CrashStore) CompletePhysical(ctx context.Context, command domain.CompletePhysicalRequest) error {
	err := s.Store.CompletePhysical(ctx, command)
	if err != nil {
		return err
	}
	call, err := s.Store.ReadLogicalCall(ctx, command.CallRecordID)
	if err != nil {
		return err
	}
	if call.Provider != "private-blob" {
		s.crash(command.StageName, "provider-completed")
	}
	return nil
}

func (s *slice2CrashStore) FinishStage(ctx context.Context, command domain.FinishStageCommand) (domain.RunSnapshot, error) {
	result, err := s.Store.FinishStage(ctx, command)
	if err == nil && command.AttemptState == domain.StageAttemptSucceeded {
		s.crash(command.StageName, "stage-committed")
	}
	return result, err
}
