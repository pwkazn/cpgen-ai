package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func qualityVerifierInput(t *testing.T) application.QualityInput {
	t.Helper()
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "solution-blobs"))
	if err != nil {
		t.Fatal(err)
	}
	input := judgeVerifierInput(t, blobs)
	blobs, err = blob.NewStore(filepath.Join(t.TempDir(), "judge-blobs"))
	if err != nil {
		t.Fatal(err)
	}
	_, publisher := judgeTestPublisher(t, blobs)
	sandbox := &judgeSandboxFixture{solutionSandboxFixture: solutionSandboxFixture{publisher: publisher, mode: "pass", t: t}, input: input}
	verifier, err := application.NewJudgeVerifier(application.JudgeVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: blobs, ToolchainLockDigest: input.DataReport.ToolchainLockDigest})
	if err != nil {
		t.Fatal(err)
	}
	result, err := verifier.Verify(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return application.QualityInput{JudgeInput: input, JudgeReport: result.Report}
}

func TestQualityVerifierRequiresCheckerContractAndEveryJudgedCase(t *testing.T) {
	input := qualityVerifierInput(t)
	for _, mode := range []string{"pass", "compile_error", "always_accept", "always_reject", "case_wa", "checker_tle"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "quality-blobs"))
			if err != nil {
				t.Fatal(err)
			}
			f, publisher := judgeTestPublisher(t, blobs)
			sandbox := &qualitySandboxFixture{solutionSandboxFixture: solutionSandboxFixture{publisher: publisher, mode: mode, t: t}}
			verifier, err := application.NewQualityVerifier(application.SolutionVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: blobs, Lock: solutionTestLock(t)})
			if err != nil {
				t.Fatal(err)
			}
			result, err := verifier.Verify(ctx, f.runID, input)
			if err != nil || result.Report.Passed != (mode == "pass") || result.Report.ValidateFor(f.runID, input) != nil {
				t.Fatalf("quality: %+v %v", result.Report, err)
			}
			if mode == "pass" {
				if sandbox.compiles != 1 || sandbox.runs != 2+len(input.JudgeReport.Cases) {
					t.Fatalf("missing checker work: compile=%d run=%d", sandbox.compiles, sandbox.runs)
				}
				assertQualityReportRejectsMissingProof(t, f.runID, input, result.Report)
			} else {
				wanted := map[string]string{"compile_error": "checker.compile.CE", "always_accept": "wa:AC", "always_reject": "ac:WA", "case_wa": string(input.JudgeReport.Cases[0].Input.Path) + ":WA", "checker_tle": "ac:CHECKER_ERROR"}[mode]
				if result.Report.Reason != wanted {
					t.Fatalf("quality failure lost its cause: %s != %s", result.Report.Reason, wanted)
				}
			}
			current, err := f.store.GetRun(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			out := result.ReportArtifact.Blob.Digest
			if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &out, NextStage: "exercise", NextInputDigest: &out, Occurrences: result.Occurrences, IdempotencyKey: coordinatorID("finish", "quality"), At: f.clock.Now()}); err != nil {
				t.Fatalf("quality evidence attachment: %v", err)
			}
		})
	}
}

type qualitySandboxFixture struct{ solutionSandboxFixture }

