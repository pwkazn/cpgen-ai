package application

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestCompiledGraphRunsOnlyTheCommittedStageSequence(t *testing.T) {
	for _, revision := range []string{workflow.Slice1WorkflowRevision, workflow.Slice2WorkflowRevision, workflow.Slice2CheckpointWorkflowRevision, workflow.SolutionWorkflowRevision, workflow.MVPWorkflowRevision} {
		t.Run(revision, func(t *testing.T) {
			compiled, err := newCompiledRunGraph(revision)
			if err != nil {
				t.Fatal(err)
			}
			stages := graphTestStages(revision)
			for start := range stages {
				initial := graphTestSnapshot(revision, start)
				var seen []domain.StageName
				var active atomic.Int32
				result, err := compiled.run(context.Background(), initial, func(_ context.Context, current domain.RunSnapshot) (domain.RunSnapshot, error) {
					if active.Add(1) != 1 {
						t.Error("graph overlapped stages")
					}
					defer active.Add(-1)
					seen = append(seen, current.CurrentStage)
					return graphTestCommit(current, stages), nil
				})
				if err != nil || result.State != domain.RunNeedsReview || !reflect.DeepEqual(seen, stages[start:]) {
					t.Fatalf("start=%d result=%+v seen=%v error=%v", start, result, seen, err)
				}
			}
		})
	}
}

func TestCompiledGraphPropagatesCommitFailureWithoutRetryOrLosingProjection(t *testing.T) {
	compiled, err := newCompiledRunGraph(workflow.Slice1WorkflowRevision)
	if err != nil {
		t.Fatal(err)
	}
	initial := graphTestSnapshot(workflow.Slice1WorkflowRevision, 0)
	commitErr := errors.New("fixture durable stage commit failed")
	calls := 0
	result, err := compiled.run(context.Background(), initial, func(_ context.Context, current domain.RunSnapshot) (domain.RunSnapshot, error) {
		calls++
		current.Version++ // BeginStage committed, but FinishStage failed.
		current.State = domain.RunRunning
		return current, commitErr
	})
	if !errors.Is(err, commitErr) || calls != 1 || result.Version != initial.Version+1 || result.CurrentStage != "prepare" {
		t.Fatalf("result=%+v calls=%d error=%v", result, calls, err)
	}
}

func TestCompiledGraphResumesInterruptedOrBlockedCurrentStage(t *testing.T) {
	compiled, err := newCompiledRunGraph(workflow.Slice2WorkflowRevision)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []domain.RunState{domain.RunCreated, domain.RunBlocked} {
		initial := graphTestSnapshot(workflow.Slice2WorkflowRevision, 1)
		initial.State = state
		var seen []domain.StageName
		result, err := compiled.run(context.Background(), initial, func(_ context.Context, s domain.RunSnapshot) (domain.RunSnapshot, error) {
			seen = append(seen, s.CurrentStage)
			return graphTestCommit(s, graphTestStages(workflow.Slice2WorkflowRevision)), nil
		})
		if err != nil || result.State != domain.RunNeedsReview || !reflect.DeepEqual(seen, []domain.StageName{"statement", "similarity"}) {
			t.Fatalf("state=%s result=%+v seen=%v error=%v", state, result, seen, err)
		}
	}
}

func TestCompiledGraphStopsAtControlOutcomesAndCancellation(t *testing.T) {
	compiled, err := newCompiledRunGraph(workflow.Slice2WorkflowRevision)
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []domain.RunState{domain.RunBlocked, domain.RunNeedsReview, domain.RunFailed, domain.RunCancelled} {
		t.Run(string(outcome), func(t *testing.T) {
			calls := 0
			result, err := compiled.run(context.Background(), graphTestSnapshot(workflow.Slice2WorkflowRevision, 0), func(_ context.Context, current domain.RunSnapshot) (domain.RunSnapshot, error) {
				calls++
				current.Version++
				current.State = outcome
				if outcome == domain.RunCancelled {
					current.CancelSummary = "cancelled"
				}
				return current, nil
			})
			if err != nil || calls != 1 || result.State != outcome {
				t.Fatalf("result=%+v calls=%d error=%v", result, calls, err)
			}
		})
	}
	for _, cancelBefore := range []bool{true, false} {
		ctx, cancel := context.WithCancel(context.Background())
		if cancelBefore {
			cancel()
		}
		calls := 0
		result, err := compiled.run(ctx, graphTestSnapshot(workflow.Slice2WorkflowRevision, 0), func(_ context.Context, current domain.RunSnapshot) (domain.RunSnapshot, error) {
			calls++
			cancel()
			return graphTestCommit(current, graphTestStages(workflow.Slice2WorkflowRevision)), nil
		})
		cancel()
		wantCalls := 1
		if cancelBefore {
			wantCalls = 0
		}
		if !errors.Is(err, context.Canceled) || calls != wantCalls {
			t.Fatalf("before=%v result=%+v calls=%d error=%v", cancelBefore, result, calls, err)
		}
	}
}

