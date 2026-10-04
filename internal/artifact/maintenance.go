package artifact

import (
	"context"
	"errors"
	"os"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
)

type Maintenance struct {
	locks    *runlock.Manager
	metadata port.GCMetadataStore
	blobs    *blob.Store
}

func NewMaintenance(locks *runlock.Manager, metadata port.GCMetadataStore, blobs *blob.Store) (*Maintenance, error) {
	if locks == nil || metadata == nil || blobs == nil {
		return nil, errors.New("artifact maintenance dependencies are required")
	}
	return &Maintenance{locks: locks, metadata: metadata, blobs: blobs}, nil
}

func (m *Maintenance) CollectGarbage(ctx context.Context) (domain.GCReport, error) {
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
	return m.removeGarbage(ctx, items)
}

// The exclusive artifact lock is held from the fresh plan/list through unlink
// and metadata commit. DELETING stays durable until both canonical and trash
// bytes are gone, so a crash never exposes an old file to a new publication.
func (m *Maintenance) removeGarbage(ctx context.Context, items []domain.GCItem) (domain.GCReport, error) {
	report := domain.GCReport{Planned: len(items)}
	for _, item := range items {
		moved, moveErr := m.blobs.MoveToTrash(ctx, item.Ref)
		if moveErr != nil && !errors.Is(moveErr, blob.ErrBlobNotFound) {
			return report, moveErr
		}
		if moved {
			report.Moved++
		}
		if _, err := m.blobs.StatTrash(ctx, item.Ref); err != nil && !errors.Is(err, os.ErrNotExist) {
			return report, err
		}
		// Missing bytes also complete removal: this is the expected restart
		// state after unlink succeeded but its metadata commit did not.
		if err := m.blobs.RemoveTrash(ctx, item.Ref); err != nil {
			return report, err
		}
		if err := m.metadata.CommitGarbage(ctx, item, domain.GCCommitRemoved); err != nil {
			return report, err
		}
		report.Removed++
	}
	return report, nil
}

func (m *Maintenance) ReconcileTrash(ctx context.Context) (domain.GCReport, error) {
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
	report, err := m.removeGarbage(ctx, items)
	if err != nil {
		return report, err
	}
	known := make(map[domain.Digest]struct{}, len(items))
	for _, item := range items {
		known[item.Ref.Digest] = struct{}{}
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
