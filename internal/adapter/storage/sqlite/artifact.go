package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"cpgen/internal/domain"
)

func attachPendingOccurrences(ctx context.Context, tx *immediateTx, command domain.FinishStageCommand) error {
	for _, pending := range command.Occurrences {
		if err := pending.Validate(); err != nil {
			return err
		}
		switch pending.Kind {
		case domain.PendingOccurrenceNewWrite:
			if err := attachNewWriteOccurrence(ctx, tx, command, *pending.NewWrite); err != nil {
				return err
			}
		case domain.PendingOccurrenceCacheReuse:
			if err := attachCacheReuseOccurrence(ctx, tx, command, *pending.CacheReuse); err != nil {
				return err
			}
		}
	}
	return nil
}

func attachNewWriteOccurrence(ctx context.Context, tx *immediateTx, command domain.FinishStageCommand, pending domain.PendingArtifact) error {
	var callRecordID, runID, stageName, attemptID, physicalKind string
	if err := tx.QueryRowContext(ctx, `SELECT call_record_id, run_id, stage_name, attempt_id, physical_kind FROM physical_calls WHERE attempt_call_id = ?`, pending.CallID).Scan(&callRecordID, &runID, &stageName, &attemptID, &physicalKind); errors.Is(err, sql.ErrNoRows) {
		return wrap(ErrNotFound, "artifact physical call does not exist", err)
	} else if err != nil {
		return err
	}
	if runID != string(command.RunID) || stageName != string(command.StageName) || attemptID != string(command.AttemptID) || physicalKind != string(domain.PhysicalLocalArtifactWrite) {
		return wrap(ErrConsistency, "artifact physical call is outside the finishing attempt", nil)
	}
	var declarationID, tokenRun, tokenState, finalDigest, pinID, pinState, declarationCallRecordID, declarationAttemptCallID, role, media, path, reservationID, reservationDimension, reservationSubkey string
	var finalSize, physicalNewBytes int64
	if err := tx.QueryRowContext(ctx, `SELECT token.declaration_id, token.run_id, token.state, token.final_digest, token.final_size,
			token.pin_id, pin.state, pin.physical_new_bytes, decl.call_record_id, decl.attempt_call_id, decl.role, decl.media_type, decl.logical_path, decl.reservation_id, decl.reservation_dimension, decl.reservation_subkey
			FROM artifact_writer_tokens token JOIN artifact_declarations decl ON decl.declaration_id = token.declaration_id
			JOIN blob_pins pin ON pin.pin_id = token.pin_id
			WHERE token.writer_token_id = ?`, pending.WriterTokenID).Scan(&declarationID, &tokenRun, &tokenState, &finalDigest, &finalSize, &pinID, &pinState, &physicalNewBytes, &declarationCallRecordID, &declarationAttemptCallID, &role, &media, &path, &reservationID, &reservationDimension, &reservationSubkey); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "artifact writer token does not exist", err)
		}
		return err
	}
	if callRecordID != declarationCallRecordID || string(pending.CallID) != declarationAttemptCallID || tokenRun != string(command.RunID) || tokenState != string(domain.ArtifactWriterFinalized) || finalDigest != string(pending.Blob.Digest) || finalSize != pending.Blob.Size || pinID != string(pending.PinID) || pinState != "ACTIVE" || declarationID == "" || role != string(pending.Role) || media != pending.MediaType || path != string(pending.LogicalPath) || reservationID != string(pending.ReservationID) || reservationDimension != string(domain.BudgetArtifactPhysicalNewBytes) || reservationSubkey == "" {
		return wrap(ErrConsistency, "new artifact occurrence bindings do not match finalized token", nil)
	}
	var occurrenceID string
	generated, err := domain.NewID("occurrence")
	if err != nil {
		return err
	}
	occurrenceID = generated
	payload, err := json.Marshal(pending.Provenance)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_occurrences(occurrence_id, kind, run_id, stage_name, attempt_id, current_call_record_id,
		declaration_id, writer_token_id, reservation_id, pin_id, digest, size, role, logical_path, media_type, provenance_json, provenance_digest, created_at)
		VALUES (?, 'NEW_WRITE', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, occurrenceID, command.RunID, command.StageName, command.AttemptID, callRecordID,
		declarationID, pending.WriterTokenID, pending.ReservationID, pending.PinID, pending.Blob.Digest, pending.Blob.Size, pending.Role, pending.LogicalPath, pending.MediaType, payload, domain.SumBytes(payload), formatTime(command.At)); err != nil {
		return fmt.Errorf("insert new artifact occurrence: %w", err)
	}
	if err := settleArtifactReservation(ctx, tx, pending.ReservationID, physicalNewBytes, command.At); err != nil {
		return err
	}
	return movePinToReleasable(ctx, tx, pending.PinID, command.At)
}

