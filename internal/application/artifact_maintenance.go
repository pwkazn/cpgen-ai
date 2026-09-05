package application

import (
	"context"
	"errors"
	"os"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
)

type ArtifactMaintenance struct {
	locks    *runlock.Manager
	metadata port.GCMetadataStore
	blobs    *blob.Store
}

func NewArtifactMaintenance(locks *runlock.Manager, metadata port.GCMetadataStore, blobs *blob.Store) (*ArtifactMaintenance, error) {
	if locks == nil || metadata == nil || blobs == nil {
		return nil, errors.New("artifact maintenance dependencies are required")
	}
	return &ArtifactMaintenance{locks: locks, metadata: metadata, blobs: blobs}, nil
}

func (m *ArtifactMaintenance) CollectGarbage(ctx context.Context) (domain.GCReport, error) {
	if err := ctx.Err(); err != nil {
		return domain.GCReport{}, err
	}
	guard, err := m.locks.AcquireArtifacts(ctx, runlock.Exclusive)
	if err != nil {
		return domain.GCReport{}, err
	}
	defer guard.Close()
	items, err := m.metadata.PlanGarbage(ctx)
	if err != nil {
		return domain.GCReport{}, err
	}
	report := domain.GCReport{Planned: len(items)}
	for _, item := range items {
		moved, moveErr := m.blobs.MoveToTrash(ctx, item.Ref)
		if moveErr != nil {
			if errors.Is(moveErr, blob.ErrBlobNotFound) {
				if repairErr := m.metadata.CommitGarbage(ctx, item, domain.GCCommitRepair); repairErr != nil {
					return report, repairErr
				}
				report.Repaired++
				continue
			}
			return report, moveErr
		}
		if moved {
			report.Moved++
		}
		if err := m.metadata.CommitGarbage(ctx, item, domain.GCCommitRemoved); err != nil {
			return report, err
		}
		report.Removed++
		if err := m.blobs.RemoveTrash(ctx, item.Ref); err != nil {
			return report, err
		}
	}
	return report, nil
}

func (m *ArtifactMaintenance) ReconcileTrash(ctx context.Context) (domain.GCReport, error) {
	if err := ctx.Err(); err != nil {
		return domain.GCReport{}, err
	}
	guard, err := m.locks.AcquireArtifacts(ctx, runlock.Exclusive)
	if err != nil {
		return domain.GCReport{}, err
	}
	defer guard.Close()
	items, err := m.metadata.ListDeleting(ctx)
	if err != nil {
		return domain.GCReport{}, err
	}
	report := domain.GCReport{Planned: len(items)}
	known := make(map[domain.Digest]struct{}, len(items))
	for _, item := range items {
		known[item.Ref.Digest] = struct{}{}
		moved, moveErr := m.blobs.MoveToTrash(ctx, item.Ref)
		if moveErr != nil && !errors.Is(moveErr, blob.ErrBlobNotFound) {
			return report, moveErr
		}
		if moved {
			report.Moved++
		}
		// If the canonical file was absent, MoveToTrash returns successfully
		// only when a deterministic trash copy already exists.  A missing pair
		// is repaired rather than silently deleting a metadata row.
		if _, statErr := m.blobs.StatTrash(ctx, item.Ref); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				if repairErr := m.metadata.CommitGarbage(ctx, item, domain.GCCommitRepair); repairErr != nil {
					return report, repairErr
				}
				report.Repaired++
				continue
			}
			return report, statErr
		}
		if err := m.metadata.CommitGarbage(ctx, item, domain.GCCommitRemoved); err != nil {
			return report, err
		}
		report.Removed++
		if err := m.blobs.RemoveTrash(ctx, item.Ref); err != nil {
			return report, err
		}
	}
	orphans, err := m.blobs.TrashReferences(ctx)
	if err != nil {
		return report, err
	}
	for _, ref := range orphans {
		if _, ok := known[ref.Digest]; ok {
			continue
		}
		if err := m.blobs.RemoveTrash(ctx, ref); err != nil {
			return report, err
		}
		report.Removed++
	}
	return report, nil
}
