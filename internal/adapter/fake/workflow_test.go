package fake_test

import (
	"context"
	"testing"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/domain"
	"cpgen/internal/workflow"
)

func TestSlice1FakeStepsDeriveDeterministicResults(t *testing.T) {
	prepare := fake.NewPrepareStep(workflow.PrepareCapabilities{})
	exercise := fake.NewExerciseStep(workflow.ExerciseCapabilities{})
	checkpoint := fake.NewCheckpointStep(workflow.CheckpointCapabilities{})
	view := testView(t)
	input := domain.Slice1Input{Brief: "demo", RequestDigest: view.RequestDigest(), ConfigDigest: view.ConfigDigest()}
	first, err := prepare.Run(context.Background(), view, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := prepare.Run(context.Background(), view, input)
	if err != nil {
		t.Fatal(err)
	}
	if first.Value == nil || second.Value == nil || first.Value.Digest != second.Value.Digest {
		t.Fatal("fake prepare is not deterministic")
	}
	evidence, err := exercise.Run(context.Background(), view, *first.Value)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Value == nil {
		t.Fatal("fake exercise did not return evidence")
	}
	result, err := checkpoint.Run(context.Background(), view, *evidence.Value)
	if err != nil {
		t.Fatal(err)
	}
	if result.Review == nil || result.Review.Reason != "slice1_fixture_complete" {
		t.Fatalf("checkpoint result = %+v", result)
	}
}

func TestSlice1FakeStepsExposeControlOutcomes(t *testing.T) {
	view := testView(t)
	prepare := fake.NewPrepareStep(workflow.PrepareCapabilities{})
	for _, scenario := range []string{"blocked", "retry", "failure", "cancel"} {
		input := domain.Slice1Input{Brief: "demo", Scenario: scenario, RequestDigest: view.RequestDigest(), ConfigDigest: view.ConfigDigest()}
		result, err := prepare.Run(context.Background(), view, input)
		if err != nil {
			t.Fatal(err)
		}
		if result.Outcome() == "" || result.Value != nil {
			t.Fatalf("scenario %q result = %+v", scenario, result)
		}
	}
}

func testView(t *testing.T) domain.RunView {
	t.Helper()
	digest := domain.SumBytes([]byte("view"))
	view, err := domain.NewRunView(domain.RunViewData{
		RunID: domain.RunID("run_0123456789abcdef0123456789abcdef"), WorkflowRevision: workflow.Slice1WorkflowRevision,
		SchemaVersion: "cpgen.request/v1", RequestDigest: digest, ConfigDigest: digest, WorkflowDigest: digest,
		State: domain.RunRunning, CurrentStage: "prepare", Version: 1,
		Budget: domain.BudgetSnapshot{Remaining: map[domain.BudgetDimension]int64{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return view
}
