package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

var _ port.MutationRecordReader = (*Store)(nil)

// ReadMutationRecord uses one read snapshot and rechecks the original command
// hash after rebuilding ordered evidence. Historical records remain readable
// after stage advancement. This metadata does not replace verified Blob reads.
func (s *Store) ReadMutationRecord(ctx context.Context, grant domain.MutationGrant) (domain.MutationRecordRequest, error) {
	var empty domain.MutationRecordRequest
	if ctx == nil {
		return empty, errors.New("mutation record read requires a context")
	}
	if err := grant.Validate(); err != nil {
		return empty, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return empty, err
	}
	defer tx.Rollback()
	storedGrant, err := readMutationGrant(ctx, tx, grant.ClaimID)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, wrap(ErrNotFound, "mutation claim does not exist", err)
	}
	if err != nil {
		return empty, err
	}
	if storedGrant != grant {
		return empty, wrap(ErrConsistency, "mutation record grant differs from immutable claim", nil)
	}
	result := domain.MutationRecordRequest{Grant: storedGrant}
	recordGrant := storedGrant
	var commandDigest domain.Digest
	var created string
	err = tx.QueryRowContext(ctx, `SELECT record_id,command_digest,created_at,run_id,stage_name,scope_digest,source_batch_digest,ordinal,kind,intent_digest
		FROM mutation_records WHERE claim_id=?`, grant.ClaimID).Scan(&result.RecordID, &commandDigest, &created,
		&recordGrant.RunID, &recordGrant.StageName, &recordGrant.ScopeDigest, &recordGrant.SourceBatchDigest,
		&recordGrant.Ordinal, &recordGrant.Kind, &recordGrant.IntentDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return empty, wrap(ErrNotFound, "mutation has no committed result record", err)
	}
	if err != nil {
		return empty, err
	}
	if recordGrant != storedGrant {
		return empty, wrap(ErrConsistency, "mutation result identity differs from its immutable claim", nil)
	}
	result.At, err = parseTime(created)
	if err != nil {
		return empty, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT operation_ordinal,call_record_id,attempt_id FROM mutation_record_operations WHERE record_id=? ORDER BY operation_ordinal LIMIT 65`, result.RecordID)
	if err != nil {
		return empty, err
	}
	for rows.Next() {
		var ordinal int
		var operation domain.MutationOperation
		if err := rows.Scan(&ordinal, &operation.CallRecordID, &operation.AttemptID); err != nil {
			_ = rows.Close()
			return empty, err
		}
		if ordinal != len(result.Operations)+1 {
			_ = rows.Close()
			return empty, wrap(ErrConsistency, "mutation operation evidence order is incomplete", nil)
		}
		result.Operations = append(result.Operations, operation)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return empty, err
	}
	rows, err = tx.QueryContext(ctx, `SELECT reservation_ordinal,reservation_id,call_record_id,attempt_call_id FROM mutation_record_reservations WHERE record_id=? ORDER BY reservation_ordinal LIMIT 513`, result.RecordID)
	if err != nil {
		return empty, err
	}
	for rows.Next() {
		var ordinal int
		var reservation domain.MutationReservation
		if err := rows.Scan(&ordinal, &reservation.ReservationID, &reservation.CallRecordID, &reservation.AttemptCallID); err != nil {
			_ = rows.Close()
			return empty, err
		}
		if ordinal != len(result.Reservations)+1 {
			_ = rows.Close()
			return empty, wrap(ErrConsistency, "mutation reservation evidence order is incomplete", nil)
		}
		result.Reservations = append(result.Reservations, reservation)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return empty, err
	}
	if err := tx.QueryRowContext(ctx, `SELECT occurrence_id FROM mutation_record_output_occurrences WHERE record_id=? AND run_id=? AND stage_name=?`,
		result.RecordID, grant.RunID, grant.StageName).Scan(&result.OutputOccurrenceID); err != nil {
		return empty, wrap(ErrConsistency, "mutation result has no bound output occurrence", err)
	}
	if err := result.Validate(); err != nil {
		return empty, wrap(ErrConsistency, "mutation result evidence is invalid", err)
	}
	digest, _, err := digestJSON(result)
	if err != nil {
		return empty, err
	}
	if digest != commandDigest {
		return empty, wrap(ErrConsistency, "mutation result differs from the original immutable command", nil)
	}
	if err := tx.Commit(); err != nil {
		return empty, err
	}
	return result, nil
}
