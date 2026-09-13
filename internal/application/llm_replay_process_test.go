package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type llmReplayProcessConfig struct {
	Endpoint, Database, BlobRoot, Boundary string
	Open                                   domain.OpenCallRequest
	Request                                port.GenerateRequest
	Now                                    time.Time
	RepairPolicy                           *application.FormatRepairPolicy
	CrashOccurrence                        int
}

func TestLLMReplayProcessCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"before-seal", "sealed", "finalized", "sent", "completed", "finished"} {
		t.Run(boundary, func(t *testing.T) {
			f := newLLMReplayFixture(t)
			configuration, err := json.Marshal(llmReplayProcessConfig{Endpoint: f.endpoint, Database: f.path, BlobRoot: f.blobRoot, Boundary: boundary, Open: f.open, Request: f.request, Now: f.clock.Now().Add(time.Second)})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLLMReplayProcessHelper$")
			command.Env = append(os.Environ(), "CPGEN_LLM_REPLAY_PROCESS_FIXTURE="+string(configuration))
			output, err := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 73 {
				t.Fatalf("crash helper: %v\n%s", err, output)
			}
			if f.httpCalls.Load() != 1 {
				t.Fatalf("before recovery: calls=%d", f.httpCalls.Load())
			}
			// The new executor's clock must follow the dead process's last write.
			f.clock.mu.Lock()
			f.clock.now = f.clock.now.Add(time.Minute)
			f.clock.mu.Unlock()
			for i := 0; i < 2; i++ {
				outcome, err := f.service.Generate(context.Background(), f.open, f.request)
				if err != nil {
					t.Fatal(err)
				}
				if boundary == "before-seal" {
					if outcome.Failure == nil || outcome.Failure.Code != domain.FailureBoundaryUnknown {
						t.Fatalf("unsealed bytes admitted: %+v", outcome)
					}
				} else if outcome.Value == nil || outcome.Value.RawBlob == nil {
					t.Fatalf("sealed receipt not restored: %+v", outcome)
				}
				if f.httpCalls.Load() != 1 {
					t.Fatalf("recovery sent again: %d", f.httpCalls.Load())
				}
			}
		})
	}
}

func TestLLMReplayProcessHelper(t *testing.T) {
	raw := os.Getenv("CPGEN_LLM_REPLAY_PROCESS_FIXTURE")
	if raw == "" {
		t.Skip("only runs as a crash fixture child")
	}
	var configuration llmReplayProcessConfig
	if err := json.Unmarshal([]byte(raw), &configuration); err != nil {
		t.Fatal(err)
	}
	clock := newRecordingClock(configuration.Now)
	store, err := sqlite.OpenWithClock(context.Background(), sqlite.Config{Path: configuration.Database, BusyTimeout: time.Second, MaxReaders: 4}, clock)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	blobs, err := blob.NewStore(configuration.BlobRoot)
	if err != nil {
		t.Fatal(err)
	}
	var prompts []port.PromptVersion
	if configuration.RepairPolicy != nil {
		prompts = append(prompts, structuredLLMRepairPrompt(configuration.Request.Schema))
	}
	provider, _ := durableTestProvider(t, configuration.Endpoint, prompts...)
	ledger := &crashingLLMReplayLedger{Store: store, boundary: configuration.Boundary, occurrence: configuration.CrashOccurrence}
	service, err := application.NewReplayableLLMCalls(ledger, provider, blobs, clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.RepairPolicy != nil {
		structured, err := application.NewStructuredLLMCalls(service, *configuration.RepairPolicy)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := structured.Generate(context.Background(), configuration.Open, configuration.Request); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := service.Generate(context.Background(), configuration.Open, configuration.Request); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("crash hook was not reached")
}

type crashingLLMReplayLedger struct {
	*sqlite.Store
	boundary   string
	occurrence int
	seen       int
}

func (l *crashingLLMReplayLedger) crash(boundary string) {
	if l.boundary != boundary {
		return
	}
	l.seen++
	if l.occurrence == 0 || l.seen == l.occurrence {
		os.Exit(73)
	}
}

func (l *crashingLLMReplayLedger) SealArtifact(ctx context.Context, id domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	l.crash("before-seal")
	err := l.Store.SealArtifact(ctx, id, ref)
	if err == nil {
		l.crash("sealed")
	}
	return err
}
func (l *crashingLLMReplayLedger) FinalizeArtifact(ctx context.Context, id domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	err := l.Store.FinalizeArtifact(ctx, id, ref)
	if err == nil {
		l.crash("finalized")
	}
	return err
}
func (l *crashingLLMReplayLedger) MarkSent(ctx context.Context, grant domain.DispatchGrant, at time.Time) error {
	err := l.Store.MarkSent(ctx, grant, at)
	if err == nil {
		l.crash("sent")
	}
	return err
}
func (l *crashingLLMReplayLedger) CompletePhysical(ctx context.Context, request domain.CompletePhysicalRequest) error {
	err := l.Store.CompletePhysical(ctx, request)
	if err == nil {
		l.crash("completed")
	}
	return err
}
func (l *crashingLLMReplayLedger) FinishCall(ctx context.Context, request domain.FinishCallRequest) (domain.CallTrace, error) {
	trace, err := l.Store.FinishCall(ctx, request)
	if err == nil {
		l.crash("finished")
	}
	return trace, err
}
