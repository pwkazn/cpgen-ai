package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func judgeTestPublisher(t *testing.T, blobs *blob.Store) (coordinatorFixture, *sandboxexec.ArtifactSink) {
	t.Helper()
	f := newCoordinatorFixtureWithStages(t, "dc", domain.BudgetLimits{MaxArtifactBytes: 32 << 20, MaxActiveTimeMilliseconds: 100000}, []domain.StageName{"prepare", "exercise"})
	operation, err := domain.NewID("call")
	if err != nil {
		t.Fatal(err)
	}
	identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000dc", LogicalOperationID: operation, Kind: domain.CallSandboxRun, ScopeDigest: domain.SumBytes([]byte("test input")), ExpectedRunVersion: 2}
	publisher, err := sandboxexec.NewArtifactSink(f.store, blobs, f.clock, identity)
	if err != nil {
		t.Fatal(err)
	}
	return f, publisher
}

func judgeVerifierInput(t *testing.T, blobs *blob.Store) application.JudgeInput {
	t.Helper()
	ctx := context.Background()
	input, solution := solutionVerifierContent(t)
	_, publisher := judgeTestPublisher(t, blobs)
	sandbox := &solutionSandboxFixture{publisher: publisher, mode: "pass", expected: input.Problem.Samples[0].Output, t: t}
	verifier, err := application.NewSolutionVerifier(application.SolutionVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: blobs, Lock: solutionTestLock(t)})
	if err != nil {
		t.Fatal(err)
	}
	solved, err := verifier.Verify(ctx, input, solution)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := domain.NewDataDraftInput(input, solution, solved.ReportArtifact.Blob.Digest)
	if err != nil {
		t.Fatal(err)
	}
	var draft domain.DataDraftV1
	if err := json.Unmarshal(llmBuiltinOutputs(t)["data.draft"], &draft); err != nil {
		t.Fatal(err)
	}
	content, err := draft.Bind(bound)
	if err != nil {
		t.Fatal(err)
	}
	dataBlobs, err := blob.NewStore(filepath.Join(t.TempDir(), "data-blobs"))
	if err != nil {
		t.Fatal(err)
	}
	_, publisher = judgeTestPublisher(t, dataBlobs)
	main := &dataSandboxFixture{solutionSandboxFixture: solutionSandboxFixture{publisher: publisher, mode: "pass", t: t}, name: "main", samples: len(input.Problem.Samples)}
	repeat := &dataSandboxFixture{solutionSandboxFixture: solutionSandboxFixture{publisher: publisher, mode: "pass", t: t}, name: "repeat", original: main}
	dataVerifier, err := application.NewDataVerifier(application.DataVerifierConfig{Sandbox: main, RepeatSandbox: repeat, Publisher: publisher, Blobs: dataBlobs, Lock: solutionTestLock(t)})
	if err != nil {
		t.Fatal(err)
	}
	data, err := dataVerifier.Verify(ctx, bound, content)
	if err != nil {
		t.Fatal(err)
	}
	return application.JudgeInput{DataInput: bound, Data: content, DataReport: data.Report, SolutionReport: solved.Report}
}

func TestJudgeVerifierRequiresAllAnswersAndIndependentSmallChecks(t *testing.T) {
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	input := judgeVerifierInput(t, blobs)
	for _, mode := range []string{"pass", "sample_wa", "differential_wa", "reference_tle", "brute_mle", "reference_ole", "shared_execution"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			// Each independent SQLite fixture owns its own physical blob store.
			// The fake runner consumes upstream identities, not executable bytes.
			blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
			if err != nil {
				t.Fatal(err)
			}
			f, publisher := judgeTestPublisher(t, blobs)
			sandbox := &judgeSandboxFixture{solutionSandboxFixture: solutionSandboxFixture{publisher: publisher, mode: mode, t: t}, input: input}
			verifier, err := application.NewJudgeVerifier(application.JudgeVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: blobs, ToolchainLockDigest: input.DataReport.ToolchainLockDigest})
			if err != nil {
				t.Fatal(err)
			}
			result, err := verifier.Verify(ctx, input)
			if mode == "shared_execution" {
				if err == nil {
					t.Fatal("reused reference execution counted as Brute")
				}
				return
			}
			if err != nil || result.Report.ValidateFor(input) != nil || result.Report.Passed != (mode == "pass") {
				t.Fatalf("Judge verification: %+v %v", result.Report, err)
			}
			if (result.DatasetArtifact != nil) != result.Report.Passed {
				t.Fatal("failed Judge produced a judged dataset")
			}
			if mode == "pass" {
				if sandbox.compiles != 0 || sandbox.runs != len(input.DataReport.Samples)+len(input.Data.Plan.Cases)+2 {
					t.Fatalf("Judge dispatch count: compiles=%d runs=%d", sandbox.compiles, sandbox.runs)
				}
				assertJudgeReportRejectsMissingProof(t, input, result.Report)
			}
			current, err := f.store.GetRun(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			out := result.ReportArtifact.Blob.Digest
			if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &out, NextStage: "exercise", NextInputDigest: &out, Occurrences: result.Occurrences, IdempotencyKey: coordinatorID("finish", "Judge"), At: f.clock.Now()}); err != nil {
				t.Fatalf("Judge evidence attachment: %v", err)
			}
		})
	}
}