func attachCacheReuseOccurrence(ctx context.Context, tx *immediateTx, command domain.FinishStageCommand, pending domain.PendingCacheReuse) error {
	var callRun, callStage, callAttempt, dispatch string
	if err := tx.QueryRowContext(ctx, `SELECT run_id, stage_name, attempt_id, COALESCE(dispatch_kind, '') FROM call_records WHERE call_record_id = ?`, pending.CurrentCallRecordID).Scan(&callRun, &callStage, &callAttempt, &dispatch); errors.Is(err, sql.ErrNoRows) {
		return wrap(ErrNotFound, "cache-hit logical call does not exist", err)
	} else if err != nil {
		return err
	}
	if callRun != string(command.RunID) || callStage != string(command.StageName) || callAttempt != string(command.AttemptID) || dispatch != string(domain.DispatchCacheHit) {
		return wrap(ErrConsistency, "cache reuse current call is not a cache hit in this attempt", nil)
	}
	var occurrenceID string
	generated, err := domain.NewID("occurrence")
	if err != nil {
		return err
	}
	occurrenceID = generated
	payload, err := json.Marshal(pending.Provenance)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_occurrences(occurrence_id, kind, run_id, stage_name, attempt_id, current_call_record_id,
		source_occurrence_id, cache_reuse_record_id, source_call_record_id, digest, size, role, logical_path, media_type, provenance_json, provenance_digest, created_at)
		VALUES (?, 'CACHE_REUSE', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, occurrenceID, command.RunID, command.StageName, command.AttemptID,
		pending.CurrentCallRecordID, pending.SourceOccurrenceID, pending.CacheReuseRecordID, pending.SourceCallRecordID, pending.Blob.Digest, pending.Blob.Size, pending.Role, pending.LogicalPath, pending.MediaType, payload, domain.SumBytes(payload), formatTime(command.At)); err != nil {
		return fmt.Errorf("insert cache reuse occurrence: %w", err)
	}
	return nil
}

func settleArtifactReservation(ctx context.Context, tx *immediateTx, id domain.ReservationID, value int64, at time.Time) error {
	if value < 0 {
		return errors.New("artifact physical bytes must not be negative")
	}
	var runID, dimension, state string
	var upper int64
	if err := tx.QueryRowContext(ctx, `SELECT run_id, dimension, upper_bound, state FROM budget_reservations WHERE reservation_id = ?`, id).Scan(&runID, &dimension, &upper, &state); err != nil {
		return err
	}
	if dimension != string(domain.BudgetArtifactPhysicalNewBytes) || state != string(domain.ReservationReserved) || value > upper {
		return wrap(ErrConsistency, "artifact reservation is not available for settlement", nil)
	}
	result, err := tx.ExecContext(ctx, `UPDATE budget_accounts SET reserved_value = reserved_value - ?, consumed_value = consumed_value + ?, account_version = account_version + 1 WHERE run_id = ? AND dimension = ? AND reserved_value >= ? AND ? <= limit_value - consumed_value`, upper, value, runID, dimension, upper, value)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return wrap(ErrConsistency, "artifact reservation cannot settle against its account", nil)
	}
	_, err = tx.ExecContext(ctx, `UPDATE budget_reservations SET state = 'SETTLED', settled_value = ?, settled_at = ? WHERE reservation_id = ? AND state = 'RESERVED'`, value, formatTime(at), id)
	return err
}

