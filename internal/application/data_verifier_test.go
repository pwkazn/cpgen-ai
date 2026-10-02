package application_test

import (
	"bytes"
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

func dataVerifierContent(t *testing.T) (domain.DataDraftInputV1, domain.DataContent) {
	t.Helper()
	input, solution := solutionVerifierContent(t)
	bound, err := domain.NewDataDraftInput(input, solution, domain.SumBytes([]byte("verified samples")))
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
	return bound, content
}

func TestDataVerifierRequiresReproductionAndValidatorEvidence(t *testing.T) {
	for _, mode := range []string{"pass", "compile_error", "invalid_sample", "invalid_generated", "nondeterministic", "generator_tle", "generator_ole", "shared_execution"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			input, content := dataVerifierContent(t)
			f := newCoordinatorFixtureWithStages(t, "db", domain.BudgetLimits{MaxArtifactBytes: 16 << 20, MaxActiveTimeMilliseconds: 100000}, []domain.StageName{"prepare", "exercise"})
			blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
			if err != nil {
				t.Fatal(err)
			}
			identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000db", LogicalOperationID: "data-verification", Kind: domain.CallSandboxCompile, ScopeDigest: content.ContentDigest, ExpectedRunVersion: 2}
			publisher, err := sandboxexec.NewArtifactSink(f.store, blobs, f.clock, identity)
			if err != nil {
				t.Fatal(err)
			}
			main := &dataSandboxFixture{solutionSandboxFixture: solutionSandboxFixture{publisher: publisher, mode: mode, t: t}, name: "main", samples: len(input.SolutionInput.Problem.Samples)}
			repeat := &dataSandboxFixture{solutionSandboxFixture: solutionSandboxFixture{publisher: publisher, mode: mode, t: t}, name: "repeat", original: main}
			verifier, err := application.NewDataVerifier(application.DataVerifierConfig{Sandbox: main, RepeatSandbox: repeat, Publisher: publisher, Blobs: blobs, Lock: solutionTestLock(t)})
			if err != nil {
				t.Fatal(err)
			}
			result, err := verifier.Verify(ctx, input, content)
			if mode == "shared_execution" {
				if err == nil {
					t.Fatal("same physical execution counted as reproducibility")
				}
				return
			}
			if err != nil || result.Report.ValidateFor(input, content) != nil || result.Report.Passed != (mode == "pass") {
				t.Fatalf("verification: %+v %v", result.Report, err)
			}
			if (result.DatasetArtifact != nil) != result.Report.Passed {
				t.Fatal("failed data verification produced a dataset")
			}
			if mode == "generator_ole" {
				generated := result.Report.Generated[0]
				if result.Report.Reason != "generated.1.run.1.OLE" || len(generated.Runs) != 1 || generated.Runs[0].Outcome != domain.ProcessOLE || generated.Runs[0].Stdout.Blob.Size != 1<<20 || generated.Validator != nil || result.DatasetArtifact != nil {
					t.Fatalf("one-MiB generator OLE evidence was not stopped and classified: %+v", result.Report)
				}
			}
			if mode == "pass" {
				if main.compiles != 2 || main.runs != len(input.SolutionInput.Problem.Samples)+2*len(content.Plan.Cases) || repeat.runs != len(content.Plan.Cases) {
					t.Fatalf("missing real checks: compile=%d run=%d repeat=%d", main.compiles, main.runs, repeat.runs)
				}
				assertDataReportRejectsMissingProof(t, input, content, result.Report)
			}
			current, err := f.store.GetRun(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			out := result.ReportArtifact.Blob.Digest
			if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &out, NextStage: "exercise", NextInputDigest: &out, Occurrences: result.Occurrences, IdempotencyKey: coordinatorID("finish", "data verification"), At: f.clock.Now()}); err != nil {
				t.Fatalf("atomic verification attachment: %v", err)
			}
		})
	}
}

