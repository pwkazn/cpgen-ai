package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"cpgen/internal/domain"
)

// QuarantineBlob records corruption as one short durable transition.  The
// intermediate QUARANTINING state is committed in the same transaction as
// CORRUPT so a reader can never observe a READY blob after this method
// returns, while the filesystem quarantine remains an independent I/O step.
func (s *Store) QuarantineBlob(ctx context.Context, ref domain.BlobRef) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		var state, gcState string
		var removedAt sql.NullString
		if err := tx.QueryRowContext(ctx, `SELECT state, gc_state, gc_removed_at FROM blobs WHERE digest = ? AND size = ?`, ref.Digest, ref.Size).Scan(&state, &gcState, &removedAt); errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "artifact blob does not exist", err)
		} else if err != nil {
			return err
		}
		if gcState != "NONE" || removedAt.Valid {
			return wrap(ErrConsistency, "garbage blob cannot be quarantined", nil)
		}
		if state == "CORRUPT" {
			return nil
		}
		if state != "READY" && state != "STAGING" && state != "QUARANTINING" {
			return wrap(ErrConsistency, fmt.Sprintf("artifact blob is %s, not quarantineable", state), nil)
		}
		if state == "STAGING" || state == "READY" {
			if _, err := tx.ExecContext(ctx, `UPDATE blobs SET state = 'QUARANTINING' WHERE digest = ? AND size = ? AND state IN ('STAGING','READY')`, ref.Digest, ref.Size); err != nil {
				return err
			}
		}
		result, err := tx.ExecContext(ctx, `UPDATE blobs SET state = 'CORRUPT' WHERE digest = ? AND size = ? AND state = 'QUARANTINING'`, ref.Digest, ref.Size)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return wrap(ErrConsistency, "artifact blob corruption transition was lost", nil)
		}
		return nil
	})
}