func movePinToReleasable(ctx context.Context, tx *immediateTx, id domain.BlobPinID, at time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE blob_pins SET state = 'RELEASABLE' WHERE pin_id = ? AND state = 'ACTIVE'`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return wrap(ErrConsistency, "artifact pin is not active", nil)
	}
	var ordinal int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(ordinal), 0) FROM blob_pin_history WHERE pin_id = ?`, id).Scan(&ordinal); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO blob_pin_history(pin_id, ordinal, state, changed_at) VALUES (?, ?, 'RELEASABLE', ?)`, id, ordinal+1, formatTime(at))
	return err
}

func releasePendingArtifactTokens(ctx context.Context, tx *immediateTx, runID domain.RunID, stage domain.StageName, attempt domain.AttemptID, at time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT token.writer_token_id FROM artifact_writer_tokens token JOIN artifact_declarations decl ON decl.declaration_id = token.declaration_id WHERE token.run_id = ? AND decl.stage_name = ? AND decl.attempt_id = ? AND token.state IN ('PREPARED','OPEN','SEALED','FINALIZED') AND NOT (token.state = 'FINALIZED' AND EXISTS (SELECT 1 FROM artifact_occurrences occurrence WHERE occurrence.writer_token_id = token.writer_token_id))`, runID, stage, attempt)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		var state, pin string
		if err := tx.QueryRowContext(ctx, `SELECT state, pin_id FROM artifact_writer_tokens WHERE writer_token_id = ?`, id).Scan(&state, &pin); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE artifact_writer_tokens SET state = 'RELEASED', released_at = ? WHERE writer_token_id = ? AND state <> 'RELEASED'`, formatTime(at), id); err != nil {
			return err
		}
		if state == string(domain.ArtifactWriterSealed) || state == string(domain.ArtifactWriterFinalized) {
			if _, err := tx.ExecContext(ctx, `UPDATE blob_pins SET state = 'RELEASED', released_at = ? WHERE pin_id = ? AND state IN ('ACTIVE','RELEASABLE')`, formatTime(at), pin); err != nil {
				return err
			}
			var ordinal int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(ordinal), 0) FROM blob_pin_history WHERE pin_id = ?`, pin).Scan(&ordinal); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO blob_pin_history(pin_id, ordinal, state, changed_at) VALUES (?, ?, 'RELEASED', ?)`, pin, ordinal+1, formatTime(at)); err != nil {
				return err
			}
		}
	}
	return nil
}

// CreateArtifactDeclaration persists the immutable binding between a local
// artifact write and its already-prepared physical call/reservation.
func (s *Store) CreateArtifactDeclaration(ctx context.Context, declaration domain.ArtifactDeclarationRecord) error {
	if err := declaration.Validate(); err != nil {
		return err
	}
	payload, err := json.Marshal(domain.ProvenanceCandidate{SchemaVersion: declaration.Provenance.SchemaVersion, Producer: declaration.Provenance.Producer, InputDigest: declaration.Provenance.InputDigest})
	if err != nil {
		return err
	}
	declarationDigest := domain.SumBytes(payload)
	now := s.clock.Now().UTC()
	if now.IsZero() {
		return errors.New("artifact declaration clock returned zero time")
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO artifact_declarations(
			declaration_id, run_id, stage_name, attempt_id, call_record_id, attempt_call_id, physical_kind,
			reservation_id, reservation_dimension, reservation_subkey, role, media_type, logical_path, max_bytes,
			provenance_json, declaration_digest, created_at)
			VALUES (?, ?, ?, ?, ?, ?, 'LOCAL_ARTIFACT_WRITE', ?, 'ARTIFACT_PHYSICAL_NEW_BYTES', ?, ?, ?, ?, ?, ?, ?, ?)`,
			declaration.ID, declaration.RunID, declaration.StageName, declaration.AttemptID, declaration.CallRecordID,
			declaration.AttemptCallID, declaration.ReservationID, declaration.ReservationSubkey, declaration.Role,
			declaration.MediaType, declaration.LogicalPath, declaration.MaxBytes, payload, declarationDigest, formatTime(now))
		if err != nil {
			return fmt.Errorf("insert artifact declaration: %w", err)
		}
		return nil
	})
}

