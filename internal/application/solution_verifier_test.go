package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

func TestSolutionVerifierCompilesBothProgramsAndChecksSamples(t *testing.T) {
	for _, mode := range []string{"pass", "compile_error", "wrong_answer", "brute_wrong", "time_limit"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			input, content := solutionVerifierContent(t)
			f := newCoordinatorFixtureWithStages(t, "d8", domain.BudgetLimits{MaxArtifactBytes: 8 << 20, MaxActiveTimeMilliseconds: 100000}, []domain.StageName{"prepare", "exercise"})
			blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
			if err != nil {
				t.Fatal(err)
			}
			identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000d8", LogicalOperationID: "solution-verification", Kind: domain.CallSandboxCompile, ScopeDigest: content.ContentDigest, ExpectedRunVersion: 2}
			publisher, err := sandboxexec.NewArtifactSink(f.store, blobs, f.clock, identity)
			if err != nil {
				t.Fatal(err)
			}
			lock := solutionTestLock(t)
			sandbox := &solutionSandboxFixture{publisher: publisher, mode: mode, expected: input.Problem.Samples[0].Output, t: t}
			verifier, err := application.NewSolutionVerifier(application.SolutionVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: blobs, Lock: lock})
			if err != nil {
				t.Fatal(err)
			}
			wrong := content
			wrong.ReferenceCode += "changed"
			if _, err := verifier.Verify(ctx, input, wrong); err == nil || sandbox.compiles != 0 {
				t.Fatal("invalid content reached compilation")
			}
			result, err := verifier.Verify(ctx, input, content)
			if err != nil {
				t.Fatal(err)
			}
			if err := result.Report.ValidateFor(input, content); err != nil {
				t.Fatal(err)
			}
			if result.Report.Passed != (mode == "pass") {
				t.Fatalf("unexpected report: %+v", result.Report)
			}
			if mode == "pass" {
				assertSolutionReportRejectsIncompleteEvidence(t, input, content, result.Report)
			}
			wantCompile, wantRun := 2, 2*len(input.Problem.Samples)
			switch mode {
			case "compile_error":
				wantCompile, wantRun = 1, 0
			case "wrong_answer", "time_limit":
				wantRun = 1
			case "brute_wrong":
				wantRun = 2
			}
			if sandbox.compiles != wantCompile || sandbox.runs != wantRun {
				t.Fatalf("unexpected retries/continuation: compile=%d run=%d", sandbox.compiles, sandbox.runs)
			}
			if result.ReportArtifact.Blob.Digest.Validate() != nil || len(result.Occurrences) < 2 {
				t.Fatalf("verification lost its evidence: %+v", result)
			}
			current, err := f.store.GetRun(ctx, f.runID)
			if err != nil {
				t.Fatal(err)
			}
			out := result.ReportArtifact.Blob.Digest
			if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &out, NextStage: "exercise", NextInputDigest: &out, Occurrences: result.Occurrences, IdempotencyKey: coordinatorID("finish", "solution verification"), At: f.clock.Now()}); err != nil {
				t.Fatalf("verification evidence attachment: %v", err)
			}
		})
	}
}

func assertSolutionReportRejectsIncompleteEvidence(t *testing.T, input domain.SolutionDraftInputV1, content domain.SolutionContent, report application.SolutionVerificationReport) {
	t.Helper()
	for name, alter := range map[string]func(*application.SolutionVerificationReport){
		"missing brute":  func(r *application.SolutionVerificationReport) { r.Compiles = r.Compiles[:1] },
		"missing sample": func(r *application.SolutionVerificationReport) { r.Samples = r.Samples[:len(r.Samples)-1] },
		"wrong role":     func(r *application.SolutionVerificationReport) { r.Samples[0].Role = port.RoleBrute },
		"substituted source": func(r *application.SolutionVerificationReport) {
			r.Compiles[0].Source.Digest = domain.SumBytes([]byte("foreign"))
		},
		"substituted bundle": func(r *application.SolutionVerificationReport) {
			r.Compiles[0].SourceBundleDigest = domain.SumBytes([]byte("foreign"))
		},
		"substituted input": func(r *application.SolutionVerificationReport) {
			r.Samples[0].Input.Digest = domain.SumBytes([]byte("foreign"))
		},
		"false match": func(r *application.SolutionVerificationReport) {
			r.Samples[0].ActualTokenDigest = domain.SumBytes([]byte("foreign"))
		},
		"false failure": func(r *application.SolutionVerificationReport) { r.Passed = false; r.Reason = "invented" },
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(report)
			if err != nil {
				t.Fatal(err)
			}
			var changed application.SolutionVerificationReport
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			alter(&changed)
			if err := changed.ValidateFor(input, content); err == nil {
				t.Fatal("altered report was accepted")
			}
		})
	}
}

