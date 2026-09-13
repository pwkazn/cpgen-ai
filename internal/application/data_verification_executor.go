package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func dataVerificationIdentity(attempt domain.StageAttempt, version int64, repeat bool) port.SandboxAuthorizationIdentity {
	operation := "data-verification"
	if repeat {
		operation += "-repeat"
	}
	return port.SandboxAuthorizationIdentity{RunID: attempt.RunID, StageName: attempt.StageName, AttemptID: attempt.AttemptID, SandboxExecutionID: domain.SandboxExecutionID(coordinatorMutationID("sandbox", operation, attempt.RunID, attempt.AttemptID, attempt.InputDigest)), LogicalOperationID: operation, Kind: domain.CallSandboxCompile, ScopeDigest: attempt.InputDigest, ExpectedRunVersion: version}
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
