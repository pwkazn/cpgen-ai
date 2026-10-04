package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cpgen/internal/domain"
)

// Released pins and writer tokens are audit evidence, not retaining references.
const unreferencedBlobSQL = `NOT EXISTS (SELECT 1 FROM artifact_occurrences occurrence WHERE occurrence.digest = blobs.digest AND occurrence.size = blobs.size)
	AND NOT EXISTS (SELECT 1 FROM cache_blob_refs cache WHERE cache.digest = blobs.digest AND cache.size = blobs.size)
	AND NOT EXISTS (SELECT 1 FROM blob_pins pin WHERE pin.digest = blobs.digest AND pin.size = blobs.size AND pin.state IN ('ACTIVE','RELEASABLE'))`

// PlanGarbage and ListDeleting must be called after acquiring the exclusive
// artifact lock. Only their freshly returned items authorize filesystem work;
// a saved item must never be reused after releasing that lock.
func (s *Store) PlanGarbage(ctx context.Context) ([]domain.GCItem, error) {
	var result []domain.GCItem
	err := s.immediate(ctx, func(tx *immediateTx) error {
		var err error
		result, err = listGarbage(ctx, tx, `state = 'READY' AND gc_state = 'NONE' AND gc_removed_at IS NULL AND `+unreferencedBlobSQL)
		if err != nil {
			return err
		}
		for _, item := range result {
			if _, err := tx.ExecContext(ctx, `UPDATE blobs SET gc_state = 'DELETING' WHERE digest = ? AND size = ?`, item.Ref.Digest, item.Ref.Size); err != nil {
				return err
			}
		}
		return nil
	})
	return result, err
}

func (s *Store) ListDeleting(ctx context.Context) ([]domain.GCItem, error) {
	var result []domain.GCItem
	err := s.immediate(ctx, func(tx *immediateTx) error {
		// Older binaries allowed a new pin after a crashed GC left DELETING.
		// Reject such legacy rows before authorizing any filesystem removal.
		var unavailable bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM blobs WHERE gc_state = 'DELETING'
			AND (state <> 'READY' OR gc_removed_at IS NOT NULL OR NOT (`+unreferencedBlobSQL+`)))`).Scan(&unavailable); err != nil {
			return err
		}
		if unavailable {
			return wrap(ErrConsistency, "deleting blob is not unreferenced READY metadata", nil)
		}
		var err error
		result, err = listGarbage(ctx, tx, `gc_state = 'DELETING'`)
		return err
	})
	return result, err
}

func listGarbage(ctx context.Context, tx *immediateTx, predicate string) ([]domain.GCItem, error) {
	rows, err := tx.QueryContext(ctx, `SELECT digest, size, canonical_relative_path, publication_generation FROM blobs WHERE `+predicate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []domain.GCItem
	for rows.Next() {
		var item domain.GCItem
		if err := rows.Scan(&item.Ref.Digest, &item.Ref.Size, &item.CanonicalRelativePath, &item.PublicationGeneration); err != nil {
			return nil, err
		}
		if err := item.Ref.Validate(); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) CommitGarbage(ctx context.Context, item domain.GCItem, phase domain.GCCommit) error {
	if err := item.Ref.Validate(); err != nil {
		return err
	}
	if !phase.Valid() || item.PublicationGeneration <= 0 {
		return fmt.Errorf("invalid garbage commit phase or publication generation")
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		var state, gcState, path string
		var generation int64
		var removedAt sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT state, gc_state, canonical_relative_path, publication_generation, gc_removed_at FROM blobs WHERE digest = ? AND size = ?`, item.Ref.Digest, item.Ref.Size).Scan(&state, &gcState, &path, &generation, &removedAt); errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrConsistency, "garbage blob metadata is missing", err)
		} else if err != nil {
			return err
		}
		if path != item.CanonicalRelativePath || generation != item.PublicationGeneration {
			return wrap(ErrConsistency, "garbage item does not match blob publication", nil)
		}
		if phase == domain.GCCommitRemoved && removedAt.Valid {
			return nil // Same-generation commit replay after durable removal.
		}
		if gcState != "DELETING" || state != "READY" {
			return wrap(ErrConsistency, "garbage item does not match deleting READY blob", nil)
		}
		var result sql.Result
		var err error
		switch phase {
		case domain.GCCommitRemoved:
			now := s.clock.Now().UTC()
			if now.IsZero() {
				return errors.New("garbage removal clock returned zero time")
			}
			result, err = tx.ExecContext(ctx, `UPDATE blobs SET state = 'STAGING', gc_state = 'NONE', verified_at = NULL, gc_removed_at = ?
				WHERE digest = ? AND size = ? AND `+unreferencedBlobSQL, formatTime(now), item.Ref.Digest, item.Ref.Size)
		case domain.GCCommitRepair:
			result, err = tx.ExecContext(ctx, `UPDATE blobs SET gc_state = 'NONE' WHERE digest = ? AND size = ? AND `+unreferencedBlobSQL, item.Ref.Digest, item.Ref.Size)
		}
		if err != nil {
			return fmt.Errorf("commit garbage blob metadata: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return wrap(ErrConsistency, "garbage blob gained a retaining reference", nil)
		}
		return nil
	})
}