// PrepareArtifact claims (or replays) the single writer token for a
// declaration. It does not reserve calls or budget; those are fixed by Task 4
// before this capability is handed to a step.
func (s *Store) PrepareArtifact(ctx context.Context, id domain.ArtifactDeclarationID) (domain.ArtifactDeclarationRecord, domain.ArtifactWriterToken, error) {
	if err := id.Validate(); err != nil {
		return domain.ArtifactDeclarationRecord{}, domain.ArtifactWriterToken{}, err
	}
	var declaration domain.ArtifactDeclarationRecord
	var token domain.ArtifactWriterToken
	err := s.immediate(ctx, func(tx *immediateTx) error {
		var err error
		declaration, err = readArtifactDeclaration(ctx, tx, id)
		if err != nil {
			return err
		}
		var state, created, opened, sealed, finalized, released string
		var finalDigest sql.NullString
		var finalSize sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT writer_token_id, state, final_digest, final_size, pin_id, created_at,
			opened_at, sealed_at, finalized_at, released_at FROM artifact_writer_tokens WHERE declaration_id = ?`, id).Scan(
			&token.ID, &state, &finalDigest, &finalSize, &token.PinID, &created, &opened, &sealed, &finalized, &released)
		if errors.Is(err, sql.ErrNoRows) {
			writerID, genErr := domain.NewID("writer")
			if genErr != nil {
				return genErr
			}
			pinID, genErr := domain.NewID("pin")
			if genErr != nil {
				return genErr
			}
			now := s.clock.Now().UTC()
			if now.IsZero() {
				return errors.New("artifact token clock returned zero time")
			}
			_, err = tx.ExecContext(ctx, `INSERT INTO artifact_writer_tokens(writer_token_id, declaration_id, run_id, state, pin_id, created_at)
				VALUES (?, ?, ?, 'PREPARED', ?, ?)`, writerID, id, declaration.RunID, pinID, formatTime(now))
			if err != nil {
				return fmt.Errorf("insert artifact writer token: %w", err)
			}
			token.ID, token.DeclarationID, token.RunID, token.State, token.PinID, token.CreatedAt = domain.ArtifactWriterTokenID(writerID), id, declaration.RunID, domain.ArtifactWriterPrepared, domain.BlobPinID(pinID), now
			return nil
		}
		if err != nil {
			return err
		}
		token.DeclarationID, token.RunID, token.State = id, declaration.RunID, domain.ArtifactWriterState(state)
		if finalDigest.Valid && finalSize.Valid {
			token.Blob = &domain.BlobRef{Digest: domain.Digest(finalDigest.String), Size: finalSize.Int64}
		}
		token.CreatedAt, err = parseTime(created)
		return err
	})
	return declaration, token, err
}

func (s *Store) OpenArtifactWriter(ctx context.Context, id domain.ArtifactWriterTokenID) error {
	if err := id.Validate(); err != nil {
		return err
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		now := s.clock.Now().UTC()
		if now.IsZero() {
			return errors.New("artifact token clock returned zero time")
		}
		result, err := tx.ExecContext(ctx, `UPDATE artifact_writer_tokens SET state = 'OPEN', opened_at = ? WHERE writer_token_id = ? AND state = 'PREPARED'`, formatTime(now), id)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count == 1 {
			return nil
		}
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT state FROM artifact_writer_tokens WHERE writer_token_id = ?`, id).Scan(&state); errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "artifact writer token does not exist", err)
		} else if err != nil {
			return err
		}
		return wrap(ErrInvalidTransition, "artifact writer token is not PREPARED", nil)
	})
}