type judgeSandboxFixture struct {
	solutionSandboxFixture
	input     application.JudgeInput
	lastTrace domain.CallTrace
}

func (s *judgeSandboxFixture) Run(ctx context.Context, request port.RunRequest) (domain.MeteredOutcome[port.RunResult], error) {
	s.runs++
	if request.Validate() != nil || request.Stdin == nil || request.Limits.Time.Milliseconds() != 2000 || request.Limits.MemoryBytes != 512<<20 {
		s.t.Fatal("Judge lost input or resource bounds")
	}
	output := []byte("answer\n")
	sample := false
	for _, item := range s.input.DataInput.SolutionInput.Problem.Samples {
		if request.Stdin.Digest == domain.SumBytes([]byte(item.Input)) {
			output = []byte(item.Output)
			sample = true
		}
	}
	if s.mode == "sample_wa" && sample || s.mode == "differential_wa" && request.Role == port.RoleBrute {
		output = []byte("wrong\n")
	}
	stdout := s.artifact(ctx, domain.ArtifactStdout, fmt.Sprintf("fixture/judge/%d.out", s.runs), output)
	evidence := s.artifact(ctx, domain.ArtifactEvidence, fmt.Sprintf("fixture/judge/%d.json", s.runs), []byte("execution evidence"))
	trace := solutionFixtureTrace(fmt.Sprintf("judge-%d", s.runs), evidence.CallID)
	if s.mode == "shared_execution" && request.Role == port.RoleBrute {
		trace = s.lastTrace
	}
	s.lastTrace = trace
	zero := 0
	result := port.RunResult{CallTrace: trace, Outcome: domain.ProcessExited, ExitCode: &zero, Stdout: &stdout, Execution: &evidence}
	if s.mode == "reference_tle" && !sample && request.Role == port.RoleSolution {
		result.Outcome, result.ExitCode = domain.ProcessTLE, nil
	}
	if s.mode == "brute_mle" && request.Role == port.RoleBrute {
		result.Outcome, result.ExitCode = domain.ProcessMLE, nil
	}
	if s.mode == "reference_ole" && !sample && request.Role == port.RoleSolution {
		result.Outcome, result.ExitCode = domain.ProcessOLE, nil
	}
	return domain.MeteredOutcome[port.RunResult]{Value: &result, CallTrace: trace}, nil
}

func assertJudgeReportRejectsMissingProof(t *testing.T, input application.JudgeInput, report application.JudgeVerificationReport) {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	small := len(input.DataReport.Samples)
	for _, change := range []func(*application.JudgeVerificationReport){
		func(r *application.JudgeVerificationReport) { r.Cases = r.Cases[:len(r.Cases)-1] },
		func(r *application.JudgeVerificationReport) { r.Cases[0].Answer = nil },
		func(r *application.JudgeVerificationReport) { r.Cases[small].Brute = nil },
		func(r *application.JudgeVerificationReport) {
			r.Cases[small].Brute.CallTrace = r.Cases[small].Reference.CallTrace
		},
		func(r *application.JudgeVerificationReport) {
			r.Cases[small].BruteTokenDigest = domain.SumBytes([]byte("foreign"))
		},
		func(r *application.JudgeVerificationReport) { r.Cases[small].Input.Seed = new(uint64) },
		func(r *application.JudgeVerificationReport) { r.PolicyDigest = domain.SumBytes([]byte("foreign")) },
		func(r *application.JudgeVerificationReport) {
			r.Cases[0].Answer = &domain.BlobRef{Digest: domain.SumBytes([]byte("foreign")), Size: 7}
		},
	} {
		var changed application.JudgeVerificationReport
		if err := json.Unmarshal(raw, &changed); err != nil {
			t.Fatal(err)
		}
		change(&changed)
		if changed.ValidateFor(input) == nil {
			t.Fatal("Judge accepted omitted or substituted evidence")
		}
	}
}

func assertJudgeCommittedReadsRejectSubstitution(t *testing.T, ctx context.Context, f *similarityExecutorFixture, generationConfig application.GenerationExecutorConfig, similarityConfig application.SimilarityExecutorConfig, base sandboxexec.Config, runID domain.RunID) {
	t.Helper()
	for _, path := range []domain.SafeRelPath{"judge/dataset.json", "judge/generated/001.out", "judge/samples/001.out"} {
		changed := generationConfig
		changed.Store = &alteredSolutionStageStore{f.store, func(stage *port.CommittedPrivateStage) {
			if stage.Attempt.StageName != "judge" {
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
		if _, err := data.Reader().ReadJudgeVerification(ctx, runID); err == nil {
			t.Fatalf("Judge accepted missing committed artifact %s", path)
		}
	}
}
