package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

func (s *LocalRunService) readSolutionStageInput(ctx context.Context, snapshot domain.RunSnapshot) (any, error) {
	if s.solution == nil || s.solutionSandbox == nil {
		return nil, errors.New("Solution stage has no compatible executor")
	}
	var input any
	switch snapshot.CurrentStage {
	case "similarity_decision":
		content, err := s.similarity.ReadCommitted(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = content
	case "solution":
		accepted, err := s.solution.ReadInput(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		if accepted.Value == nil {
			return nil, errors.New("Solution stage has no current acceptance")
		}
		input = *accepted.Value
	case "solution_verify":
		content, err := s.solution.ReadDraft(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = content
	case "solution_checkpoint", "solution_decision":
		report, err := s.solution.ReadVerification(ctx, snapshot.RunID, *s.solutionSandbox)
		if err != nil {
			return nil, err
		}
		input = report
	default:
		return nil, errors.New("unsupported Solution stage")
	}
	expected, err := s.generation.config.Store.ReadStageInputDigest(ctx, snapshot.RunID, snapshot.CurrentStage)
	if err != nil {
		return nil, err
	}
	digest, err := stageInputDigest(input)
	if err != nil {
		return nil, err
	}
	if digest != expected {
		return nil, errors.New("Solution stage input differs from its committed predecessor")
	}
	return input, nil
}

func (s *LocalRunService) newSolutionSandbox(_ context.Context, identity port.SandboxAuthorizationIdentity) (SolutionSandbox, toolchain.Lock, error) {
	config := *s.solutionSandbox
	config.Identity = identity
	sandbox, err := NewDockerSandboxSession(config)
	return sandbox, config.Lock, err
}

func (s *SolutionExecutor) ReconcileDraft(ctx context.Context, runID domain.RunID) error {
	current, attempt, err := s.generation.reconciliationAttempt(ctx, runID)
	if err != nil || attempt == nil {
		return err
	}
	if current.CurrentStage != "solution" {
		return errors.New("Solution cleanup requires its draft stage")
	}
	input, err := s.ReadInput(ctx, runID)
	if err != nil {
		return err
	}
	if input.Value == nil {
		return errors.New("Solution cleanup lost current acceptance")
	}
	digest, err := input.Value.Digest()
	if err != nil {
		return err
	}
	if digest != attempt.InputDigest {
		return errors.New("Solution cleanup input differs")
	}
	variables, err := input.Value.CanonicalJSON()
	if err != nil {
		return err
	}
	return s.generation.reconcileDraftRequest(ctx, current, *attempt, variables)
}
