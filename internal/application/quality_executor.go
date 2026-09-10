package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type QualityExecutor struct{ data *DataExecutor }

func NewQualityExecutor(data *DataExecutor) (*QualityExecutor, error) {
	if data == nil {
		return nil, errors.New("quality requires its current Data/Judge executor")
	}
	return &QualityExecutor{data}, nil
}

func qualityVerificationIdentity(attempt domain.StageAttempt, version int64) port.SandboxAuthorizationIdentity {
	const operation = "quality-verification"
	return port.SandboxAuthorizationIdentity{RunID: attempt.RunID, StageName: attempt.StageName, AttemptID: attempt.AttemptID, SandboxExecutionID: domain.SandboxExecutionID(coordinatorMutationID("sandbox", operation, attempt.RunID, attempt.AttemptID, attempt.InputDigest)), LogicalOperationID: operation, Kind: domain.CallSandboxCompile, ScopeDigest: attempt.InputDigest, ExpectedRunVersion: version}
}

func (s *QualityExecutor) ReadInput(ctx context.Context, runID domain.RunID) (QualityInput, error) {
	var empty QualityInput
	input, err := s.data.ReadJudgeInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	report, err := s.data.ReadJudgeVerification(ctx, runID)
	if err != nil {
		return empty, err
	}
	value := QualityInput{JudgeInput: input, JudgeReport: report}
	if err := value.Validate(); err != nil {
		return empty, err
	}
	return value, nil
}

func (s *QualityExecutor) Run(ctx context.Context, view domain.RunView, factory SolutionSandboxFactory) (QualityVerificationResult, error) {
	var empty QualityVerificationResult
	if factory == nil {
		return empty, errors.New("quality requires a sandbox factory")
	}
	input, err := s.ReadInput(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	digest, err := stableValueDigest(input.JudgeReport)
	if err != nil {
		return empty, err
	}
	attempt, err := s.data.generation.admit(ctx, view, "quality", digest)
	if err != nil {
		return empty, err
	}
	identity := qualityVerificationIdentity(attempt, view.Version())
	sandbox, lock, err := factory(ctx, identity)
	if err != nil {
		return empty, err
	}
	config := s.data.generation.config
	publisher, err := NewSandboxArtifactSink(config.Store, config.Blobs, config.Clock, identity)
	if err != nil {
		return empty, err
	}
	verifier, err := NewQualityVerifier(SolutionVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: config.Blobs, Lock: lock})
	if err != nil {
		return empty, err
	}
	return verifier.Verify(ctx, view.RunID(), input)
}