func (s *Store) SealArtifact(ctx context.Context, id domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		var declarationID, runID, state, pinID string
		var finalDigest sql.NullString
		var finalSize sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT declaration_id, run_id, state, pin_id, final_digest, final_size FROM artifact_writer_tokens WHERE writer_token_id = ?`, id).Scan(&declarationID, &runID, &state, &pinID, &finalDigest, &finalSize); errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "artifact writer token does not exist", err)
		} else if err != nil {
			return err
		} else if (state == string(domain.ArtifactWriterSealed) || state == string(domain.ArtifactWriterFinalized)) && finalDigest.Valid && finalDigest.String == string(ref.Digest) && finalSize.Valid && finalSize.Int64 == ref.Size {
			// A restart may replay SealArtifact after the SEALED transaction
			// committed but before publication.  The existing pin is the durable
			// ownership record; do not create a second pin.
			var pinDigest string
			var pinSize int64
			if err := tx.QueryRowContext(ctx, `SELECT digest, size FROM blob_pins WHERE pin_id = ?`, pinID).Scan(&pinDigest, &pinSize); err != nil {
				return wrap(ErrConsistency, "SEALED artifact writer pin is missing", err)
			}
			if pinDigest != string(ref.Digest) || pinSize != ref.Size {
				return wrap(ErrConsistency, "SEALED artifact writer pin does not match blob", nil)
			}
			return nil
		} else if state != string(domain.ArtifactWriterOpen) {
			return wrap(ErrInvalidTransition, "artifact writer token is not OPEN", nil)
		}
		now := s.clock.Now().UTC()
		if now.IsZero() {
			return errors.New("artifact token clock returned zero time")
		}
		canonical := canonicalRelativePath(ref)
		var priorState string
		priorErr := tx.QueryRowContext(ctx, `SELECT state FROM blobs WHERE digest = ? AND size = ?`, ref.Digest, ref.Size).Scan(&priorState)
		if priorErr != nil && !errors.Is(priorErr, sql.ErrNoRows) {
			return priorErr
		}
		physicalNewBytes := int64(0)
		blobWasNew := errors.Is(priorErr, sql.ErrNoRows)
		if blobWasNew {
			physicalNewBytes = ref.Size
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO blobs(digest, size, state, canonical_relative_path)
			VALUES (?, ?, 'STAGING', ?) ON CONFLICT(digest, size) DO UPDATE SET state = CASE WHEN blobs.state = 'READY' THEN 'READY' ELSE 'STAGING' END`, ref.Digest, ref.Size, canonical)
		if err != nil {
			return fmt.Errorf("stage blob ledger row: %w", err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE artifact_writer_tokens SET state = 'SEALED', final_digest = ?, final_size = ?, sealed_at = ? WHERE writer_token_id = ? AND state = 'OPEN'`, ref.Digest, ref.Size, formatTime(now), id)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO blob_pins(pin_id, writer_token_id, digest, size, physical_new_bytes, state, created_at) VALUES (?, ?, ?, ?, ?, 'ACTIVE', ?)`, pinID, id, ref.Digest, ref.Size, physicalNewBytes, formatTime(now)); err != nil {
			return fmt.Errorf("create blob pin: %w", err)
		}
		if blobWasNew {
			// The first pin created for a canonical blob is the immutable
			// publication owner. Keep this identity separate from wall-clock
			// timestamps so historical ties and future replay cannot reassign
			// physical byte ownership.
			if _, err := tx.ExecContext(ctx, `INSERT INTO artifact_blob_publication_owners(digest, size, pin_id)
				VALUES (?, ?, ?) ON CONFLICT(digest, size) DO NOTHING`, ref.Digest, ref.Size, pinID); err != nil {
				return fmt.Errorf("record blob publication owner: %w", err)
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO blob_pin_history(pin_id, ordinal, state, changed_at) VALUES (?, 1, 'ACTIVE', ?)`, pinID, formatTime(now))
		return err
	})
}

func (s *Store) FinalizeArtifact(ctx context.Context, id domain.ArtifactWriterTokenID, ref domain.BlobRef) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		now := s.clock.Now().UTC()
		if now.IsZero() {
			return errors.New("artifact token clock returned zero time")
		}
		var state string
		var finalDigest sql.NullString
		var finalSize sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT state, final_digest, final_size FROM artifact_writer_tokens WHERE writer_token_id = ?`, id).Scan(&state, &finalDigest, &finalSize); errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "artifact writer token does not exist", err)
		} else if err != nil {
			return err
		}
		if state == string(domain.ArtifactWriterFinalized) && finalDigest.Valid && finalDigest.String == string(ref.Digest) && finalSize.Valid && finalSize.Int64 == ref.Size {
			return nil
		}
		result, err := tx.ExecContext(ctx, `UPDATE blobs SET state = 'READY', verified_at = ? WHERE digest = ? AND size = ? AND state IN ('STAGING','READY')`, formatTime(now), ref.Digest, ref.Size)
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return wrap(ErrConsistency, "artifact blob is not staged", nil)
		}
		result, err = tx.ExecContext(ctx, `UPDATE artifact_writer_tokens SET state = 'FINALIZED', finalized_at = ? WHERE writer_token_id = ? AND state = 'SEALED' AND final_digest = ? AND final_size = ?`, formatTime(now), id, ref.Digest, ref.Size)
		if err != nil {
			return err
		}
		count, err = result.RowsAffected()
		if err != nil {
			return err
		}
		if count != 1 {
			return wrap(ErrInvalidTransition, "artifact writer token is not SEALED for this blob", nil)
		}
		return nil
	})
}

