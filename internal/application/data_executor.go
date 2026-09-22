package application

import (
	"context"
	"encoding/json"
	"errors"

	sandboxexec "cpgen/internal/adapter/sandbox"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
)

// DataExecutor only admits the current committed, passing Solution. Its model
// output remains a proposal until separately executed and validated in Docker.
type DataExecutor struct {
	publisher StagePublisher
	blobs     port.VerifiedBlobReader
	reader    *DataReader
	sandbox   sandboxexec.ReadPolicy
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
	variables, err := dataDraftVariables(view.WorkflowRevision(), input)
	if err != nil {
		return result, err
	}
	generated, err := s.drafts.generate(ctx, view, attempt, variables)
	result = generationResult[domain.DataContent](generated)
	if err != nil {
		return result, err
	}
	if generated.outcome.Failure != nil {
		result.Outcome = draftFailure[domain.DataContent](view, attempt, s.drafts.config.Content.ProviderPolicyDigest, generated, s.drafts.config.Clock.Now())
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
	variables, err := dataDraftVariables(current.WorkflowRevision, *input.Value)
	if err != nil {
		return err
	}
	return s.drafts.reconcileDraftRequest(ctx, current, *attempt, variables)
}

func (s *DataExecutor) Reader() *DataReader { return s.reader }

func dataVerificationIdentity(attempt domain.StageAttempt, version int64, repeat bool) port.SandboxAuthorizationIdentity {
	operation := "data-verification"
	if repeat {
		operation += "-repeat"
	}
	return port.SandboxAuthorizationIdentity{RunID: attempt.RunID, StageName: attempt.StageName, AttemptID: attempt.AttemptID, SandboxExecutionID: domain.SandboxExecutionID(durable.MutationID("sandbox", operation, attempt.RunID, attempt.AttemptID, attempt.InputDigest)), LogicalOperationID: operation, Kind: domain.CallSandboxCompile, ScopeDigest: attempt.InputDigest, ExpectedRunVersion: version}
}

func (s *DataExecutor) VerifyDraft(ctx context.Context, view domain.RunView, factory SolutionSandboxFactory) (DataVerificationResult, error) {
	var empty DataVerificationResult
	if factory == nil {
		return empty, errors.New("data verification requires its sandbox factory")
	}
	input, err := s.reader.ReadInput(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("data verification requires a passing Solution")
	}
	content, err := s.reader.ReadDraft(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	attempt, err := s.drafts.admit(ctx, view, "data_verify", content.ContentDigest)
	if err != nil {
		return empty, err
	}
	identity := dataVerificationIdentity(attempt, view.Version(), false)
	main, lock, err := factory(ctx, identity)
	if err != nil {
		return empty, err
	}
	repeat, repeatLock, err := factory(ctx, dataVerificationIdentity(attempt, view.Version(), true))
	if err != nil {
		return empty, err
	}
	wanted, err := s.sandbox.Lock.Digest()
	if err != nil {
		return empty, err
	}
	mainDigest, mainErr := lock.Digest()
	repeatDigest, repeatErr := repeatLock.Digest()
	if mainErr != nil || repeatErr != nil || mainDigest != wanted || repeatDigest != wanted {
		return empty, errors.New("data execution changed the frozen toolchain")
	}
	publisher, err := s.publisher(attempt, view.Version())
	if err != nil {
		return empty, err
	}
	verifier, err := NewDataVerifier(DataVerifierConfig{Sandbox: main, RepeatSandbox: repeat, Publisher: publisher, Blobs: s.blobs, Lock: lock})
	if err != nil {
		return empty, err
	}
	return verifier.Verify(ctx, *input.Value, content)
}
