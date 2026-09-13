package application

import (
	"context"
	"cpgen/internal/domain"
	"cpgen/internal/runlock"
)

// PackageArchiveReader returns an archive only after verifying its committed evidence.
type PackageArchiveReader interface {
	ReadArchive(context.Context, domain.RunID) ([]byte, domain.VerifiedPackageRecord, error)
}

type lockedPackageReader struct {
	locks  *runlock.Manager
	reader PackageArchiveReader
}

func (r *lockedPackageReader) ReadArchive(ctx context.Context, id domain.RunID) ([]byte, domain.VerifiedPackageRecord, error) {
	var empty domain.VerifiedPackageRecord
	if err := id.Validate(); err != nil {
		return nil, empty, err
	}
	guard, err := r.locks.AcquireRun(ctx, id, runlock.Shared)
	if err != nil {
		return nil, empty, err
	}
	defer guard.Close()
	artifacts, err := r.locks.AcquireArtifacts(ctx, runlock.Shared)
	if err != nil {
		return nil, empty, err
	}
	defer artifacts.Close()
	return r.reader.ReadArchive(ctx, id)
}
