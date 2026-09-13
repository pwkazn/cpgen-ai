package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestStructuredLLMProcessCrashRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, boundary string
		occurrence     int
		unknown        bool
	}{
		{"validation-sealed", "sealed", 1, false},
		{"between-calls", "finished", 1, false},
		{"repair-unsealed", "before-seal", 2, true},
		{"repair-sealed", "sealed", 2, false},
		{"repair-completed", "completed", 2, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStructuredLLMFixture(t, false)
			configuration, err := json.Marshal(llmReplayProcessConfig{Endpoint: f.endpoint, Database: f.path, BlobRoot: f.blobRoot, Boundary: tc.boundary, Open: f.open, Request: f.request, Now: f.clock.Now().Add(time.Second), RepairPolicy: &f.policy, CrashOccurrence: tc.occurrence})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLLMReplayProcessHelper$")
			command.Env = append(os.Environ(), "CPGEN_LLM_REPLAY_PROCESS_FIXTURE="+string(configuration))
			output, err := command.CombinedOutput()
			var exited *exec.ExitError
			if !errors.As(err, &exited) || exited.ExitCode() != 73 {
				t.Fatalf("crash helper: %v\n%s", err, output)
			}
			f.clock.mu.Lock()
			f.clock.now = f.clock.now.Add(time.Minute)
			f.clock.mu.Unlock()
			for i := 0; i < 2; i++ {
				result, err := f.structured.Generate(context.Background(), f.open, f.request)
				if err != nil || len(result.CallTraces) != 2 || f.httpCalls.Load() != 2 {
					t.Fatalf("recovery=%+v err=%v calls=%d", result, err, f.httpCalls.Load())
				}
				if tc.unknown {
					if result.Outcome.Failure == nil || result.Outcome.Failure.Code != domain.FailureBoundaryUnknown || len(result.Artifacts) != 1 {
						t.Fatalf("unsealed repair was admitted: %+v", result)
					}
				} else if result.Outcome.Value == nil || len(result.Artifacts) != 2 || result.Usage.InputTokens != 6 || result.Usage.OutputTokens != 8 {
					t.Fatalf("sealed repair was not recovered: %+v", result)
				}
			}
		})
	}
}
