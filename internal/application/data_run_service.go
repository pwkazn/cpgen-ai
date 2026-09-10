package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
)

func (s *LocalRunService) readDataStageInput(ctx context.Context, snapshot domain.RunSnapshot) (any, error) {
	if s.data == nil {
		return nil, errors.New("Data stage has no compatible executor")
	}
	var input any
	switch snapshot.CurrentStage {
	case "data":
		accepted, err := s.data.ReadInput(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		if accepted.Value == nil {
			return nil, errors.New("Data stage has no current passing Solution")
		}
		input = *accepted.Value
	case "data_verify":
		content, err := s.data.ReadDraft(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = content
	case "judge":
		report, err := s.data.ReadVerification(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = report
	case "quality":
		report, err := s.data.ReadJudgeVerification(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = report
	case "package":
		report, err := s.quality.ReadReport(ctx, snapshot.RunID)
		if err != nil {
			return nil, err
		}
		input = report
	default:
		return nil, errors.New("unsupported Data stage")
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
		return nil, errors.New("Data stage input differs from its committed predecessor")
	}
	return input, nil
}
