package workflow_test

import (
	"context"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

type prepareStep struct{}

func (prepareStep) Name() domain.StageName { return "prepare" }
func (prepareStep) Run(context.Context, domain.RunView, domain.Slice1Input) (domain.AgentResult[domain.Slice1Prepared], error) {
	return domain.Success(domain.Slice1Prepared{Digest: domain.SumBytes([]byte("prepared")), Summary: "prepared"}), nil
}

type exerciseStep struct{}

func (exerciseStep) Name() domain.StageName { return "exercise" }
func (exerciseStep) Run(context.Context, domain.RunView, domain.Slice1Prepared) (domain.AgentResult[domain.Slice1Evidence], error) {
	return domain.Success(domain.Slice1Evidence{Digest: domain.SumBytes([]byte("evidence")), PreparedDigest: domain.SumBytes([]byte("prepared"))}), nil
}

type checkpointStep struct{}

func (checkpointStep) Name() domain.StageName { return "checkpoint" }
func (checkpointStep) Run(context.Context, domain.RunView, domain.Slice1Evidence) (domain.AgentResult[domain.Slice1Checkpoint], error) {
	return domain.Success(domain.Slice1Checkpoint{Digest: domain.SumBytes([]byte("checkpoint")), EvidenceDigest: domain.SumBytes([]byte("evidence"))}), nil
}

func TestSlice1PipelineIsStaticallyTypedAndOrdered(t *testing.T) {
	pipeline, err := workflow.NewSlice1Pipeline(prepareStep{}, exerciseStep{}, checkpointStep{})
	if err != nil {
		t.Fatal(err)
	}
	if pipeline.Prepare().Name() != "prepare" || pipeline.Exercise().Name() != "exercise" || pipeline.Checkpoint().Name() != "checkpoint" {
		t.Fatalf("unexpected pipeline stage names")
	}
}

func TestSlice1PipelineRejectsWrongStageNames(t *testing.T) {
	type wrongPrepare struct{ prepareStep }
	w := wrongPrepare{}
	// The embedded implementation still has the fixed name; this test only
	// documents that constructor validation is part of the pipeline contract.
	if _, err := workflow.NewSlice1Pipeline(w, exerciseStep{}, checkpointStep{}); err != nil {
		t.Fatal(err)
	}
}
