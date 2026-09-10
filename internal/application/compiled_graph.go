package application

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"cpgen/internal/domain"
	"cpgen/internal/workflow"

	"github.com/smallnest/langgraphgo/graph"
)

// compiledRunGraph schedules one durable stage boundary at a time. The stage
// callback owns input loading, typed invocation and the checked SQLite commit.
// This graph never stores stage values or chooses a user-defined sequence.
type compiledRunGraph struct {
	revision string
	stages   []domain.StageName
	runnable *graph.StateRunnable[*runGraphFrame]
}

type runStageBoundary func(context.Context, domain.RunSnapshot) (domain.RunSnapshot, error)

// Each invocation owns its frame. The library returns a zero state on ordinary
// errors, so the caller retains the last checked projection independently. This
// is transient progress metadata, not a checkpoint or an alternative state store.
type runGraphFrame struct {
	snapshot domain.RunSnapshot
	advance  runStageBoundary
}

func newCompiledRunGraph(revision string) (*compiledRunGraph, error) {
	compiled := &compiledRunGraph{revision: revision}
	switch revision {
	case workflow.Slice1WorkflowRevision:
		compiled.stages = []domain.StageName{"prepare", "exercise", "checkpoint"}
	case workflow.Slice2WorkflowRevision:
		compiled.stages = []domain.StageName{"idea", "statement", "similarity"}
	case workflow.Slice2CheckpointWorkflowRevision:
		compiled.stages = []domain.StageName{"idea", "statement", "similarity", "slice2_checkpoint"}
	case workflow.SolutionWorkflowRevision:
		compiled.stages = []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_checkpoint"}
	case workflow.MVPWorkflowRevision:
		compiled.stages = []domain.StageName{"idea", "statement", "similarity", "similarity_decision", "solution", "solution_verify", "solution_decision", "data", "data_verify", "judge", "quality", "package"}
	default:
		return nil, errors.New("workflow revision has no compatible compiled graph")
	}
	g := graph.NewStateGraph[*runGraphFrame]()
	// Do not install retries, schemas/mergers, tracing, callbacks, checkpoints or
	// library resume configuration. All edges choose exactly one compiled node.
	g.SetRetryPolicy(nil)
	g.AddNode("resume", "select the authoritative current stage", func(ctx context.Context, frame *runGraphFrame) (*runGraphFrame, error) {
		return frame, ctx.Err()
	})
	g.SetEntryPoint("resume")
	g.AddConditionalEdge("resume", func(_ context.Context, frame *runGraphFrame) string { return string(frame.snapshot.CurrentStage) })
	for _, stage := range compiled.stages {
		g.AddNode(string(stage), "execute and commit one typed stage", func(ctx context.Context, frame *runGraphFrame) (*runGraphFrame, error) {
			if err := ctx.Err(); err != nil {
				return frame, err
			}
			before := cloneGraphSnapshot(frame.snapshot)
			if before.CurrentStage != stage {
				return frame, errors.New("graph node differs from current committed stage")
			}
			after, err := frame.advance(ctx, cloneGraphSnapshot(before))
			if err != nil {
				// BeginStage or accounting may have committed before the failure.
				// Preserve that projection, but never a foreign or regressed one.
				if compiled.validateIdentity(before, after) == nil && after.CurrentStage == before.CurrentStage {
					frame.snapshot = cloneGraphSnapshot(after)
				}
				return frame, err
			}
			if err := compiled.validateTransition(before, after); err != nil {
				return frame, err
			}
			frame.snapshot = cloneGraphSnapshot(after)
			return frame, nil
		})
		g.AddConditionalEdge(string(stage), func(_ context.Context, frame *runGraphFrame) string {
			if frame.snapshot.State != domain.RunRunning {
				return graph.END
			}
			return string(frame.snapshot.CurrentStage)
		})
	}
	var err error
	compiled.runnable, err = g.Compile()
	if err != nil {
		return nil, fmt.Errorf("compile fixed workflow: %w", err)
	}
	return compiled, nil
}

