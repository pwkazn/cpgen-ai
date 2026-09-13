package fake_test

import (
	"context"
	"testing"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/domain"
)

type prepareStep struct{}

func (prepareStep) Name() domain.StageName { return "prepare" }
func (prepareStep) Run(context.Context, domain.RunView, domain.FakeInput) (domain.AgentResult[domain.FakePrepared], error) {
	return domain.Success(domain.FakePrepared{Digest: domain.SumBytes([]byte("prepared")), Summary: "prepared"}), nil
}

type exerciseStep struct{}

func (exerciseStep) Name() domain.StageName { return "exercise" }
func (exerciseStep) Run(context.Context, domain.RunView, domain.FakePrepared) (domain.AgentResult[domain.FakeEvidence], error) {
	return domain.Success(domain.FakeEvidence{Digest: domain.SumBytes([]byte("evidence")), PreparedDigest: domain.SumBytes([]byte("prepared"))}), nil
}

type checkpointStep struct{}

func (checkpointStep) Name() domain.StageName { return "checkpoint" }
func (checkpointStep) Run(context.Context, domain.RunView, domain.FakeEvidence) (domain.AgentResult[domain.FakeCheckpoint], error) {
	return domain.Success(domain.FakeCheckpoint{Digest: domain.SumBytes([]byte("checkpoint")), EvidenceDigest: domain.SumBytes([]byte("evidence"))}), nil
}

func TestPipelineIsStaticallyTypedAndOrdered(t *testing.T) {
	pipeline, err := fake.NewPipeline(prepareStep{}, exerciseStep{}, checkpointStep{})
	if err != nil {
		t.Fatal(err)
	}
	if pipeline.Prepare().Name() != "prepare" || pipeline.Exercise().Name() != "exercise" || pipeline.Checkpoint().Name() != "checkpoint" {
		t.Fatalf("unexpected pipeline stage names")
	}
}

type wrongPrepare struct{ prepareStep }

func (wrongPrepare) Name() domain.StageName { return "exercise" }

func TestPipelineRejectsWrongStageNames(t *testing.T) {
	if _, err := fake.NewPipeline(wrongPrepare{}, exerciseStep{}, checkpointStep{}); err == nil {
		t.Fatal("accepted prepare step with the wrong name")
	}
	if _, err := fake.NewPipeline(nil, exerciseStep{}, checkpointStep{}); err == nil {
		t.Fatal("accepted missing prepare step")
	}
}