func (s *Store) ReleaseArtifact(ctx context.Context, id domain.ArtifactWriterTokenID) error {
	if err := id.Validate(); err != nil {
		return err
	}
	return s.immediate(ctx, func(tx *immediateTx) error {
		now := s.clock.Now().UTC()
		if now.IsZero() {
			return errors.New("artifact token clock returned zero time")
		}
		var state, pinID string
		if err := tx.QueryRowContext(ctx, `SELECT state, pin_id FROM artifact_writer_tokens WHERE writer_token_id = ?`, id).Scan(&state, &pinID); errors.Is(err, sql.ErrNoRows) {
			return wrap(ErrNotFound, "artifact writer token does not exist", err)
		} else if err != nil {
			return err
		}
		if state == string(domain.ArtifactWriterReleased) {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE artifact_writer_tokens SET state = 'RELEASED', released_at = ? WHERE writer_token_id = ?`, formatTime(now), id); err != nil {
			return err
		}
		// A pin exists only after SEALED. PREPARED/OPEN release simply closes
		// the token and leaves no durable blob reference.
		if state == string(domain.ArtifactWriterSealed) || state == string(domain.ArtifactWriterFinalized) {
			if _, err := tx.ExecContext(ctx, `UPDATE blob_pins SET state = 'RELEASED', released_at = ? WHERE pin_id = ? AND state IN ('ACTIVE','RELEASABLE')`, formatTime(now), pinID); err != nil {
				return err
			}
			var ordinal int
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(max(ordinal), 0) FROM blob_pin_history WHERE pin_id = ?`, pinID).Scan(&ordinal); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, `INSERT INTO blob_pin_history(pin_id, ordinal, state, changed_at) VALUES (?, ?, 'RELEASED', ?)`, pinID, ordinal+1, formatTime(now))
			return err
		}
		return nil
	})
}

func readArtifactDeclaration(ctx context.Context, queryer rowQuerier, id domain.ArtifactDeclarationID) (domain.ArtifactDeclarationRecord, error) {
	var result domain.ArtifactDeclarationRecord
	var role, path, payload, created string
	if err := queryer.QueryRowContext(ctx, `SELECT declaration_id, run_id, stage_name, attempt_id, call_record_id, attempt_call_id,
		reservation_id, reservation_subkey, media_type, role, logical_path, max_bytes, provenance_json, created_at FROM artifact_declarations WHERE declaration_id = ?`, id).Scan(
		&result.ID, &result.RunID, &result.StageName, &result.AttemptID, &result.CallRecordID, &result.AttemptCallID, &result.ReservationID,
		&result.ReservationSubkey, &result.MediaType, &role, &path, &result.MaxBytes, &payload, &created); errors.Is(err, sql.ErrNoRows) {
		return domain.ArtifactDeclarationRecord{}, wrap(ErrNotFound, "artifact declaration does not exist", err)
	} else if err != nil {
		return domain.ArtifactDeclarationRecord{}, err
	}
	result.Role, result.LogicalPath = domain.ArtifactRole(role), domain.SafeRelPath(path)
	if err := json.Unmarshal([]byte(payload), &result.Provenance); err != nil {
		return domain.ArtifactDeclarationRecord{}, wrap(ErrConsistency, "stored artifact provenance is invalid", err)
	}
	parsedCreated, err := parseTime(created)
	if err != nil {
		return domain.ArtifactDeclarationRecord{}, err
	}
	result.CreatedAt = parsedCreated
	if err := result.Validate(); err != nil {
		return domain.ArtifactDeclarationRecord{}, wrap(ErrConsistency, "stored artifact declaration is invalid", err)
	}
	return result, nil
}

func canonicalRelativePath(ref domain.BlobRef) string {
	hex := strings.TrimPrefix(string(ref.Digest), "sha256:")
	return filepathJoinSlash("blobs", "sha256", hex[:2], hex)
}

func filepathJoinSlash(parts ...string) string { return strings.Join(parts, "/") }
