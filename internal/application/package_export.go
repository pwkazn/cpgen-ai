package application

import (
	"context"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/runlock"
)

func (s *LocalRunService) ReadPackageArchive(ctx context.Context, runID domain.RunID) ([]byte, domain.VerifiedPackageRecord, error) {
	var empty domain.VerifiedPackageRecord
	if s.archive == nil {
		return nil, empty, errors.New("this workflow does not export verified packages")
	}
	if err := runID.Validate(); err != nil {
		return nil, empty, err
	}
	guard, err := s.locks.AcquireRun(ctx, runID, runlock.Shared)
	if err != nil {
		return nil, empty, err
	}
	defer guard.Close()
	artifacts, err := s.locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		return nil, empty, err
	}
	defer artifacts.Close()
	return s.archive.ReadArchive(ctx, runID)
}
