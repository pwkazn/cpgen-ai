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
	"cpgen/internal/runlock"
)

type llmCacheProcessConfig struct {
	Endpoint, Database, BlobRoot, LockRoot, Boundary string
	Open                                             domain.OpenCallRequest
	Request                                          port.GenerateRequest
	Policy                                           application.FormatRepairPolicy
	Now                                              time.Time
}

func TestStructuredLLMCacheProcessCrashKeepsOneReuseIdentity(t *testing.T) {
	for _, boundary := range []string{"before-reuse", "after-reuse"} {
		t.Run(boundary, func(t *testing.T) {
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
			configuration, err := json.Marshal(llmCacheProcessConfig{Endpoint: f.endpoint, Database: f.path, BlobRoot: f.blobRoot, LockRoot: t.TempDir(), Boundary: boundary, Open: open, Request: request, Policy: f.policy, Now: f.clock.Now().Add(time.Second)})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLLMCacheProcessHelper$")
			command.Env = append(os.Environ(), "CPGEN_LLM_CACHE_PROCESS_FIXTURE="+string(configuration))
			output, err := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 73 {
				t.Fatalf("crash helper: %v\n%s", err, output)
			}
			f.clock.mu.Lock()
			f.clock.now = f.clock.now.Add(time.Minute)
			f.clock.mu.Unlock()
			var reuse domain.CacheReuseRecordID
			for i := 0; i < 2; i++ {
				hit, err := cache.Reuse(context.Background(), open, request)
				if err != nil || !hit.Hit || len(hit.Reuses) != 1 || f.httpCalls.Load() != 2 {
					t.Fatalf("recovery=%+v err=%v", hit, err)
				}
				if i == 1 && hit.Reuses[0].CacheReuseRecordID != reuse {
					t.Fatal("restart changed the committed cache use identity")
				}
				reuse = hit.Reuses[0].CacheReuseRecordID
			}
		})
	}
}

func TestLLMCacheProcessHelper(t *testing.T) {
	raw := os.Getenv("CPGEN_LLM_CACHE_PROCESS_FIXTURE")
	if raw == "" {
		t.Skip("only runs as a cache crash fixture child")
	}
	var configuration llmCacheProcessConfig
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
	provider, _ := durableTestProvider(t, configuration.Endpoint, structuredLLMRepairPrompt(configuration.Request.Schema))
	calls, err := application.NewReplayableLLMCalls(store, provider, blobs, clock, 100)
	if err != nil {
		t.Fatal(err)
	}
	structured, err := application.NewStructuredLLMCalls(calls, configuration.Policy)
	if err != nil {
		t.Fatal(err)
	}
	locks, err := runlock.NewManager(configuration.LockRoot, runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer locks.Close()
	cache, err := application.NewStructuredLLMCache(structured, &crashingLLMCacheLedger{Store: store, boundary: configuration.Boundary}, locks)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Reuse(context.Background(), configuration.Open, configuration.Request); err != nil {
		t.Fatal(err)
	}
	t.Fatal("cache crash hook was not reached")
}

type crashingLLMCacheLedger struct {
	*sqlite.Store
	boundary string
}

func (l *crashingLLMCacheLedger) CommitReuseCollection(ctx context.Context, request domain.CommitCacheReuse) ([]domain.PendingCacheReuse, error) {
	if l.boundary == "before-reuse" {
		os.Exit(73)
	}
	result, err := l.Store.CommitReuseCollection(ctx, request)
	if err == nil && l.boundary == "after-reuse" {
		os.Exit(73)
	}
	return result, err
}
