package application_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpgen/internal/agent"
	"cpgen/internal/application"
	"cpgen/internal/cli"
	"cpgen/internal/config"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

func llmApplicationConfig(t *testing.T) config.Config {
	t.Helper()
	cfg, err := config.Decode([]byte("storage:\n  state_root: " + filepath.Join(t.TempDir(), "state") + "\nllm:\n  base_url: https://api.deepseek.com/v1\n  model: deepseek-v4-flash\n  api_key_env: CPGEN_LLM_CONFIG_TEST_KEY\n  timeout: 45s\n  max_output_tokens: 8192\n  max_response_bytes: 2097152\n"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestBuildLLMConfigSingleAttemptAndExplicitOutputLimits(t *testing.T) {
	t.Setenv("CPGEN_LLM_CONFIG_TEST_KEY", "")
	cfg := llmApplicationConfig(t)
	mapped, output, err := application.BuildLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if mapped.Endpoint != cfg.LLM.BaseURL || mapped.Model != cfg.LLM.Model || mapped.APIKeyEnv != cfg.LLM.APIKeyEnv || mapped.Timeout != 45*time.Second || mapped.MaxResponseBytes != 2097152 {
		t.Fatalf("provider fields lost: %+v", mapped)
	}
	if mapped.MaxAttempts != 1 || output != (port.OutputLimit{Tokens: 8192, Bytes: 2097152}) {
		t.Fatalf("attempts=%d output=%+v", mapped.MaxAttempts, output)
	}
	if mapped.AllowInsecureHTTP || mapped.AllowLoopbackForTesting || mapped.HTTPClient != nil || mapped.PromptResolver != nil || len(mapped.SchemaValidators) != 0 {
		t.Fatal("mapping enabled test policy or unbound schema/prompt callbacks")
	}
	if err := mapped.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.NewLangChain(mapped); err != nil {
		t.Fatalf("cannot construct adapter without credentials: %v", err)
	}
	for _, bad := range []config.Config{{}, {LLM: &config.LLMConfig{}}} {
		got, limits, err := application.BuildLLMConfig(bad)
		var fieldErr *config.FieldError
		if !errors.As(err, &fieldErr) || got.MaxAttempts != 0 || limits != (port.OutputLimit{}) {
			t.Fatalf("invalid/absent provider returned usable mapping: %+v %+v %v", got, limits, err)
		}
	}
}

func TestBuildFormatRepairPolicyUsesCompiledStagePrompts(t *testing.T) {
	cfg := llmApplicationConfig(t)
	cfg.LLM.MaxFormatRepairs = 1
	mapped, _, err := application.BuildLLMConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []string{"idea", "statement", "idea.draft", "statement.draft", "idea.mutate", "solution.draft", "data.draft"} {
		policy, err := application.BuildFormatRepairPolicy(cfg, step)
		if err != nil || policy.MaxRepairs != 1 || policy.Prompt.Step != step+".format-repair" || policy.Prompt.InputSchemaVersion != "cpgen.format-repair-input/v1" {
			t.Fatalf("policy=%+v err=%v", policy, err)
		}
		if _, err := mapped.PromptRegistry.Resolve(policy.Prompt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := application.BuildFormatRepairPolicy(cfg, "uncompiled"); err == nil {
		t.Fatal("uncompiled repair step admitted")
	}
	cfg.LLM.MaxFormatRepairs = 2
	if _, err := application.BuildFormatRepairPolicy(cfg, "idea"); err == nil {
		t.Fatal("unbounded repair configuration admitted")
	}
}

func TestBuildLLMConfigBindsBuiltinPromptAndSchemaRegistries(t *testing.T) {
	mapped, _, err := application.BuildLLMConfig(llmApplicationConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if mapped.PromptRegistry == nil || mapped.SchemaRegistry == nil {
		t.Fatal("missing trusted registry")
	}
	fixtures := llmBuiltinOutputs(t)
	versions := mapped.PromptRegistry.Versions()
	if len(versions) < len(fixtures) {
		t.Fatalf("builtin versions = %v", versions)
	}
	for _, ref := range versions {
		definition, err := mapped.PromptRegistry.Lookup(ref)
		if err != nil || definition.TemplateDigest != domain.SumBytes([]byte(definition.Template)) {
			t.Fatalf("unbound prompt: %v", err)
		}
		raw, ok := fixtures[strings.TrimSuffix(ref.Step, ".format-repair")]
		if !ok {
			t.Fatalf("unexpected builtin: %s", ref.Step)
		}
		if ref.Version == "v1" && ref.InputSchemaVersion != domain.ProgramContextSchema {
			if err := mapped.SchemaRegistry.Validate(raw, ref.OutputSchema, 1<<20); err != nil {
				t.Fatalf("%s builtin rejected valid domain output: %v", ref.Step, err)
			}
			for _, invalid := range [][]byte{
				append([]byte(`{"unknown":true,`), raw[1:]...),
				append([]byte(`{"schema_version":"duplicate",`), raw[1:]...),
				[]byte(`{"schema_version":"` + string(ref.OutputSchema.SchemaVersion) + `"}`),
			} {
				if err := mapped.SchemaRegistry.Validate(invalid, ref.OutputSchema, 1<<20); err == nil {
					t.Fatalf("%s accepted invalid output", ref.Step)
				}
			}
			wrong := ref.OutputSchema
			wrong.Digest = domain.SumBytes([]byte("untrusted schema"))
			if err := mapped.SchemaRegistry.Validate(raw, wrong, 1<<20); err == nil {
				t.Fatal("schema digest substitution accepted")
			}
			wrongPrompt := ref
			wrongPrompt.TemplateDigest = domain.SumBytes([]byte("untrusted prompt"))
			if _, err := mapped.PromptRegistry.Lookup(wrongPrompt); err == nil {
				t.Fatal("prompt digest substitution accepted")
			}
		}
	}
	second, _, err := application.BuildLLMConfig(llmApplicationConfig(t))
	if err != nil || second.PromptRegistry == mapped.PromptRegistry || second.SchemaRegistry == mapped.SchemaRegistry {
		t.Fatalf("mapping shares mutable registry state: %v", err)
	}
}

func TestDraftPromptCompatibilityAndV3Selection(t *testing.T) {
	checks := []struct {
		stage, v1, v2 string
	}{
		{"idea", "d9bf5b2536abdf5372cb8f7e826592e04716af9000a27ef4ccd5b02b808064cd", "d9bf5b2536abdf5372cb8f7e826592e04716af9000a27ef4ccd5b02b808064cd"},
		{"statement", "6af84deab4ea89f48d7c268bae84a3fa6929d1c1f0bba4f568f45fad659b85c6", "db41a402cf9565b43bd358e7e7759a795e78614d7124d8ce6a4fcdf52c1d7416"},
		{"solution", "9cfe865befce99012b59949f1fed11c072b47e922f9d2150de43930682fbd2c3", "be9975eaac508416f90a4345643b7c4a0cb8fb15626bb20fe5ed9aea19d25576"},
		{"data", "2a40a633e364c5aeb4365becd3c6d264f0db88e36589fcf3444ba0ea3a54e695", "e8be5799625ff021e94dae053a0160b0c5ab2104fe48b94eb3785955200a10ee"},
	}
	for _, tc := range checks {
		old, _, err := application.BuildLLMDraftPromptForWorkflow(tc.stage, workflow.GenerationRevision)
		if err != nil || string(old.TemplateDigest) != "sha256:"+tc.v1 {
			t.Fatalf("%s v1 prompt=%v err=%v", tc.stage, old.TemplateDigest, err)
		}
		v3, _, err := application.BuildLLMDraftPromptForWorkflow(tc.stage, workflow.ExecutedSamplesRevision)
		if err != nil || string(v3.TemplateDigest) != "sha256:"+tc.v2 {
			t.Fatalf("%s v3 prompt=%v err=%v", tc.stage, v3.TemplateDigest, err)
		}
	}
}

func llmBuiltinOutputs(t *testing.T) map[string][]byte {
	t.Helper()
	snapshot, err := domain.NewGenerationRequestSnapshotV1(domain.GenerationRequestV1{
		SchemaVersion: domain.RequestSchemaV1, Mode: "manual", Brief: "Graphs", Language: "en", Difficulty: "hard",
		TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", VerificationProfile: "default",
		BudgetLimits: domain.BudgetLimits{MaxLLMCalls: 4},
	}, 42)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := domain.NewIdeaBatch(snapshot, 2, domain.GenerationPolicyV1, []domain.IdeaCandidate{
		{AbstractTask: "Find distances", IntendedAlgorithm: "bfs", TargetComplexity: "O(n+m)", FeasibilityStatus: "FEASIBLE"},
		{CandidateOrdinal: 1, AbstractTask: "Find components", IntendedAlgorithm: "dfs", TargetComplexity: "O(n+m)", FeasibilityStatus: "FEASIBLE"},
	})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := domain.NewIdeaSelection(snapshot.RequestDigest, batch, batch.Candidates[0].IdeaID, domain.SelectionOrdinalPolicyV1, []string{"stable"}, []domain.Digest{batch.BatchDigest})
	if err != nil {
		t.Fatal(err)
	}
	input := domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: snapshot.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}
	problem, err := domain.NewProblemSpec(input, snapshot, batch, selection, domain.ProblemSpec{
		Revision: 1, Title: "Distances", Description: "Find distances.",
		Input: domain.ProblemIO{Description: "Graph", Fields: []string{"n", "edges"}}, Output: domain.ProblemIO{Description: "Distances", Fields: []string{"distances"}},
		Samples: []domain.ProblemSample{{Input: "1 0", Output: "0"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[string][]byte)
	ideaDraft := domain.IdeaDraftV1{SchemaVersion: domain.IdeaDraftSchemaV1, Candidates: []domain.IdeaContentDraft{
		{AbstractTask: "Find distances", IntendedAlgorithm: "bfs", TargetComplexity: "O(n+m)", FeasibilityStatus: "FEASIBLE", FeasibilityReasons: []string{}},
		{AbstractTask: "Find components", IntendedAlgorithm: "dfs", TargetComplexity: "O(n+m)", FeasibilityStatus: "FEASIBLE", FeasibilityReasons: []string{}},
	}}
	statementDraft := domain.StatementDraftV1{SchemaVersion: domain.StatementDraftSchemaV1, Title: problem.Title, Description: problem.Description, Input: problem.Input, Output: problem.Output, Samples: problem.Samples}
	solutionDraft := domain.SolutionDraftV1{SchemaVersion: domain.SolutionDraftSchemaV1, ReferenceCode: "int main() { return 0; }\n", BruteCode: "int main() {}\n", Explanation: "Use BFS for shortest paths in the unweighted graph."}
	dataDraft := domain.DataDraftV1{SchemaVersion: domain.DataDraftSchemaV1, GeneratorCode: "int main() { return 0; }\n", ValidatorCode: "int main() { return 3; }\n", Cases: []domain.DataCaseDraft{{Kind: domain.DataCaseSmall, Purpose: "Minimal graph"}, {Kind: domain.DataCaseSmall, Purpose: "Small random graph"}, {Kind: domain.DataCaseBoundary, Purpose: "Disconnected graph"}, {Kind: domain.DataCaseStress, Purpose: "Maximum graph"}}}
	for name, value := range map[string]any{"idea": batch, "statement": problem, "idea.draft": ideaDraft, "statement.draft": statementDraft, "idea.mutate": ideaDraft, "solution.draft": solutionDraft, "data.draft": dataDraft} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = raw
	}
	return out
}

func TestLLMConfigurationDoesNotEnableLiveCLI(t *testing.T) {
	t.Setenv("CPGEN_LLM_CONFIG_TEST_KEY", "")
	root := t.TempDir()
	configPath := filepath.Join(root, "config.yaml")
	configYAML := "storage:\n  state_root: " + filepath.Join(root, "state") + "\nfake_workflow:\n  scenario: review\nllm:\n  base_url: https://provider.invalid\n  model: fixture\n  api_key_env: CPGEN_LLM_CONFIG_TEST_KEY\n"
	if err := os.WriteFile(configPath, []byte(configYAML), 0600); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(root, "request.yaml")
	request := "schema_version: cpgen.request/v1\nmode: offline\nbrief: demo\nlanguage: cpp\ndifficulty: easy\ntime_limit_milliseconds: 1000\nmemory_limit_megabytes: 64\nsolution_language: go\nverification_profile: default\nbudget_limits:\n  max_active_time_milliseconds: 5000\n"
	if err := os.WriteFile(requestPath, []byte(request), 0600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{"--config", configPath, "generate", "--request", requestPath}, &stdout, &stderr)
	if code != 6 || !strings.Contains(stdout.String(), "NEEDS_REVIEW") || stderr.Len() != 0 {
		t.Fatalf("Fake review workflow changed: code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
	}
}
