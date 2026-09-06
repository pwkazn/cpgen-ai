package fake

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/workflow"
)

type PrepareStep struct{ capabilities workflow.PrepareCapabilities }

func NewPrepareStep(capabilities workflow.PrepareCapabilities) *PrepareStep {
	return &PrepareStep{capabilities: capabilities}
}
func NewPrepare(capabilities workflow.PrepareCapabilities) *PrepareStep {
	return NewPrepareStep(capabilities)
}
func (s *PrepareStep) Name() domain.StageName { return "prepare" }
func (s *PrepareStep) Run(ctx context.Context, view domain.RunView, input domain.Slice1Input) (domain.AgentResult[domain.Slice1Prepared], error) {
	if err := ctx.Err(); err != nil {
		return domain.Cancelled[domain.Slice1Prepared](domain.CancellationEvidence{Cause: domain.CauseUserCancel, Evidence: domain.SumBytes([]byte(err.Error()))}), nil
	}
	if err := input.Validate(); err != nil {
		return domain.AgentResult[domain.Slice1Prepared]{}, err
	}
	if input.RequestDigest != view.RequestDigest() || input.ConfigDigest != view.ConfigDigest() {
		return domain.AgentResult[domain.Slice1Prepared]{}, errors.New("slice1 input is not bound to RunView")
	}
	switch input.Scenario {
	case "blocked":
		return domain.Blocked[domain.Slice1Prepared](blockedCheckpoint(view, input.RequestDigest, "prepare")), nil
	case "retry":
		return domain.Retry[domain.Slice1Prepared](retryFailure(input.RequestDigest)), nil
	case "failure":
		return domain.Failure[domain.Slice1Prepared](permanentFailure(input.RequestDigest)), nil
	case "cancel":
		return domain.Cancelled[domain.Slice1Prepared](cancelEvidence(input.RequestDigest)), nil
	}
	digest := domain.SumBytes([]byte(fmt.Sprintf("slice1.prepare/v1\x00%s\x00%s\x00%s", input.RequestDigest, input.ConfigDigest, view.WorkflowDigest())))
	return domain.Success(domain.Slice1Prepared{Digest: digest, Summary: "slice1 prepared", Scenario: input.Scenario}), nil
}

func (s *PrepareStep) Revalidate(ctx context.Context, view domain.RunView, input domain.Digest) (bool, error) {
	if s.capabilities.LLM == nil {
		return true, nil
	}
	outcome, err := s.capabilities.LLM.Generate(ctx, port.GenerateRequest{Prompt: port.PromptRef{Step: "slice1-dependency", Version: "v1", Digest: view.WorkflowDigest()}, Schema: port.OutputSchemaRef{SchemaVersion: view.SchemaVersion(), Digest: view.ConfigDigest()}, Variables: []byte(`{}`), Sampling: port.SamplingPolicy{TopP: 1}, MaxOutput: port.OutputLimit{Tokens: 1, Bytes: 1}})
	if err != nil {
		return false, err
	}
	return outcome.Failure == nil && outcome.Value != nil, nil
}

type ExerciseStep struct{ capabilities workflow.ExerciseCapabilities }

func NewExerciseStep(capabilities workflow.ExerciseCapabilities) *ExerciseStep {
	return &ExerciseStep{capabilities: capabilities}
}
func NewExercise(capabilities workflow.ExerciseCapabilities) *ExerciseStep {
	return NewExerciseStep(capabilities)
}
func (s *ExerciseStep) Name() domain.StageName { return "exercise" }
func (s *ExerciseStep) Run(ctx context.Context, view domain.RunView, input domain.Slice1Prepared) (domain.AgentResult[domain.Slice1Evidence], error) {
	if err := ctx.Err(); err != nil {
		return domain.Cancelled[domain.Slice1Evidence](domain.CancellationEvidence{Cause: domain.CauseUserCancel, Evidence: domain.SumBytes([]byte(err.Error()))}), nil
	}
	if err := input.Validate(); err != nil {
		return domain.AgentResult[domain.Slice1Evidence]{}, err
	}
	digest := domain.SumBytes([]byte(fmt.Sprintf("slice1.exercise/v1\x00%s\x00%s", input.Digest, view.ConfigDigest())))
	return domain.Success(domain.Slice1Evidence{Digest: digest, PreparedDigest: input.Digest, Scenario: input.Scenario}), nil
}