func solutionTestLock(t *testing.T) toolchain.Lock {
	t.Helper()
	file, err := os.Open(filepath.Join("..", "..", "config", "toolchains", "docker-v1.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lock, err := toolchain.LoadLock(file)
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

type solutionSandboxFixture struct {
	publisher      *sandboxexec.ArtifactSink
	mode, expected string
	compiles, runs int
	artifacts      []domain.PendingArtifact
	t              *testing.T
}

func (s *solutionSandboxFixture) Artifacts() []domain.PendingArtifact {
	return append([]domain.PendingArtifact(nil), s.artifacts...)
}
func (s *solutionSandboxFixture) artifact(ctx context.Context, role domain.ArtifactRole, path string, data []byte) domain.PendingArtifact {
	s.t.Helper()
	p, err := s.publisher.Publish(ctx, port.ArtifactDeclaration{MediaType: "application/octet-stream", Role: role, LogicalPath: domain.SafeRelPath(path), MaxBytes: 1 << 20, Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: "fixture"}}, data)
	if err != nil {
		s.t.Fatal(err)
	}
	s.artifacts = append(s.artifacts, p)
	return p
}
func (s *solutionSandboxFixture) Compile(ctx context.Context, request port.CompileRequest) (domain.MeteredOutcome[port.CompileResult], error) {
	s.compiles++
	if request.Language != port.LanguageCPP20 || request.Toolchain != "cpp20-gcc-bookworm-v1" || request.SourceBundle.Validate() != nil {
		s.t.Fatal("compile request lost source/language binding")
	}
	evidence := s.artifact(ctx, domain.ArtifactEvidence, fmt.Sprintf("fixture/compile/%d.json", s.compiles), []byte("compile evidence"))
	trace := solutionFixtureTrace(fmt.Sprintf("compile-%d", s.compiles), evidence.CallID)
	result := port.CompileResult{CallTrace: trace, Outcome: domain.CompileCE, Execution: &evidence}
	if s.mode != "compile_error" {
		program := s.artifact(ctx, domain.ArtifactProgram, fmt.Sprintf("fixture/program/%d", s.compiles), []byte("program"))
		result.Outcome, result.Program = domain.CompileOK, &program
	}
	return domain.MeteredOutcome[port.CompileResult]{Value: &result, CallTrace: trace}, nil
}
func (s *solutionSandboxFixture) Run(ctx context.Context, request port.RunRequest) (domain.MeteredOutcome[port.RunResult], error) {
	s.runs++
	if request.Stdin == nil || request.Limits.Time.Milliseconds() != 2000 || request.Limits.MemoryBytes != 512<<20 {
		s.t.Fatal("sample run lost frozen input/resource constraints")
	}
	output := []byte(" \t" + s.expected + "\r\n")
	if s.mode == "wrong_answer" || (s.mode == "brute_wrong" && request.Role == port.RoleBrute) {
		output = []byte("incorrect answer")
	}
	stdout := s.artifact(ctx, domain.ArtifactStdout, fmt.Sprintf("fixture/run/%d.out", s.runs), output)
	evidence := s.artifact(ctx, domain.ArtifactEvidence, fmt.Sprintf("fixture/run/%d.json", s.runs), []byte("execution evidence"))
	trace := solutionFixtureTrace(fmt.Sprintf("run-%d", s.runs), evidence.CallID)
	zero := 0
	result := port.RunResult{CallTrace: trace, Outcome: domain.ProcessExited, ExitCode: &zero, Stdout: &stdout, Execution: &evidence}
	if s.mode == "time_limit" {
		result.Outcome, result.ExitCode = domain.ProcessTLE, nil
	}
	return domain.MeteredOutcome[port.RunResult]{Value: &result, CallTrace: trace}, nil
}
func solutionFixtureTrace(name string, call domain.AttemptCallID) domain.CallTrace {
	return domain.CallTrace{LogicalOperationID: name, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &call, PhysicalAttemptCallIDs: []domain.AttemptCallID{call}}
}

func solutionVerifierContent(t *testing.T) (domain.SolutionDraftInputV1, domain.SolutionContent) {
	t.Helper()
	snapshot, err := domain.NewGenerationRequestSnapshotV1(domain.GenerationRequestV1{SchemaVersion: domain.RequestSchemaV1, Mode: "manual", Brief: "Graphs", Language: "en", Difficulty: "hard", TimeLimitMilliseconds: 2000, MemoryLimitMegabytes: 512, SolutionLanguage: "cpp", VerificationProfile: "default"}, 42)
	if err != nil {
		t.Fatal(err)
	}
	ideaInput, err := domain.NewIdeaDraftInput(snapshot, 2)
	if err != nil {
		t.Fatal(err)
	}
	outputs := llmBuiltinOutputs(t)
	var idea domain.IdeaDraftV1
	if err := json.Unmarshal(outputs["idea.draft"], &idea); err != nil {
		t.Fatal(err)
	}
	batch, err := idea.Bind(ideaInput)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := domain.NewIdeaSelection(snapshot.RequestDigest, batch, batch.Candidates[0].IdeaID, domain.SelectionOrdinalPolicyV1, []string{"deterministic_selection"}, []domain.Digest{batch.BatchDigest})
	if err != nil {
		t.Fatal(err)
	}
	statementInput, err := domain.NewStatementDraftInput(domain.StatementInput{SchemaVersion: domain.StatementInputSchemaV1, RequestSnapshotDigest: snapshot.SnapshotDigest, IdeaBatchDigest: batch.BatchDigest, IdeaSelectionDigest: selection.SelectionDigest, SelectedIdeaID: selection.SelectedIdeaID}, snapshot, batch, selection)
	if err != nil {
		t.Fatal(err)
	}
	var statement domain.StatementDraftV1
	if err := json.Unmarshal(outputs["statement.draft"], &statement); err != nil {
		t.Fatal(err)
	}
	problem, err := statement.Bind(statementInput, 1)
	if err != nil {
		t.Fatal(err)
	}
	input, err := domain.NewSolutionDraftInput(snapshot, problem, domain.SumBytes([]byte("similarity input")), domain.SumBytes([]byte("accepted evidence")), domain.SumBytes([]byte("accept decision")))
	if err != nil {
		t.Fatal(err)
	}
	var draft domain.SolutionDraftV1
	if err := json.Unmarshal(outputs["solution.draft"], &draft); err != nil {
		t.Fatal(err)
	}
	content, err := draft.Bind(input)
	if err != nil {
		t.Fatal(err)
	}
	return input, content
}
