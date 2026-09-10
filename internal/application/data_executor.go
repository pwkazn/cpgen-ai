package application

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/toolchain"
	"cpgen/internal/workflow"
)

// DataExecutor only admits the current committed, passing Solution. Its model
// output remains a proposal until separately executed and validated in Docker.
type DataExecutor struct {
	solution   *SolutionExecutor
	sandbox    DockerSandboxConfig
	generation *GenerationExecutor
}

func NewDataExecutor(solution *SolutionExecutor, sandbox DockerSandboxConfig) (*DataExecutor, error) {
	if solution == nil || solution.similarity.config.WorkflowRevision != workflow.MVPWorkflowRevision {
		return nil, errors.New("data executor requires the forward MVP workflow")
	}
	if _, err := solution.generation.calls(solution.generation.config.Store, "data"); err != nil {
		return nil, err
	}
	raw, err := sandbox.Lock.MarshalIndent()
	if err != nil {
		return nil, err
	}
	sandbox.Lock, err = toolchain.LoadLock(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	return &DataExecutor{solution: solution, sandbox: sandbox, generation: solution.generation}, nil
}

func (s *DataExecutor) ReadInput(ctx context.Context, runID domain.RunID) (domain.AgentResult[domain.DataDraftInputV1], error) {
	var empty domain.AgentResult[domain.DataDraftInputV1]
	input, err := s.solution.ReadInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("data requires current committed acceptance")
	}
	content, err := s.solution.ReadDraft(ctx, runID)
	if err != nil {
		return empty, err
	}
	report, err := s.solution.ReadVerification(ctx, runID, s.sandbox)
	if err != nil {
		return empty, err
	}
	raw, err := json.Marshal(report)
	if err != nil {
		return empty, err
	}
	digest := domain.SumBytes(raw)
	if !report.Passed {
		return domain.Review[domain.DataDraftInputV1](domain.ReviewRequest{EvidenceDigest: digest, PolicyDigest: report.PolicyDigest, Reason: "solution_requires_review:" + report.Reason}), nil
	}
	bound, err := domain.NewDataDraftInput(*input.Value, content, digest)
	if err != nil {
		return empty, err
	}
	return domain.Success(bound), nil
}

func (s *DataExecutor) CollectDraft(ctx context.Context, view domain.RunView, input domain.DataDraftInputV1) (GenerationStageResult[domain.DataContent], error) {
	var result GenerationStageResult[domain.DataContent]
	digest, err := input.Digest()
	if err != nil {
		return result, err
	}
	attempt, err := s.generation.admit(ctx, view, "data", digest)
	if err != nil {
		return result, err
	}
	expected, err := s.ReadInput(ctx, view.RunID())
	if err != nil {
		return result, err
	}
	if expected.Review != nil {
		result.Outcome = domain.Review[domain.DataContent](*expected.Review)
		return result, nil
	}
	expectedDigest, err := expected.Value.Digest()
	if err != nil || expectedDigest != digest {
		return result, errors.New("data input differs from current verified Solution")
	}
	variables, err := input.CanonicalJSON()
	if err != nil {
		return result, err
	}
	generated, err := s.generation.generate(ctx, view, attempt, variables)
	result = generationResult[domain.DataContent](generated)
	if err != nil {
		return result, err
	}
	if generated.outcome.Failure != nil {
		result.Outcome = generationFailure[domain.DataContent](view, attempt, s.generation.config.Content.ProviderPolicyDigest, generated.outcome, s.generation.config.Clock.Now())
		return result, nil
	}
	var draft domain.DataDraftV1
	if err := json.Unmarshal(generated.outcome.Value.Structured, &draft); err != nil {
		return result, err
	}
	content, err := draft.Bind(input)
	if err != nil {
		result.Outcome = generationContentReview[domain.DataContent](s.generation.config.Content.ProviderPolicyDigest, generated.outcome.CallTrace, "data_binding_rejected")
		return result, nil
	}
	result.Outcome = domain.Success(content)
	result.Occurrences, result.publication = generated.occurrences, generated.publication
	return result, nil
}

func (s *DataExecutor) ReadDraft(ctx context.Context, runID domain.RunID) (domain.DataContent, error) {
	var empty domain.DataContent
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("data draft has no passing current Solution")
	}
	variables, err := input.Value.CanonicalJSON()
	if err != nil {
		return empty, err
	}
	digest, err := input.Value.Digest()
	if err != nil {
		return empty, err
	}
	calls, err := s.generation.calls(s.generation.config.Store, "data")
	if err != nil {
		return empty, err
	}
	raw, expected, err := s.generation.reader.readDraft(ctx, runID, "data", digest, variables, calls)
	if err != nil {
		return empty, err
	}
	var draft domain.DataDraftV1
	if err := json.Unmarshal(raw, &draft); err != nil {
		return empty, err
	}
	content, err := draft.Bind(*input.Value)
	if err != nil {
		return empty, err
	}
	if content.ContentDigest != expected {
		return empty, errors.New("committed data digest differs from reconstructed content")
	}
	return content, nil
}

func (s *DataExecutor) ReconcileDraft(ctx context.Context, runID domain.RunID) error {
	current, attempt, err := s.generation.reconciliationAttempt(ctx, runID)
	if err != nil || attempt == nil {
		return err
	}
	if current.CurrentStage != "data" {
		return errors.New("data cleanup requires its draft stage")
	}
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return err
	}
	if input.Value == nil {
		return errors.New("data cleanup lost its passing Solution")
	}
	digest, err := input.Value.Digest()
	if err != nil || digest != attempt.InputDigest {
		return errors.New("data cleanup input differs")
	}
	variables, err := input.Value.CanonicalJSON()
	if err != nil {
		return err
	}
	return s.generation.reconcileDraftRequest(ctx, current, *attempt, variables)
}
