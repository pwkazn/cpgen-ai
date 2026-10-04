package application_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

var errReadReuseVerificationBoundary = errors.New("stop before uncommitted solution verification")

type countedEvidenceStore struct {
	*sqlite.Store
	reads map[domain.StageName]int
	stop  bool
	alter func(*port.CommittedLLMStage)
}

func (s *countedEvidenceStore) ReadCommittedLLMStage(ctx context.Context, runID domain.RunID, stage domain.StageName) (port.CommittedLLMStage, error) {
	s.reads[stage]++
	value, err := s.Store.ReadCommittedLLMStage(ctx, runID, stage)
	if err == nil && s.alter != nil {
		s.alter(&value)
	}
	return value, err
}

func (s *countedEvidenceStore) ReadCommittedSandboxStage(ctx context.Context, runID domain.RunID, stage domain.StageName) (port.CommittedPrivateStage, error) {
	s.reads[stage]++
	if s.stop {
		return port.CommittedPrivateStage{}, errReadReuseVerificationBoundary
	}
	return s.Store.ReadCommittedSandboxStage(ctx, runID, stage)
}

func countedReaders(t *testing.T, f *similarityExecutorFixture, base sandboxexec.Config) (*application.SolutionReader, *application.DataReader, *application.QualityReader, *countedEvidenceStore) {
	t.Helper()
	store := &countedEvidenceStore{Store: f.store, reads: make(map[domain.StageName]int)}
	config := f.executorConfig
	config.Store = store
	generation, err := application.NewGenerationExecutor(config)
	if err != nil {
		t.Fatal(err)
	}
	similarityConfig := f.config
	similarityConfig.Generation = generation
	evidence, err := application.NewSimilarityExecutor(similarityConfig)
	if err != nil {
		t.Fatal(err)
	}
	solution, err := application.NewSolutionExecutor(evidence, generation)
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
	return solution.Reader(), data.Reader(), quality.Reader(), store
}

// This uses real committed SQLite/Blob/model receipts through the Solution
// draft, without requiring Docker. The unavailable verification must still
// fail closed, after reading each upstream committed stage exactly once.
func TestDownstreamReadersVerifyUpstreamOncePerCall(t *testing.T) {
	for _, revision := range []string{workflow.GenerationRevision, workflow.RetryingGenerationRevision, workflow.ExecutedSamplesRevision} {
		t.Run(revision, func(t *testing.T) {
			ctx := context.Background()
			f, executor := newSolutionExecutorFixtureForWorkflow(t, false, dataDockerOutputs(t, "pass"), revision)
			beginCommittedSolutionVerification(t, f, executor)
			base := sandboxexec.Config{Lock: solutionTestLock(t)}
			solution, data, quality, store := countedReaders(t, f, base)
			store.stop = true
			reads := []struct {
				name string
				read func() error
			}{
				{"solution verification", func() error { _, err := solution.ReadVerification(ctx, f.runID, base.ReadPolicy()); return err }},
				{"data input", func() error { _, err := data.ReadInput(ctx, f.runID); return err }},
				{"data draft", func() error { _, err := data.ReadDraft(ctx, f.runID); return err }},
				{"data verification", func() error { _, err := data.ReadVerification(ctx, f.runID); return err }},
				{"Judge input", func() error { _, err := data.ReadJudgeInput(ctx, f.runID); return err }},
				{"Judge verification", func() error { _, err := data.ReadJudgeVerification(ctx, f.runID); return err }},
				{"quality input", func() error { _, err := quality.ReadInput(ctx, f.runID); return err }},
				{"quality report", func() error { _, err := quality.ReadReport(ctx, f.runID); return err }},
			}
			for _, tc := range reads {
				t.Run(tc.name, func(t *testing.T) {
					for call := 0; call < 2; call++ {
						clear(store.reads)
						if err := tc.read(); !errors.Is(err, errReadReuseVerificationBoundary) {
							t.Fatalf("did not validate up to the missing verification: %v", err)
						}
						want := map[domain.StageName]int{"idea": 1, "statement": 1, "solution": 1, "solution_verify": 1}
						if !reflect.DeepEqual(store.reads, want) {
							t.Fatalf("committed stage reads = %v, want %v", store.reads, want)
						}
					}
					// Mutating the current draft after a read must invalidate the
					// next call on the very same reader; no cross-call cache is safe.
					store.alter = func(stage *port.CommittedLLMStage) {
						if stage.Attempt.StageName == "solution" {
							stage.Attempt.State = domain.StageAttemptInterrupted
						}
					}
					err := tc.read()
					store.alter = nil
					if err == nil || errors.Is(err, errReadReuseVerificationBoundary) {
						t.Fatalf("reused an invalidated Solution stage: %v", err)
					}
				})
			}
			// An earlier successful read must not hide subsequent corruption
			// of a retained response. Same-length damage requires hashing.
			if _, err := solution.ReadDraft(ctx, f.runID); err != nil {
				t.Fatal(err)
			}
			stage, err := f.store.ReadCommittedLLMStage(ctx, f.runID, "solution")
			if err != nil {
				t.Fatal(err)
			}
			hex := strings.TrimPrefix(string(stage.Artifacts[0].Blob.Blob.Digest), "sha256:")
			path := filepath.Join(f.blobRoot, "blobs", "sha256", hex[:2], hex)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			raw[len(raw)/2] ^= 1
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := quality.ReadInput(ctx, f.runID); err == nil || errors.Is(err, errReadReuseVerificationBoundary) {
				t.Fatalf("reused corrupted upstream bytes: %v", err)
			}
			if f.httpCalls.Load() != 3 || f.sends.Load() != 1 {
				t.Fatal("evidence reading dispatched a provider")
			}
		})
	}
}

// Called by both legacy and executed-sample Docker integration fixtures after
// they commit a passing Judge. Count the complete successful read twice to
// cover the upper Data/Judge fan-out as well as per-call revalidation.
func assertQualityReadsEachCommittedStageOnce(t *testing.T, ctx context.Context, f *similarityExecutorFixture, base sandboxexec.Config, runID domain.RunID) {
	t.Helper()
	_, _, quality, store := countedReaders(t, f, base)
	for call := 0; call < 2; call++ {
		clear(store.reads)
		if _, err := quality.ReadInput(ctx, runID); err != nil {
			t.Fatal(err)
		}
		want := map[domain.StageName]int{"idea": 1, "statement": 1, "solution": 1, "solution_verify": 1, "data": 1, "data_verify": 1, "judge": 1}
		if !reflect.DeepEqual(store.reads, want) {
			t.Fatalf("quality committed stage reads = %v, want %v", store.reads, want)
		}
	}
}
