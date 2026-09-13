package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type QualityExecutor struct {
	reader    *QualityReader
	blobs     port.VerifiedBlobReader
	admission *StageAdmission
	publisher StagePublisher
}

func qualityVerificationIdentity(attempt domain.StageAttempt, version int64) port.SandboxAuthorizationIdentity {
	const operation = "quality-verification"
	return port.SandboxAuthorizationIdentity{RunID: attempt.RunID, StageName: attempt.StageName, AttemptID: attempt.AttemptID, SandboxExecutionID: domain.SandboxExecutionID(coordinatorMutationID("sandbox", operation, attempt.RunID, attempt.AttemptID, attempt.InputDigest)), LogicalOperationID: operation, Kind: domain.CallSandboxCompile, ScopeDigest: attempt.InputDigest, ExpectedRunVersion: version}
}

func (s *QualityExecutor) Run(ctx context.Context, view domain.RunView, factory SolutionSandboxFactory) (QualityVerificationResult, error) {
	var empty QualityVerificationResult
	if factory == nil {
		return empty, errors.New("quality requires a sandbox factory")
	}
	input, err := s.reader.ReadInput(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	digest, err := stableValueDigest(input.JudgeReport)
	if err != nil {
		return empty, err
	}
	attempt, err := s.admission.admit(ctx, view, "quality", digest)
	if err != nil {
		return empty, err
	}
	identity := qualityVerificationIdentity(attempt, view.Version())
	sandbox, lock, err := factory(ctx, identity)
	if err != nil {
		return empty, err
	}
	publisher, err := s.publisher(attempt, view.Version())
	if err != nil {
		return empty, err
	}
	verifier, err := NewQualityVerifier(SolutionVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: s.blobs, Lock: lock})
	if err != nil {
		return empty, err
	}
	return verifier.Verify(ctx, view.RunID(), input)
}

func (s *QualityExecutor) Reader() *QualityReader { return s.reader }