func (s *qualitySandboxFixture) Run(ctx context.Context, request port.RunRequest) (domain.MeteredOutcome[port.RunResult], error) {
	s.runs++
	if request.Validate() != nil || request.Role != port.RoleChecker || len(request.Files) != 3 || request.Stdin != nil || request.Seed != nil || request.Limits.Time.Milliseconds() != 2000 || request.Limits.MemoryBytes != 256<<20 {
		s.t.Fatal("quality checker lost fixed role, files or limits")
	}
	for i, name := range []domain.SafeRelPath{"input.txt", "output.txt", "answer.txt"} {
		if request.Files[i].Path != name {
			s.t.Fatal("checker file arguments changed")
		}
	}
	code := 0
	if s.runs == 2 {
		code = 1
	}
	if s.mode == "always_accept" {
		code = 0
	}
	if s.mode == "always_reject" || (s.mode == "case_wa" && s.runs > 2) {
		code = 1
	}
	stdout := s.artifact(ctx, domain.ArtifactStdout, fmt.Sprintf("fixture/checker/%d.out", s.runs), nil)
	evidence := s.artifact(ctx, domain.ArtifactEvidence, fmt.Sprintf("fixture/checker/%d.json", s.runs), []byte("checker execution"))
	trace := solutionFixtureTrace(fmt.Sprintf("checker-%d", s.runs), evidence.CallID)
	result := port.RunResult{CallTrace: trace, Outcome: domain.ProcessExited, ExitCode: &code, Stdout: &stdout, Execution: &evidence}
	if s.mode == "checker_tle" {
		result.Outcome, result.ExitCode = domain.ProcessTLE, nil
	}
	return domain.MeteredOutcome[port.RunResult]{Value: &result, CallTrace: trace}, nil
}

func assertQualityReportRejectsMissingProof(t *testing.T, runID domain.RunID, input application.QualityInput, report application.QualityReport) {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*application.QualityReport){
		func(r *application.QualityReport) { r.RunID = "run_000000000000000000000000000000ff" },
		func(r *application.QualityReport) { r.Cases = r.Cases[:len(r.Cases)-1] },
		func(r *application.QualityReport) { r.Canaries = r.Canaries[:1] },
		func(r *application.QualityReport) { r.Canaries[1].Expected = domain.CheckerAC },
		func(r *application.QualityReport) { r.Checker.Source.Digest = domain.SumBytes([]byte("other checker")) },
		func(r *application.QualityReport) { r.Coverage.DifferentialRuns = 0 },
		func(r *application.QualityReport) { r.Cases[0].Answer.Digest = domain.SumBytes([]byte("other answer")) },
		func(r *application.QualityReport) { r.InputDigest = domain.SumBytes([]byte("other Judge")) },
	} {
		var changed application.QualityReport
		if err := json.Unmarshal(raw, &changed); err != nil {
			t.Fatal(err)
		}
		change(&changed)
		if changed.ValidateFor(runID, input) == nil {
			t.Fatal("quality accepted missing or substituted evidence")
		}
	}
	failed := input
	failed.JudgeReport.Passed = false
	if report.ValidateFor(runID, failed) == nil {
		t.Fatal("quality accepted a failed upstream Judge")
	}
}

func assertQualityCommittedReadsRejectSubstitution(t *testing.T, ctx context.Context, f *similarityExecutorFixture, generationConfig application.GenerationExecutorConfig, similarityConfig application.SimilarityExecutorConfig, base application.DockerSandboxConfig, runID domain.RunID) {
	t.Helper()
	for _, path := range []domain.SafeRelPath{"quality/checker/main.cpp", "quality/canaries/wa/output.txt"} {
		changed := generationConfig
		changed.Store = &alteredSolutionStageStore{f.store, func(stage *port.CommittedPrivateStage) {
			if stage.Attempt.StageName != "quality" {
				return
			}
			for i, item := range stage.Artifacts {
				if item.Blob.LogicalPath == path {
					stage.Artifacts = append(stage.Artifacts[:i], stage.Artifacts[i+1:]...)
					return
				}
			}
		}}
		generation, err := application.NewGenerationExecutor(changed)
		if err != nil {
			t.Fatal(err)
		}
		similarityConfig.Generation = generation
		similarity, err := application.NewSimilarityExecutor(similarityConfig)
		if err != nil {
			t.Fatal(err)
		}
		solution, err := application.NewSolutionExecutor(similarity, generation)
		if err != nil {
			t.Fatal(err)
		}
		data, err := application.NewDataExecutor(solution, base)
		if err != nil {
			t.Fatal(err)
		}
		quality, err := application.NewQualityExecutor(data)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := quality.Reader().ReadReport(ctx, runID); err == nil {
			t.Fatalf("quality accepted a missing committed artifact: %s", path)
		}
	}
}
