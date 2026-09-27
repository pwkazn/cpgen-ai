package application

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"cpgen/internal/config"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

// Exercise each actual request reconstruction path up to its planning boundary.
// The spy stops before any ledger mutation, receipt read or physical dispatch.
func TestDataPromptVersionUsesFrozenSelectionForGenerationReaderAndReconcile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "mvp.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// Git may check the example out with CRLF on Windows. Normalize it before
	// editing individual YAML lines for the historical workflow fixtures.
	raw = []byte(strings.ReplaceAll(string(raw), "\r\n", "\n"))
	// The example ships Windows absolute paths, but the loader requires both
	// state_root and toolchain_lock_path to be absolute, and D:/ is not
	// absolute on Linux or macOS. Move every path onto this host, not just the
	// first one, or config.Decode rejects the example off Windows.
	root := filepath.ToSlash(t.TempDir())
	raw = []byte(strings.ReplaceAll(string(raw), "D:/cpgen-private", root))
	for _, tc := range []struct{ name, revision, selected, want string }{
		{"legacy-v1", workflow.GenerationRevision, "", "v1"},
		{"legacy-v2", workflow.RetryingGenerationRevision, "", "v1"},
		{"existing-v3", workflow.ExecutedSamplesRevision, "", "v2"},
		{"explicit-v3", workflow.ExecutedSamplesRevision, "v3", "v3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := strings.Replace(string(raw), workflow.ExecutedSamplesRevision, tc.revision, 1)
			input = strings.Replace(input, "  data_prompt_version: v3\n", "", 1)
			cfg, err := config.Decode([]byte(input))
			if err != nil {
				t.Fatal(err)
			}
			cfg.LLM.DataPromptVersion = tc.selected
			cfg.LLM.MaxFormatRepairs = 1
			frozenBytes, err := cfg.Effective()
			if err != nil {
				t.Fatal(err)
			}
			frozen, err := config.DecodeEffective(frozenBytes)
			if err != nil {
				t.Fatal(err)
			}
			content, _, err := BuildGenerationExecutionSettings(frozen)
			if err != nil || content.DataPromptVersion != tc.selected {
				t.Fatalf("frozen selection missing from execution policy: %+v %v", content, err)
			}
			prompt, schema, err := BuildLLMDraftPromptForConfig("data", frozen)
			if err != nil || prompt.Version != tc.want {
				t.Fatalf("wrong frozen prompt: %+v %v", prompt, err)
			}
			repair, err := BuildFormatRepairPolicy(frozen, "data.draft")
			if err != nil || repair.Prompt.Version != tc.want || repair.Prompt.SchemaDigest != schema.Digest || repair.MaxRepairs != 1 {
				t.Fatalf("format repair selection differs: %+v %v", repair, err)
			}
			registry, _, err := builtinLLMRegistries()
			if err != nil {
				t.Fatal(err)
			}
			definition, err := registry.Resolve(prompt)
			if err != nil {
				t.Fatal(err)
			}
			repairDefinition, err := registry.Resolve(repair.Prompt)
			if err != nil || repairDefinition.Template != definition.Template+builtinFormatRepairInstruction {
				t.Fatalf("repair lost selected instruction: %v", err)
			}
			if tc.selected == "v3" {
				if !strings.HasPrefix(definition.Template, builtinDataDraftPrompt) {
					t.Fatal("V3 replaced the historical data contract")
				}
				for _, required := range []string{`argc=4`, `argv[1]="--seed=42"`, `argv[2]="--case=1"`, `argv[3]="--kind=small"`, `remove_prefix(7)`, `std::uint64_t`, `std::from_chars`, `18446744073709551615`} {
					if !strings.Contains(definition.Template, required) {
						t.Fatalf("V3 lacks argument clarification %q", required)
					}
				}
			}
			// Changing the caller's live config cannot change reconstructed
			// settings that came from the persisted effective bytes.
			cfg.LLM.DataPromptVersion = "changed-after-freezing"
			spy := &dataPromptPlanSpy{}
			drafts := &DraftExecution{config: GenerationExecutorConfig{LLM: spy, Content: content}}
			at := time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC)
			digest := domain.SumBytes([]byte("data prompt selection fixture"))
			attempt := domain.StageAttempt{RunID: "run_00000000000000000000000000000031", AttemptID: "attempt_00000000000000000000000000000031", StageName: "data", Ordinal: 1, State: domain.StageAttemptSucceeded, InputDigest: digest, OutputDigest: &digest, StartedAt: at, FinishedAt: &at}
			variables := []byte(`{"frozen":"input"}`)
			_, generated, err := drafts.draftCall(attempt.RunID, 1, attempt, variables)
			if !errors.Is(err, errDataPromptPlanBoundary) || generated.Prompt != prompt || generated.Schema != schema {
				t.Fatalf("generation selected another prompt: %+v %v", generated.Prompt, err)
			}
			current := domain.RunSnapshot{RunID: attempt.RunID, Version: 1}
			if err := drafts.reconcileDraftRequest(context.Background(), current, attempt, variables); !errors.Is(err, errDataPromptPlanBoundary) {
				t.Fatalf("reconcile did not reach identical planning boundary: %v", err)
			}
			call := domain.CallRecord{ID: "callrec_00000000000000000000000000000031", RunID: attempt.RunID, StageName: "data", AttemptID: attempt.AttemptID, Kind: domain.CallLLMGenerate, State: domain.CallRecordTerminal, LogicalOperationID: generated.LogicalIdempotencyKey}
			store := dataPromptReadStore{stage: port.CommittedLLMStage{StageVersion: 1, Attempt: attempt, Artifacts: []port.CommittedLLMStageArtifact{{ProviderCallRecordID: call.ID}}}, call: call}
			reader := &GenerationReader{store: store, options: content}
			if _, _, err := reader.readDraft(context.Background(), attempt.RunID, "data", digest, variables, spy); !errors.Is(err, errDataPromptPlanBoundary) {
				t.Fatalf("committed reader did not reach planning boundary: %v", err)
			}
			if len(spy.requests) != 3 {
				t.Fatalf("planning calls=%d, want generation, reconcile, reader", len(spy.requests))
			}
			for _, request := range spy.requests {
				if !reflect.DeepEqual(request, generated) {
					t.Fatalf("frozen request reconstruction drifted: %+v != %+v", request, generated)
				}
			}
		})
	}
}

var errDataPromptPlanBoundary = errors.New("stop at data prompt plan boundary")

type dataPromptPlanSpy struct {
	port.PhysicalLLM
	durable.CommittedDraftReader
	requests []port.GenerateRequest
}

func (s *dataPromptPlanSpy) PlanGenerate(request port.GenerateRequest) (port.LLMRequestPlan, error) {
	s.requests = append(s.requests, request)
	return port.LLMRequestPlan{}, errDataPromptPlanBoundary
}

type dataPromptReadStore struct {
	GenerationReadStore
	stage port.CommittedLLMStage
	call  domain.CallRecord
}

func (s dataPromptReadStore) ReadCommittedLLMStage(context.Context, domain.RunID, domain.StageName) (port.CommittedLLMStage, error) {
	return s.stage, nil
}

func (s dataPromptReadStore) ReadLogicalCall(context.Context, domain.CallRecordID) (domain.CallRecord, error) {
	return s.call, nil
}

func (s dataPromptReadStore) ReadAttemptLLMCalls(context.Context, domain.RunID, domain.StageName, domain.AttemptID) ([]domain.CallRecord, error) {
	return []domain.CallRecord{s.call}, nil
}
