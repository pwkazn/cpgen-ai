package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cpgen/internal/domain"
)

var _ interface {
	PlanGarbage(context.Context) ([]domain.GCItem, error)
	CommitGarbage(context.Context, domain.GCItem, domain.GCCommit) error
	ListDeleting(context.Context) ([]domain.GCItem, error)
} = (*Store)(nil)

func (s *Store) PlanGarbage(ctx context.Context) ([]domain.GCItem, error) {
	var result []domain.GCItem
	err := s.immediate(ctx, func(tx *immediateTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT digest, size, canonical_relative_path FROM blobs
			WHERE state = 'READY' AND gc_state = 'NONE'
			AND NOT EXISTS (SELECT 1 FROM artifact_occurrences occurrence WHERE occurrence.digest = blobs.digest AND occurrence.size = blobs.size)
			AND NOT EXISTS (SELECT 1 FROM cache_blob_refs cache WHERE cache.digest = blobs.digest AND cache.size = blobs.size)
			AND NOT EXISTS (SELECT 1 FROM blob_pins pin WHERE pin.digest = blobs.digest AND pin.size = blobs.size AND pin.state IN ('ACTIVE','RELEASABLE'))`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item domain.GCItem
			if err := rows.Scan(&item.Ref.Digest, &item.Ref.Size, &item.CanonicalRelativePath); err != nil {
				return err
			}
			if err := item.Ref.Validate(); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE blobs SET gc_state = 'DELETING' WHERE digest = ? AND size = ? AND state = 'READY' AND gc_state = 'NONE'`, item.Ref.Digest, item.Ref.Size); err != nil {
				return err
			}
			result = append(result, item)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) ListDeleting(ctx context.Context) ([]domain.GCItem, error) {
	var result []domain.GCItem
	err := s.immediate(ctx, func(tx *immediateTx) error {
		rows, err := tx.QueryContext(ctx, `SELECT digest, size, canonical_relative_path FROM blobs WHERE gc_state = 'DELETING'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var item domain.GCItem
			if err := rows.Scan(&item.Ref.Digest, &item.Ref.Size, &item.CanonicalRelativePath); err != nil {
				return err
			}
			if err := item.Ref.Validate(); err != nil {
				return err
			}
			result = append(result, item)
		}
		return rows.Err()
	})
	return result, err
}

func (s *Store) CommitGarbage(ctx context.Context, item domain.GCItem, phase domain.GCCommit) error {
	if err := item.Ref.Validate(); err != nil {
		return err
	}
	if !phase.Valid() {
		return fmt.Errorf("invalid garbage commit phase %q", phase)
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		var state, gcState, path string
		if err := tx.QueryRowContext(ctx, `SELECT state, gc_state, canonical_relative_path FROM blobs WHERE digest = ? AND size = ?`, item.Ref.Digest, item.Ref.Size).Scan(&state, &gcState, &path); errors.Is(err, sql.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if path != item.CanonicalRelativePath || gcState != "DELETING" || state != "READY" {
			return wrap(ErrConsistency, "garbage item does not match deleting READY blob", nil)
		}
		switch phase {
		case domain.GCCommitRemoved:
			if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE digest = ? AND size = ? AND gc_state = 'DELETING'`, item.Ref.Digest, item.Ref.Size); err != nil {
				return fmt.Errorf("remove garbage blob metadata: %w", err)
			}
		case domain.GCCommitRepair:
			if _, err := tx.ExecContext(ctx, `UPDATE blobs SET gc_state = 'NONE' WHERE digest = ? AND size = ? AND gc_state = 'DELETING'`, item.Ref.Digest, item.Ref.Size); err != nil {
				return err
			}
		}
		return nil
	})
}
