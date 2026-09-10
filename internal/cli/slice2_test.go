package cli_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/cli"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

// Exercise the checked-in examples through the public command parser and
// production Bootstrap. Zero budgets prohibit external dispatch, independently
// of whether the test host has any provider credentials installed.
func TestSlice2ExamplesValidateGenerateAndResumeThroughCLI(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "slice2.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	raw = []byte(strings.Replace(string(raw), "D:/cpgen-private/slice2", filepath.ToSlash(filepath.Join(root, "state")), 1))
	configPath := filepath.Join(root, "cpgen.yaml")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	requestPath, err := filepath.Abs(filepath.Join("..", "..", "config", "slice2-zero-budget.request.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := cli.Run([]string{"--config", configPath, "config", "validate"}, &stdout, &stderr); code != 0 {
		t.Fatalf("validate=%d %s %s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := cli.Run([]string{"--config", configPath, "generate", "--request", requestPath}, &stdout, &stderr); code != 6 {
		t.Fatalf("generate=%d %s %s", code, stdout.String(), stderr.String())
	}
	var result struct {
		Status string             `json:"status"`
		Data   domain.RunSnapshot `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "NEEDS_REVIEW" || result.Data.State != domain.RunNeedsReview || result.Data.CurrentStage != "idea" || result.Data.WorkflowRevision != workflow.Slice2CheckpointWorkflowRevision {
		t.Fatalf("unexpected preview: %+v", result)
	}
	version := result.Data.Version
	stdout.Reset()
	stderr.Reset()
	if code := cli.Run([]string{"--config", configPath, "run", "resume", string(result.Data.RunID)}, &stdout, &stderr); code != 6 {
		t.Fatalf("resume=%d %s %s", code, stdout.String(), stderr.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.Data.Version != version {
		t.Fatalf("review resume changed projection: %+v %v", result, err)
	}
}