func TestCompiledGraphRejectsIncompatibleEntryBeforeWork(t *testing.T) {
	if _, err := newCompiledRunGraph("user-supplied-graph"); err == nil {
		t.Fatal("uncompiled revision admitted")
	}
	compiled, err := newCompiledRunGraph(workflow.Slice1WorkflowRevision)
	if err != nil {
		t.Fatal(err)
	}
	for name, alter := range map[string]func(*domain.RunSnapshot){
		"revision":        func(s *domain.RunSnapshot) { s.WorkflowRevision = workflow.Slice2WorkflowRevision },
		"workflow digest": func(s *domain.RunSnapshot) { s.WorkflowDigest = domain.SumBytes([]byte("other")) },
		"schema":          func(s *domain.RunSnapshot) { s.SchemaVersion = "cpgen.request/v2" },
		"unknown stage":   func(s *domain.RunSnapshot) { s.CurrentStage = "package" },
		"stage ordinal":   func(s *domain.RunSnapshot) { s.CurrentStageOrdinal = 2 },
		"premature ready": func(s *domain.RunSnapshot) { s.State = domain.RunReady },
	} {
		t.Run(name, func(t *testing.T) {
			initial := graphTestSnapshot(workflow.Slice1WorkflowRevision, 0)
			alter(&initial)
			calls := 0
			_, err := compiled.run(context.Background(), initial, func(_ context.Context, s domain.RunSnapshot) (domain.RunSnapshot, error) { calls++; return s, nil })
			if err == nil || calls != 0 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestCompiledGraphRejectsInvalidTransitionsBeforeDownstreamWork(t *testing.T) {
	compiled, err := newCompiledRunGraph(workflow.Slice1WorkflowRevision)
	if err != nil {
		t.Fatal(err)
	}
	for name, alter := range map[string]func(*domain.RunSnapshot){
		"uncommitted":          func(s *domain.RunSnapshot) { s.Version-- },
		"same stage loop":      func(s *domain.RunSnapshot) { s.CurrentStage = "prepare"; s.CurrentStageOrdinal = 1 },
		"skip":                 func(s *domain.RunSnapshot) { s.CurrentStage = "checkpoint"; s.CurrentStageOrdinal = 3 },
		"change run":           func(s *domain.RunSnapshot) { s.RunID = "run_00000000000000000000000000000002" },
		"change request":       func(s *domain.RunSnapshot) { s.RequestDigest = domain.SumBytes([]byte("other request")) },
		"change config":        func(s *domain.RunSnapshot) { s.ConfigDigest = domain.SumBytes([]byte("other config")) },
		"ready":                func(s *domain.RunSnapshot) { s.State = domain.RunReady },
		"pause after skipping": func(s *domain.RunSnapshot) { s.State = domain.RunBlocked },
	} {
		t.Run(name, func(t *testing.T) {
			calls := 0
			initial := graphTestSnapshot(workflow.Slice1WorkflowRevision, 0)
			_, err := compiled.run(context.Background(), initial, func(_ context.Context, s domain.RunSnapshot) (domain.RunSnapshot, error) {
				calls++
				s = graphTestCommit(s, graphTestStages(workflow.Slice1WorkflowRevision))
				alter(&s)
				return s, nil
			})
			if err == nil || calls != 1 {
				t.Fatalf("calls=%d error=%v", calls, err)
			}
		})
	}
}

func TestCompiledGraphConcurrentInvocationsHaveSeparateProgress(t *testing.T) {
	compiled, err := newCompiledRunGraph(workflow.Slice1WorkflowRevision)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for start := 0; start < 3; start++ {
		wg.Add(1)
		go func(start int) {
			defer wg.Done()
			calls := 0
			_, err := compiled.run(context.Background(), graphTestSnapshot(workflow.Slice1WorkflowRevision, start), func(_ context.Context, s domain.RunSnapshot) (domain.RunSnapshot, error) {
				calls++
				return graphTestCommit(s, graphTestStages(workflow.Slice1WorkflowRevision)), nil
			})
			if err != nil || calls != 3-start {
				t.Errorf("start=%d calls=%d error=%v", start, calls, err)
			}
		}(start)
	}
	wg.Wait()
}

func graphTestStages(revision string) []domain.StageName {
	if revision == workflow.MVPWorkflowRevision {
		return []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_decision", "data", "data_verify", "judge", "quality", "package"}
	}
	if revision == workflow.SolutionWorkflowRevision {
		return []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_checkpoint"}
	}
	if revision == workflow.Slice2CheckpointWorkflowRevision {
		return []domain.StageName{"idea", "statement", "similarity", "slice2_checkpoint"}
	}
	if revision == workflow.Slice2WorkflowRevision {
		return []domain.StageName{"idea", "statement", "similarity"}
	}
	return []domain.StageName{"prepare", "exercise", "checkpoint"}
}

func graphTestSnapshot(revision string, start int) domain.RunSnapshot {
	state := domain.RunCreated
	if start > 0 {
		state = domain.RunRunning
	}
	at := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	return domain.RunSnapshot{RunID: "run_00000000000000000000000000000001", State: state, Version: int64(start + 1), WorkflowRevision: revision, SchemaVersion: domain.RequestSchemaV1, RequestDigest: domain.SumBytes([]byte("request")), ConfigDigest: domain.SumBytes([]byte("config")), WorkflowDigest: domain.SumBytes([]byte(revision)), CurrentStage: graphTestStages(revision)[start], CurrentStageOrdinal: start + 1, CreatedAt: at, UpdatedAt: at}
}

func graphTestCommit(current domain.RunSnapshot, stages []domain.StageName) domain.RunSnapshot {
	current.Version++
	current.State = domain.RunRunning
	if current.CurrentStageOrdinal == len(stages) {
		current.State = domain.RunNeedsReview
		return current
	}
	current.CurrentStage = stages[current.CurrentStageOrdinal]
	current.CurrentStageOrdinal++
	return current
}
