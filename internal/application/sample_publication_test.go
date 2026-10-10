package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestJudgeSamplePublicationFailuresProduceContentVerdicts(t *testing.T) {
	for _, test := range []struct {
		name    string
		output  func(string) string
		failure application.SamplePublicationFailure
	}{
		{"normal", func(output string) string { return output }, ""},
		{"64KiB_boundary", func(output string) string { return output + strings.Repeat(" ", (1<<16)-len(output)) }, ""},
		{"CRLF", func(output string) string { return output + "\r\n" }, application.SamplePublicationInvalidText},
		{"NUL", func(output string) string { return output + "\x00" }, application.SamplePublicationInvalidText},
		{"invalid_UTF8", func(output string) string { return output + "\xff" }, application.SamplePublicationInvalidText},
		{"over_64KiB", func(output string) string { return output + strings.Repeat(" ", (1<<16)+1-len(output)) }, application.SamplePublicationTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
			if err != nil {
				t.Fatal(err)
			}
			upstreamBlobs, err := blob.NewStore(filepath.Join(t.TempDir(), "upstream-blobs"))
			if err != nil {
				t.Fatal(err)
			}
			input := judgeVerifierInputWithSampleOutput(t, upstreamBlobs, test.output, workflow.ExecutedSamplesRevision)
			f, publisher := judgeTestPublisher(t, blobs)
			sandbox := &judgeSandboxFixture{solutionSandboxFixture: solutionSandboxFixture{publisher: publisher, mode: "pass", t: t}, input: input, sampleOutput: test.output}
			verifier, err := application.NewJudgeVerifier(application.JudgeVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: blobs, ToolchainLockDigest: input.DataReport.ToolchainLockDigest, WorkflowRevision: workflow.ExecutedSamplesRevision})
			if err != nil {
				t.Fatal(err)
			}
			result, err := verifier.Verify(ctx, input)
			if err != nil {
				t.Fatalf("sample content escaped as an execution error: %v", err)
			}
			if err := result.Report.ValidateFor(input); err != nil {
				t.Fatal(err)
			}
			rawReport, err := json.Marshal(result.Report)
			if err != nil {
				t.Fatal(err)
			}
			var restored application.JudgeVerificationReport
			if err := json.Unmarshal(rawReport, &restored); err != nil || restored.ValidateFor(input) != nil {
				t.Fatalf("publication verdict did not survive report replay: %v", err)
			}
			check := result.Report.Cases[0]
			if check.SamplePublicationFailure != test.failure || result.Report.Passed != (test.failure == "") {
				t.Fatalf("unexpected verdict: %+v", result.Report)
			}
			if test.failure == "" {
				if result.DatasetArtifact == nil || result.FinalStatementArtifact == nil || check.Answer == nil {
					t.Fatal("valid sample omitted publication")
				}
				final, err := application.FinalizeSamples(ctx, blobs, input, result.Report)
				if err != nil || final.Samples[0].Output != test.output(input.DataInput.SolutionInput.Problem.Samples[0].Output) {
					t.Fatalf("sample finalization changed the executed bytes: %v", err)
				}
				if strings.Contains(string(rawReport), "sample_publication_failure") {
					t.Fatal("passing report changed its canonical field set")
				}
			} else {
				if len(result.Report.Cases) != 1 || check.Answer != nil || result.DatasetArtifact != nil || result.FinalStatementArtifact != nil {
					t.Fatal("rejected sample published an answer or continued")
				}
				reason := "judge_requires_review:" + result.Report.Reason
				if target := workflow.ContentRetryTarget(workflow.ExecutedSamplesRevision, "quality", reason); target != "solution" {
					t.Fatalf("failure lost content retry route: %s -> %s", reason, target)
				}
				if _, err := application.FinalizeSamples(ctx, blobs, input, result.Report); err == nil {
					t.Fatal("failed sample was finalized")
				}
			}
			// Content failures must retain the same complete, attachable proof as a
			// successful Judge stage so the next stage can select retry or review.
			current, err := f.store.GetRun(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			out := result.ReportArtifact.Blob.Digest
			if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &out, NextStage: "exercise", NextInputDigest: &out, Occurrences: result.Occurrences, IdempotencyKey: coordinatorID("finish", "sample-publication"), At: f.clock.Now()}); err != nil {
				t.Fatalf("failed to commit Judge proof: %v", err)
			}
		})
	}
}

func TestJudgeSamplePublicationPreservesLegacyAndExecutionErrors(t *testing.T) {
	executionError := errors.New("sandbox transport unavailable")
	for _, test := range []struct {
		name, revision string
		runError       error
	}{
		{"legacy_V1", workflow.GenerationRevision, nil},
		{"legacy_V2", workflow.RetryingGenerationRevision, nil},
		{"transport", workflow.ExecutedSamplesRevision, executionError},
	} {
		t.Run(test.name, func(t *testing.T) {
			blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
			if err != nil {
				t.Fatal(err)
			}
			upstreamBlobs, err := blob.NewStore(filepath.Join(t.TempDir(), "upstream-blobs"))
			if err != nil {
				t.Fatal(err)
			}
			input := judgeVerifierInput(t, upstreamBlobs, test.revision)
			_, publisher := judgeTestPublisher(t, blobs)
			sandbox := &judgeSandboxFixture{solutionSandboxFixture: solutionSandboxFixture{publisher: publisher, mode: "pass", t: t}, input: input, sampleOutput: func(output string) string { return output + "\r\n" }, runError: test.runError}
			verifier, err := application.NewJudgeVerifier(application.JudgeVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: blobs, ToolchainLockDigest: input.DataReport.ToolchainLockDigest, WorkflowRevision: test.revision})
			if err != nil {
				t.Fatal(err)
			}
			result, err := verifier.Verify(context.Background(), input)
			if test.runError != nil {
				if !errors.Is(err, test.runError) || result.ReportArtifact.Blob.Digest != "" {
					t.Fatalf("execution error became content: %+v, %v", result, err)
				}
			} else if err != nil || !result.Report.Passed || result.Report.Cases[0].SamplePublicationFailure != "" || result.FinalStatementArtifact != nil {
				t.Fatalf("legacy output contract changed: %+v, %v", result.Report, err)
			}
		})
	}
}
