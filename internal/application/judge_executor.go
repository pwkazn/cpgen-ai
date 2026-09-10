package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func judgeVerificationIdentity(attempt domain.StageAttempt, version int64) port.SandboxAuthorizationIdentity {
	const operation = "judge-verification"
	return port.SandboxAuthorizationIdentity{RunID: attempt.RunID, StageName: attempt.StageName, AttemptID: attempt.AttemptID, SandboxExecutionID: domain.SandboxExecutionID(coordinatorMutationID("sandbox", operation, attempt.RunID, attempt.AttemptID, attempt.InputDigest)), LogicalOperationID: operation, Kind: domain.CallSandboxRun, ScopeDigest: attempt.InputDigest, ExpectedRunVersion: version}
}

func (s *DataExecutor) ReadJudgeInput(ctx context.Context, runID domain.RunID) (JudgeInput, error) {
	var empty JudgeInput
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return empty, err
	}
	if input.Value == nil {
		return empty, errors.New("Judge requires a current passing Solution")
	}
	content, err := s.ReadDraft(ctx, runID)
	if err != nil {
		return empty, err
	}
	report, err := s.ReadVerification(ctx, runID)
	if err != nil {
		return empty, err
	}
	solution, err := s.solution.ReadVerification(ctx, runID, s.sandbox)
	if err != nil {
		return empty, err
	}
	value := JudgeInput{DataInput: *input.Value, Data: content, DataReport: report, SolutionReport: solution}
	if _, err := value.dataset(); err != nil {
		return empty, err
	}
	return value, nil
}

func (s *DataExecutor) RunJudge(ctx context.Context, view domain.RunView, factory SolutionSandboxFactory) (JudgeVerificationResult, error) {
	var empty JudgeVerificationResult
	if factory == nil {
		return empty, errors.New("Judge requires its sandbox factory")
	}
	input, err := s.ReadJudgeInput(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	digest, err := stableValueDigest(input.DataReport)
	if err != nil {
		return empty, err
	}
	attempt, err := s.generation.admit(ctx, view, "judge", digest)
	if err != nil {
		return empty, err
	}
	identity := judgeVerificationIdentity(attempt, view.Version())
	sandbox, lock, err := factory(ctx, identity)
	if err != nil {
		return empty, err
	}
	lockDigest, err := lock.Digest()
	if err != nil || lockDigest != input.DataReport.ToolchainLockDigest {
		return empty, errors.New("Judge changed the frozen toolchain")
	}
	config := s.generation.config
	publisher, err := NewSandboxArtifactSink(config.Store, config.Blobs, config.Clock, identity)
	if err != nil {
		return empty, err
	}
	verifier, err := NewJudgeVerifier(JudgeVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: config.Blobs, ToolchainLockDigest: lockDigest})
	if err != nil {
		return empty, err
	}
	return verifier.Verify(ctx, input)
}
