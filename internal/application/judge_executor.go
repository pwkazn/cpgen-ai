package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
)

func judgeVerificationIdentity(attempt domain.StageAttempt, version int64) port.SandboxAuthorizationIdentity {
	const operation = "judge-verification"
	return port.SandboxAuthorizationIdentity{RunID: attempt.RunID, StageName: attempt.StageName, AttemptID: attempt.AttemptID, SandboxExecutionID: domain.SandboxExecutionID(durable.MutationID("sandbox", operation, attempt.RunID, attempt.AttemptID, attempt.InputDigest)), LogicalOperationID: operation, Kind: domain.CallSandboxRun, ScopeDigest: attempt.InputDigest, ExpectedRunVersion: version}
}

func (s *DataExecutor) RunJudge(ctx context.Context, view domain.RunView, factory SolutionSandboxFactory) (JudgeVerificationResult, error) {
	var empty JudgeVerificationResult
	if factory == nil {
		return empty, errors.New("Judge requires its sandbox factory")
	}
	input, err := s.reader.ReadJudgeInput(ctx, view.RunID())
	if err != nil {
		return empty, err
	}
	digest, err := stableValueDigest(input.DataReport)
	if err != nil {
		return empty, err
	}
	attempt, err := s.drafts.admit(ctx, view, "judge", digest)
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
	publisher, err := s.publisher(attempt, view.Version())
	if err != nil {
		return empty, err
	}
	verifier, err := NewJudgeVerifier(JudgeVerifierConfig{Sandbox: sandbox, Publisher: publisher, Blobs: s.blobs, ToolchainLockDigest: lockDigest})
	if err != nil {
		return empty, err
	}
	return verifier.Verify(ctx, input)
}