type dataSandboxFixture struct {
	solutionSandboxFixture
	name        string
	samples     int
	validations int
	original    *dataSandboxFixture
	lastTrace   domain.CallTrace
}

func (s *dataSandboxFixture) Run(ctx context.Context, request port.RunRequest) (domain.MeteredOutcome[port.RunResult], error) {
	s.runs++
	wantStdout := int64(4096)
	if request.Role == port.RoleGenerator {
		wantStdout = 1 << 20
	}
	if request.Validate() != nil || request.Limits.Time.Milliseconds() != 2000 || request.Limits.MemoryBytes != 256<<20 || request.Limits.StdoutBytes != wantStdout {
		s.t.Fatal("data run lost typed arguments or resource bounds")
	}
	zero := 0
	output := []byte{}
	if request.Role == port.RoleGenerator {
		if request.Args.GeneratorCase == nil || request.Stdin != nil {
			s.t.Fatal("generator lost case arguments")
		}
		output = []byte(fmt.Sprintf("%d %d\n", request.Args.GeneratorCase.Ordinal, *request.Seed%97))
		if s.mode == "generator_ole" {
			output = bytes.Repeat([]byte{'x'}, 1<<20)
		}
		if s.mode == "nondeterministic" && s.name == "repeat" {
			output = append(output, ' ')
		}
	} else if request.Role == port.RoleValidator && request.Stdin != nil {
		s.validations++
		if s.mode == "invalid_sample" || (s.mode == "invalid_generated" && s.validations > s.samples) {
			zero = 3
		}
	} else {
		s.t.Fatal("unexpected data run role")
	}
	stdout := s.artifact(ctx, domain.ArtifactStdout, fmt.Sprintf("fixture/%s/%d.out", s.name, s.runs), output)
	evidence := s.artifact(ctx, domain.ArtifactEvidence, fmt.Sprintf("fixture/%s/%d.json", s.name, s.runs), []byte("execution evidence"))
	trace := solutionFixtureTrace(fmt.Sprintf("%s-%d", s.name, s.runs), evidence.CallID)
	if s.mode == "shared_execution" && s.name == "repeat" {
		trace = s.original.lastTrace
	}
	s.lastTrace = trace
	result := port.RunResult{CallTrace: trace, Outcome: domain.ProcessExited, ExitCode: &zero, Stdout: &stdout, Execution: &evidence}
	if s.mode == "generator_tle" && request.Role == port.RoleGenerator {
		result.Outcome, result.ExitCode = domain.ProcessTLE, nil
	}
	if s.mode == "generator_ole" && request.Role == port.RoleGenerator {
		result.Outcome, result.ExitCode = domain.ProcessOLE, nil
	}
	return domain.MeteredOutcome[port.RunResult]{Value: &result, CallTrace: trace}, nil
}

func assertDataReportRejectsMissingProof(t *testing.T, input domain.DataDraftInputV1, content domain.DataContent, report application.DataVerificationReport) {
	t.Helper()
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*application.DataVerificationReport){
		func(r *application.DataVerificationReport) { r.Generated = r.Generated[:1] },
		func(r *application.DataVerificationReport) { r.Generated[0].Runs = r.Generated[0].Runs[:1] },
		func(r *application.DataVerificationReport) { r.Generated[0].Validator = nil },
		func(r *application.DataVerificationReport) { r.Generated[0].Case.Seed++ },
		func(r *application.DataVerificationReport) {
			r.Generated[0].Runs[1].CallTrace = r.Generated[0].Runs[0].CallTrace
		},
		func(r *application.DataVerificationReport) { r.Samples = nil },
		func(r *application.DataVerificationReport) { r.Compiles = r.Compiles[:1] },
	} {
		var changed application.DataVerificationReport
		if err := json.Unmarshal(raw, &changed); err != nil {
			t.Fatal(err)
		}
		change(&changed)
		if changed.ValidateFor(input, content) == nil {
			t.Fatal("passing report accepted missing or substituted evidence")
		}
	}
}
