package application

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

// compiledRunGraph schedules one durable stage boundary at a time. The stage
// callback owns input loading, typed invocation and the checked SQLite commit.
// The local loop never stores stage values or chooses a user-defined sequence.
type compiledRunGraph struct {
	definition workflow.Definition
	revision   string
	stages     []domain.StageName
}

type runStageBoundary func(context.Context, domain.RunSnapshot) (domain.RunSnapshot, error)

func newCompiledRunGraph(revision string) (*compiledRunGraph, error) {
	definition, err := workflow.DefinitionFor(revision)
	if err != nil {
		return nil, err
	}
	compiled := &compiledRunGraph{definition: definition, revision: revision, stages: definition.Stages()}
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
	current := cloneGraphSnapshot(snapshot)
	for {
		if err := ctx.Err(); err != nil {
			return current, err
		}
		before := cloneGraphSnapshot(current)
		after, err := advance(ctx, cloneGraphSnapshot(before))
		if err != nil {
			// BeginStage or accounting may have committed before the failure.
			// Keep that projection only if it belongs to the same current stage.
			if g.validateIdentity(before, after) == nil && after.CurrentStage == before.CurrentStage {
				current = cloneGraphSnapshot(after)
			}
			return current, err
		}
		if err := g.validateTransition(before, after); err != nil {
			return current, err
		}
		current = cloneGraphSnapshot(after)
		if current.State != domain.RunRunning {
			return current, nil
		}
	}
}

func (g *compiledRunGraph) validateSnapshot(snapshot domain.RunSnapshot) error {
	if g == nil || len(g.stages) == 0 {
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
	if snapshot.State == domain.RunReady && (!g.definition.ProducesPackage() || snapshot.CurrentStage != "package" || snapshot.CurrentStageOrdinal != len(g.stages)) {
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
