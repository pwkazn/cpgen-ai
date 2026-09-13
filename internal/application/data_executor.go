package application

import (
	"context"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// DataExecutor only admits the current committed, passing Solution. Its model
// output remains a proposal until separately executed and validated in Docker.
type DataExecutor struct {
	publisher StagePublisher
	blobs     port.VerifiedBlobReader
	reader    *DataReader
	sandbox   SandboxReadPolicy
	drafts    *DraftExecution
}

func (s *DataExecutor) CollectDraft(ctx context.Context, view domain.RunView, input domain.DataDraftInputV1) (GenerationStageResult[domain.DataContent], error) {
	var result GenerationStageResult[domain.DataContent]
	digest, err := input.Digest()
	if err != nil {
		return result, err
	}
	attempt, err := s.drafts.admit(ctx, view, "data", digest)
	if err != nil {
		return result, err
	}
	expected, err := s.reader.ReadInput(ctx, view.RunID())
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
	generated, err := s.drafts.generate(ctx, view, attempt, variables)
	result = generationResult[domain.DataContent](generated)
	if err != nil {
		return result, err
	}
	if generated.outcome.Failure != nil {
		result.Outcome = generationFailure[domain.DataContent](view, attempt, s.drafts.config.Content.ProviderPolicyDigest, generated.outcome, s.drafts.config.Clock.Now())
		return result, nil
	}
	var draft domain.DataDraftV1
	if err := json.Unmarshal(generated.outcome.Value.Structured, &draft); err != nil {
		return result, err
	}
	content, err := draft.Bind(input)
	if err != nil {
		result.Outcome = generationContentReview[domain.DataContent](s.drafts.config.Content.ProviderPolicyDigest, generated.outcome.CallTrace, "data_binding_rejected")
		return result, nil
	}
	result.Outcome = domain.Success(content)
	result.Occurrences, result.publication = generated.occurrences, generated.publication
	return result, nil
}

func (s *DataExecutor) ReconcileDraft(ctx context.Context, runID domain.RunID) error {
	current, attempt, err := s.drafts.reconciliationAttempt(ctx, runID)
	if err != nil || attempt == nil {
		return err
	}
	if current.CurrentStage != "data" {
		return errors.New("data cleanup requires its draft stage")
	}
	input, err := s.reader.ReadInput(ctx, runID)
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
	return s.drafts.reconcileDraftRequest(ctx, current, *attempt, variables)
}

func (s *DataExecutor) Reader() *DataReader { return s.reader }
