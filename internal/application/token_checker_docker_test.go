package application_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/judge"
	"cpgen/internal/port"
)

func TestExportedTokenCheckerMatchesHostThroughDocker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	base := newDockerSandboxTestConfig(t, ctx)
	f := newCoordinatorFixtureAt(t, "dd", domain.BudgetLimits{MaxArtifactBytes: 64 << 20, MaxSandboxCreates: 80, MaxActiveTimeMilliseconds: 300000}, []domain.StageName{"prepare", "exercise"}, domain.SumBytes([]byte("token checker")), time.Now().UTC())
	blobs, err := blob.NewStore(filepath.Join(t.TempDir(), "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: "prepare", AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000dd", LogicalOperationID: "token-checker", Kind: domain.CallSandboxCompile, ScopeDigest: domain.SumBytes(judge.ExactTokenCheckerSource()), ExpectedRunVersion: 2}
	publisher, err := application.NewSandboxArtifactSink(f.store, blobs, clock.Real{}, identity)
	if err != nil {
		t.Fatal(err)
	}
	var files []domain.PendingArtifact
	publish := func(path string, raw []byte) domain.PendingArtifact {
		p, err := publisher.Publish(ctx, port.ArtifactDeclaration{LogicalPath: domain.SafeRelPath(path), Role: domain.ArtifactInput, MediaType: "application/octet-stream", MaxBytes: int64(max(1, len(raw))), Provenance: domain.ProvenanceCandidate{SchemaVersion: "cpgen.token-checker-canary/v1", Producer: "fixture"}}, raw)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, p)
		return p
	}
	source := publish("checker/main.cpp", judge.ExactTokenCheckerSource())
	bundle := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: "main.cpp", Files: []port.SourceFile{{Path: "main.cpp", Blob: source.Blob}}}
	bundle.Digest, err = port.ComputeSourceBundleDigest(bundle)
	if err != nil {
		t.Fatal(err)
	}
	base.Store, base.Blobs, base.Clock, base.Identity = f.store, blobs, clock.Real{}, identity
	worker, err := application.NewDockerSandboxSession(base)
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := worker.Compile(ctx, port.CompileRequest{Language: port.LanguageCPP20, Role: port.RoleChecker, SourceBundle: bundle, Toolchain: "cpp20-gcc-bookworm-v1", Limits: port.CompileLimits{Time: 30 * time.Second, MemoryBytes: 512 << 20, PIDs: 64, OutputBytes: 8 << 20}, ExpectedOutput: "result/files/main"})
	if err != nil || compiled.Value == nil || compiled.Value.Outcome != domain.CompileOK {
		t.Fatalf("checker compilation: %+v %v", compiled, err)
	}
	testInput := publish("checker/input.txt", []byte("input\n"))
	var whitespace, expected strings.Builder
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsSpace(r) {
			whitespace.WriteString("token")
			whitespace.WriteRune(r)
			expected.WriteString("token ")
		}
	}
	cases := []struct {
		name, output, answer string
		failure              bool
	}{
		{"unicode_whitespace", whitespace.String(), expected.String(), false},
		{"empty", " \t\r\n\u3000", "", false},
		{"number_form", "01", "1", false},
		{"extra_token", "1 2", "1", false},
		{"missing_token", "1", "1 2", false},
		{"not_whitespace", "a\u180eb\u200bc\ufeffd", "a b c d", false},
		{"invalid_utf8_equal", "a\xff\xc0b\xe2\x80", "a\xff\xc0b\xe2\x80", false},
		{"invalid_utf8_different", "a\xff", "a\xfe", false},
		{"nul_is_content", "a\x00b", "a b", false},
		{"byte_bound", strings.Repeat("x", (1<<20)+1), "x", true},
	}
	var replayRequest port.RunRequest
	var replayTrace domain.CallTrace
	for index, item := range cases {
		output := publish(fmt.Sprintf("checker/%02d.output", index), []byte(item.output))
		answer := publish(fmt.Sprintf("checker/%02d.answer", index), []byte(item.answer))
		request := port.RunRequest{Role: port.RoleChecker, Program: compiled.Value.Program.Blob, Files: []port.InputMount{{Path: "input.txt", Blob: testInput.Blob}, {Path: "output.txt", Blob: output.Blob}, {Path: "answer.txt", Blob: answer.Blob}}, Limits: port.RunLimits{Time: 2 * time.Second, MemoryBytes: 256 << 20, PIDs: 64, StdoutBytes: 4096, StderrBytes: 4096}}
		result, err := worker.Run(ctx, request)
		if err != nil || result.Value == nil {
			t.Fatalf("%s: %+v %v", item.name, result, err)
		}
		adapted, err := judge.AdaptChecker(*result.Value, judge.DefaultTestlibV1)
		wanted := domain.CheckerWA
		if judge.ExactTokenDigest([]byte(item.output)) == judge.ExactTokenDigest([]byte(item.answer)) {
			wanted = domain.CheckerAC
		}
		if item.failure {
			wanted = domain.CheckerError
		}
		if err != nil || adapted.Outcome == nil || *adapted.Outcome != wanted {
			t.Fatalf("%s: checker=%+v wanted=%s err=%v", item.name, adapted, wanted, err)
		}
		replayRequest, replayTrace = request, result.CallTrace
	}
	before, err := f.store.BudgetSnapshot(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := application.NewDockerSandboxSession(base)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := restarted.Run(ctx, replayRequest)
	if err != nil || !replay.CallTrace.Equal(replayTrace) {
		t.Fatalf("checker replay changed execution: %+v %v", replay, err)
	}
	after, err := f.store.BudgetSnapshot(ctx, f.runID)
	if err != nil || before.Remaining[domain.BudgetDockerContainerCreates] != after.Remaining[domain.BudgetDockerContainerCreates] || before.Remaining[domain.BudgetArtifactPhysicalNewBytes] != after.Remaining[domain.BudgetArtifactPhysicalNewBytes] {
		t.Fatalf("checker replay consumed budget: %+v %v", after, err)
	}
	var occurrences []domain.PendingOccurrence
	for _, p := range append(files, worker.Artifacts()...) {
		pending := p
		occurrences = append(occurrences, domain.PendingOccurrence{Kind: domain.PendingOccurrenceNewWrite, NewWrite: &pending})
	}
	current, err := f.store.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatal(err)
	}
	out := domain.SumBytes([]byte("token checker canary passed"))
	if _, err := f.store.FinishStage(ctx, domain.FinishStageCommand{RunID: f.runID, ExpectedRunVersion: current.Version, StageName: "prepare", AttemptID: f.attemptID, AttemptState: domain.StageAttemptSucceeded, RunState: domain.RunRunning, OutputDigest: &out, NextStage: "exercise", NextInputDigest: &out, Occurrences: occurrences, IdempotencyKey: coordinatorID("finish", "checker"), At: time.Now().UTC()}); err != nil {
		t.Fatalf("checker artifact attachment: %v", err)
	}
}