func (g *compiledRunGraph) run(ctx context.Context, snapshot domain.RunSnapshot, advance runStageBoundary) (domain.RunSnapshot, error) {
	if err := g.validateSnapshot(snapshot); err != nil {
		return snapshot, err
	}
	if err := ctx.Err(); err != nil {
		return snapshot, err
	}
	if advance == nil {
		return snapshot, errors.New("durable stage boundary is required")
	}
	switch snapshot.State {
	case domain.RunCreated, domain.RunRunning, domain.RunBlocked:
	default:
		return snapshot, nil
	}
	frame := &runGraphFrame{snapshot: cloneGraphSnapshot(snapshot), advance: advance}
	_, err := g.runnable.Invoke(ctx, frame)
	return cloneGraphSnapshot(frame.snapshot), err
}

func (g *compiledRunGraph) validateSnapshot(snapshot domain.RunSnapshot) error {
	if g == nil || g.runnable == nil {
		return errors.New("compiled workflow graph is required")
	}
	if err := snapshot.Validate(); err != nil {
		return fmt.Errorf("graph run projection: %w", err)
	}
	if snapshot.WorkflowRevision != g.revision || snapshot.WorkflowDigest != domain.SumBytes([]byte(g.revision)) {
		return errors.New("run workflow revision or digest is incompatible with compiled graph")
	}
	if snapshot.SchemaVersion != domain.RequestSchemaV1 {
		return errors.New("run schema is incompatible with compiled graph")
	}
	ordinal := slices.Index(g.stages, snapshot.CurrentStage)
	if ordinal < 0 || snapshot.CurrentStageOrdinal != ordinal+1 {
		return errors.New("run stage or ordinal is incompatible with compiled graph")
	}
	if snapshot.State == domain.RunReady && (g.revision != workflow.MVPWorkflowRevision || snapshot.CurrentStage != "package" || snapshot.CurrentStageOrdinal != len(g.stages)) {
		return errors.New("unfinished compiled slice cannot produce READY")
	}
	return nil
}

func (g *compiledRunGraph) validateIdentity(before, after domain.RunSnapshot) error {
	if err := g.validateSnapshot(after); err != nil {
		return err
	}
	if after.RunID != before.RunID || after.RequestDigest != before.RequestDigest || after.ConfigDigest != before.ConfigDigest || !after.CreatedAt.Equal(before.CreatedAt) {
		return errors.New("stage changed immutable run bindings")
	}
	if after.Version < before.Version || after.UpdatedAt.Before(before.UpdatedAt) || after.ActiveElapsed < before.ActiveElapsed {
		return errors.New("stage returned a regressed run projection")
	}
	return nil
}

func (g *compiledRunGraph) validateTransition(before, after domain.RunSnapshot) error {
	if err := g.validateIdentity(before, after); err != nil {
		return err
	}
	if after.Version <= before.Version {
		return errors.New("stage advanced without a committed run version")
	}
	if after.ActiveStartedAt != nil {
		return errors.New("stage boundary retains an open active-time interval")
	}
	switch after.State {
	case domain.RunReady:
		if before.CurrentStage != "package" || after.CurrentStage != before.CurrentStage || after.FinalPackageOccurrenceID == nil {
			return errors.New("READY did not finish its verified package stage")
		}
	case domain.RunRunning:
		if before.CurrentStageOrdinal >= len(g.stages) {
			return errors.New("compiled slice boundary requires an explicit pause")
		}
		if after.CurrentStageOrdinal != before.CurrentStageOrdinal+1 || after.CurrentStage != g.stages[before.CurrentStageOrdinal] {
			return errors.New("stage did not advance to its exact compiled successor")
		}
	case domain.RunBlocked, domain.RunNeedsReview, domain.RunFailed, domain.RunCancelled:
		if after.CurrentStage != before.CurrentStage {
			return errors.New("stage pause or terminal outcome moved to another stage")
		}
	default:
		return errors.New("unsupported compiled stage transition")
	}
	return nil
}

func cloneGraphSnapshot(snapshot domain.RunSnapshot) domain.RunSnapshot {
	if snapshot.FinalPackageOccurrenceID != nil {
		value := *snapshot.FinalPackageOccurrenceID
		snapshot.FinalPackageOccurrenceID = &value
	}
	if snapshot.ActiveStartedAt != nil {
		value := *snapshot.ActiveStartedAt
		snapshot.ActiveStartedAt = &value
	}
	if snapshot.LastAccountingHeartbeatAt != nil {
		value := *snapshot.LastAccountingHeartbeatAt
		snapshot.LastAccountingHeartbeatAt = &value
	}
	return snapshot
}
