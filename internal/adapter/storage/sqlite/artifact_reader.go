package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"cpgen/internal/domain"
)

// ReadArtifactWriter observes an existing writer without granting a new one.
// The declaration and token are read in the same SQLite snapshot.
func (s *Store) ReadArtifactWriter(ctx context.Context, id domain.ArtifactDeclarationID) (domain.ArtifactDeclarationRecord, domain.ArtifactWriterToken, error) {
	var declaration domain.ArtifactDeclarationRecord
	var token domain.ArtifactWriterToken
	if err := id.Validate(); err != nil {
		return declaration, token, err
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return declaration, token, err
	}
	defer tx.Rollback()
	declaration, err = readArtifactDeclaration(ctx, tx, id)
	if err != nil {
		return declaration, token, err
	}
	var state, created string
	var digest sql.NullString
	var size sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT writer_token_id, state, final_digest, final_size, pin_id, created_at FROM artifact_writer_tokens WHERE declaration_id = ?`, id).Scan(&token.ID, &state, &digest, &size, &token.PinID, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return declaration, token, wrap(ErrNotFound, "artifact writer token does not exist", err)
	}
	if err != nil {
		return declaration, token, err
	}
	token.DeclarationID, token.RunID, token.State = id, declaration.RunID, domain.ArtifactWriterState(state)
	if digest.Valid && size.Valid {
		token.Blob = &domain.BlobRef{Digest: domain.Digest(digest.String), Size: size.Int64}
	}
	token.CreatedAt, err = parseTime(created)
	if err != nil {
		return declaration, token, err
	}
	if err := token.Validate(); err != nil {
		return declaration, token, wrap(ErrConsistency, "artifact token is invalid", err)
	}
	return declaration, token, tx.Commit()
}

// ReadPendingArtifact restores attachment evidence from the writer/pin ledger.
// It returns only a finalized, retained, READY blob. Bytes still require a
// VerifiedBlobReader; a metadata row alone is never proof of content integrity.
func (s *Store) ReadPendingArtifact(ctx context.Context, id domain.ArtifactDeclarationID) (domain.PendingArtifact, error) {
	var pending domain.PendingArtifact
	if err := id.Validate(); err != nil {
		return pending, err
	}
	var provenance []byte
	err := s.db.QueryRowContext(ctx, `SELECT token.final_digest, token.final_size, decl.media_type, decl.role, decl.logical_path,
		decl.attempt_call_id, decl.reservation_id, token.writer_token_id, token.pin_id, pin.physical_new_bytes, decl.provenance_json
		FROM artifact_declarations decl JOIN artifact_writer_tokens token ON token.declaration_id = decl.declaration_id
		JOIN blob_pins pin ON pin.pin_id = token.pin_id AND pin.writer_token_id = token.writer_token_id
		JOIN blobs blob ON blob.digest = token.final_digest AND blob.size = token.final_size
		WHERE decl.declaration_id = ? AND token.state = 'FINALIZED' AND blob.state = 'READY'
		AND pin.digest = token.final_digest AND pin.size = token.final_size
		AND (pin.state = 'ACTIVE' OR EXISTS (SELECT 1 FROM artifact_occurrences occurrence WHERE occurrence.writer_token_id = token.writer_token_id))`, id).Scan(
		&pending.Blob.Digest, &pending.Blob.Size, &pending.MediaType, &pending.Role, &pending.LogicalPath, &pending.CallID, &pending.ReservationID, &pending.WriterTokenID, &pending.PinID, &pending.PhysicalNewBytes, &provenance)
	if errors.Is(err, sql.ErrNoRows) {
		return pending, wrap(ErrNotFound, "finalized retained artifact is unavailable", err)
	}
	if err != nil {
		return pending, err
	}
	if err := json.Unmarshal(provenance, &pending.Provenance); err != nil {
		return domain.PendingArtifact{}, wrap(ErrConsistency, "stored artifact provenance is invalid", err)
	}
	if err := pending.Validate(); err != nil {
		return domain.PendingArtifact{}, wrap(ErrConsistency, "pending artifact is invalid", err)
	}
	return pending, nil
}