type CheckpointStep struct {
	capabilities workflow.CheckpointCapabilities
}

func NewCheckpointStep(capabilities workflow.CheckpointCapabilities) *CheckpointStep {
	return &CheckpointStep{capabilities: capabilities}
}
func NewCheckpoint(capabilities workflow.CheckpointCapabilities) *CheckpointStep {
	return NewCheckpointStep(capabilities)
}
func (s *CheckpointStep) Name() domain.StageName { return "checkpoint" }
func (s *CheckpointStep) Run(ctx context.Context, view domain.RunView, input domain.Slice1Evidence) (domain.AgentResult[domain.Slice1Checkpoint], error) {
	if err := ctx.Err(); err != nil {
		return domain.Cancelled[domain.Slice1Checkpoint](domain.CancellationEvidence{Cause: domain.CauseUserCancel, Evidence: domain.SumBytes([]byte(err.Error()))}), nil
	}
	if err := input.Validate(); err != nil {
		return domain.AgentResult[domain.Slice1Checkpoint]{}, err
	}
	switch input.Scenario {
	case "blocked":
		return domain.Blocked[domain.Slice1Checkpoint](blockedCheckpoint(view, input.PreparedDigest, "checkpoint")), nil
	case "retry":
		return domain.Retry[domain.Slice1Checkpoint](retryFailure(input.PreparedDigest)), nil
	case "failure":
		return domain.Failure[domain.Slice1Checkpoint](permanentFailure(input.PreparedDigest)), nil
	case "cancel":
		return domain.Cancelled[domain.Slice1Checkpoint](cancelEvidence(input.PreparedDigest)), nil
	default:
		checkpoint := domain.Slice1Checkpoint{EvidenceDigest: input.Digest, Reason: "slice1_fixture_complete"}
		checkpoint.Digest = domain.SumBytes([]byte(fmt.Sprintf("slice1.checkpoint/v1\x00%s\x00%s", input.Digest, view.WorkflowDigest())))
		return domain.Review[domain.Slice1Checkpoint](domain.ReviewRequest{EvidenceDigest: input.Digest, PolicyDigest: view.ConfigDigest(), Reason: "slice1_fixture_complete"}), nil
	}
}

func (s *CheckpointStep) Revalidate(ctx context.Context, view domain.RunView, input domain.Digest) (bool, error) {
	if s.capabilities.Similarity == nil {
		return true, nil
	}
	outcome, err := s.capabilities.Similarity.Search(ctx, port.SimilaritySearchRequest{Query: "slice1 dependency", QueryDigest: input, Limit: 1})
	if err != nil {
		return false, err
	}
	return outcome.Failure == nil && outcome.Value != nil, nil
}

func blockedCheckpoint(view domain.RunView, input domain.Digest, stage domain.StageName) domain.BlockedCheckpoint {
	dependency := domain.SumBytes([]byte("slice1.dependency/v1\x00" + string(input)))
	policy := view.ConfigDigest()
	return domain.BlockedCheckpoint{RunID: view.RunID(), StageName: stage, StageInputDigest: input, DependencyID: "slice1-provider", DependencyDigest: dependency, PolicyDigest: policy, ErrorDigest: domain.SumBytes([]byte("slice1 blocked")), RetryAfter: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}
func retryFailure(input domain.Digest) domain.RetryableFailure {
	return domain.RetryableFailure{Code: domain.FailureUnavailable, Evidence: domain.SumBytes([]byte("slice1 retry/v1\x00" + string(input)))}
}
func permanentFailure(input domain.Digest) domain.PermanentFailure {
	return domain.PermanentFailure{Code: domain.FailurePolicyRejected, Evidence: domain.SumBytes([]byte("slice1 failure/v1\x00" + string(input)))}
}
func cancelEvidence(input domain.Digest) domain.CancellationEvidence {
	return domain.CancellationEvidence{Cause: domain.CauseUserCancel, Evidence: domain.SumBytes([]byte("slice1 cancel/v1\x00" + string(input)))}
}
