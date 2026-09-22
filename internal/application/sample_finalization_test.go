package application_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

const connectivityRegressionInput = "5 7\n1 2\n2 3\n1 3\n4 4\n3 4\n2 4\n5 5\n"

func TestProgramContextExcludesDraftAnswersAndExplanations(t *testing.T) {
	input, solution := solutionVerifierContentWithSample(t, &domain.ProblemSample{Input: connectivityRegressionInput, Output: "1101000", Explanation: "STALE_SAMPLE_EXPLANATION"})
	data, err := domain.NewDataDraftInput(input, solution, domain.SumBytes([]byte("proof")))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []interface {
		ProgramContextJSON() ([]byte, error)
		Digest() (domain.Digest, error)
	}{input, data} {
		raw, err := value.ProgramContextJSON()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte("1101000")) || bytes.Contains(raw, []byte("STALE_SAMPLE_EXPLANATION")) {
			t.Fatal("unverified answer leaked into program semantics")
		}
		var decoded struct {
			Source  domain.Digest   `json:"source_input_digest"`
			Context json.RawMessage `json:"context"`
		}
		expected, err := value.Digest()
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Source != expected || !bytes.Contains(decoded.Context, []byte(`5 7\n1 2`)) {
			t.Fatal("program context lost input binding")
		}
	}
}

func TestFinalizedSamplesRequireJudgeAndReplaceDraftExplanation(t *testing.T) {
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	input := judgeVerifierInput(t, blobs, workflow.ExecutedSamplesRevision)
	_, publisher := judgeTestPublisher(t, blobs)
	runner := &judgeSandboxFixture{solutionSandboxFixture: solutionSandboxFixture{publisher: publisher, mode: "pass", t: t}, input: input}
	verifier, err := application.NewJudgeVerifier(application.JudgeVerifierConfig{Sandbox: runner, Publisher: publisher, Blobs: blobs, ToolchainLockDigest: input.DataReport.ToolchainLockDigest, WorkflowRevision: workflow.ExecutedSamplesRevision})
	if err != nil {
		t.Fatal(err)
	}
	result, err := verifier.Verify(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if result.FinalStatementArtifact == nil {
		t.Fatal("passing Judge omitted formal samples")
	}
	final, err := application.FinalizeSamples(context.Background(), blobs, input, result.Report)
	if err != nil {
		t.Fatal(err)
	}
	for i, sample := range final.Samples {
		if sample.Input != input.DataInput.SolutionInput.Problem.Samples[i].Input || !strings.Contains(final.StatementMarkdown, sample.Explanation) || sample.Explanation == input.DataInput.SolutionInput.Problem.Samples[i].Explanation {
			t.Fatal("final explanation is absent or reused draft prose")
		}
	}
	result.Report.Cases[0].Brute = nil
	if _, err := application.FinalizeSamples(context.Background(), blobs, input, result.Report); err == nil {
		t.Fatal("reference-only answer became a formal sample")
	}
}
