package artifact

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type PreparedArtifactSession interface {
	Prepare(context.Context, domain.ArtifactDeclarationID) (port.ArtifactWriter, error)
	RestartUnsealed(context.Context, domain.ArtifactDeclarationID) (port.ArtifactWriter, error)
	ReleaseUnused(context.Context) error
}

type preparedArtifactSession struct {
	mu       sync.Mutex
	ledger   port.ArtifactLedger
	store    *blob.Store
	prepared domain.PreparedCalls
	used     map[domain.ArtifactDeclarationID]struct{}
	writers  map[domain.ArtifactWriterTokenID]*sessionWriter
}

func NewPreparedArtifactSession(ledger port.ArtifactLedger, store *blob.Store, prepared domain.PreparedCalls) (PreparedArtifactSession, error) {
	if ledger == nil || store == nil {
		return nil, errors.New("artifact session dependencies are required")
	}
	if corruption, ok := ledger.(blob.CorruptionLedger); ok {
		if err := store.AttachCorruptionLedger(corruption); err != nil {
			return nil, err
		}
	}
	if err := prepared.Validate(); err != nil {
		return nil, fmt.Errorf("prepared artifact calls: %w", err)
	}
	if prepared.Call.State == domain.CallRecordTerminal {
		return nil, errors.New("artifact session cannot use a terminal call")
	}
	return &preparedArtifactSession{ledger: ledger, store: store, prepared: clonePreparedCalls(prepared), used: map[domain.ArtifactDeclarationID]struct{}{}, writers: map[domain.ArtifactWriterTokenID]*sessionWriter{}}, nil
}

func (s *preparedArtifactSession) Prepare(ctx context.Context, id domain.ArtifactDeclarationID) (port.ArtifactWriter, error) {
	return s.prepare(ctx, id, false)
}

func (s *preparedArtifactSession) RestartUnsealed(ctx context.Context, id domain.ArtifactDeclarationID) (port.ArtifactWriter, error) {
	return s.prepare(ctx, id, true)
}

func (s *preparedArtifactSession) prepare(ctx context.Context, id domain.ArtifactDeclarationID, restart bool) (port.ArtifactWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := id.Validate(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.used[id]; exists {
		return nil, errors.New("artifact declaration was already consumed")
	}
	declaration, token, err := s.ledger.PrepareArtifact(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := declaration.Validate(); err != nil {
		return nil, err
	}
	if err := tokenIdentity(token); err != nil {
		return nil, err
	}
	if err := s.validatePreparedBinding(declaration, token, restart); err != nil {
		return nil, err
	}
	if token.State == domain.ArtifactWriterPrepared {
		if err := s.ledger.OpenArtifactWriter(ctx, token.ID); err != nil {
			return nil, err
		}
	} else if token.State != domain.ArtifactWriterSealed && !(restart && token.State == domain.ArtifactWriterOpen) {
		return nil, errors.New("artifact declaration writer token is not reusable")
	}
	identity := blob.WriterIdentity{CallID: declaration.AttemptCallID, ReservationID: declaration.ReservationID, WriterTokenID: token.ID, PinID: token.PinID}
	artifact := port.ArtifactDeclaration{MediaType: declaration.MediaType, Role: declaration.Role, LogicalPath: declaration.LogicalPath, MaxBytes: declaration.MaxBytes, Provenance: declaration.Provenance}
	var inner port.ArtifactWriter
	if token.State == domain.ArtifactWriterSealed {
		if token.Blob == nil {
			return nil, errors.New("SEALED artifact writer token has no blob")
		}
		inner, err = s.store.ResumeStaged(ctx, artifact, identity, *token.Blob)
	} else if restart && token.State == domain.ArtifactWriterOpen {
		inner, err = s.store.RestartUnsealed(ctx, artifact, identity)
	} else {
		inner, err = s.store.Prepare(ctx, artifact, identity)
	}
	if err != nil {
		// A failed recovery does not revoke the durable SEALED publication.
		// Keep its pin and staged bytes available for a later retry.
		if token.State != domain.ArtifactWriterSealed {
			_ = s.ledger.ReleaseArtifact(context.Background(), token.ID)
		}
		return nil, err
	}
	wrapper := &sessionWriter{inner: inner, ledger: s.ledger, tokenID: token.ID, sealed: token.State == domain.ArtifactWriterSealed}
	s.used[id] = struct{}{}
	s.writers[token.ID] = wrapper
	return wrapper, nil
}

func (s *preparedArtifactSession) validatePreparedBinding(declaration domain.ArtifactDeclarationRecord, token domain.ArtifactWriterToken, restart bool) error {
	var foundPhysical *domain.PhysicalCall
	for index := range s.prepared.PhysicalCalls {
		if s.prepared.PhysicalCalls[index].ID == declaration.AttemptCallID {
			foundPhysical = &s.prepared.PhysicalCalls[index]
			break
		}
	}
	if foundPhysical == nil || foundPhysical.Kind != domain.PhysicalLocalArtifactWrite {
		return errors.New("artifact declaration is not bound to a prepared local artifact call")
	}
	if foundPhysical.State != domain.PhysicalPrepared && !((token.State == domain.ArtifactWriterSealed || (restart && token.State == domain.ArtifactWriterOpen)) && (foundPhysical.State == domain.PhysicalDispatching || foundPhysical.State == domain.PhysicalSent)) {
		return errors.New("artifact recovery requires a sealed dispatched writer")
	}
	for _, reservation := range s.prepared.Reservations {
		if reservation.ID == declaration.ReservationID {
			if reservation.AttemptCallID != declaration.AttemptCallID || reservation.Dimension != domain.BudgetArtifactPhysicalNewBytes || reservation.State != domain.ReservationReserved {
				return errors.New("artifact declaration reservation binding is invalid")
			}
			return nil
		}
	}
	return errors.New("artifact declaration reservation is outside prepared calls")
}

func (s *preparedArtifactSession) ReleaseUnused(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var joined error
	for id, writer := range s.writers {
		if writer.isFinalized() {
			continue
		}
		if err := writer.abortIfOpen(ctx); err != nil {
			joined = errors.Join(joined, err)
			continue
		}
		if err := s.ledger.ReleaseArtifact(ctx, id); err != nil {
			joined = errors.Join(joined, err)
		}
	}
	return joined
}

func clonePreparedCalls(value domain.PreparedCalls) domain.PreparedCalls {
	value.PhysicalCalls = append([]domain.PhysicalCall(nil), value.PhysicalCalls...)
	value.Reservations = append([]domain.BudgetReservation(nil), value.Reservations...)
	if value.CallTrace != nil {
		trace := *value.CallTrace
		trace.PhysicalAttemptCallIDs = append([]domain.AttemptCallID(nil), trace.PhysicalAttemptCallIDs...)
		value.CallTrace = &trace
	}
	if value.Failure != nil {
		failure := *value.Failure
		value.Failure = &failure
	}
	return value
}

type sessionWriter struct {
	mu      sync.Mutex
	inner   port.ArtifactWriter
	ledger  port.ArtifactLedger
	tokenID domain.ArtifactWriterTokenID
	ref     domain.BlobRef
	closed  bool
	sealed  bool
}

func tokenIdentity(token domain.ArtifactWriterToken) error {
	if err := token.ID.Validate(); err != nil {
		return err
	}
	if err := token.DeclarationID.Validate(); err != nil {
		return err
	}
	if err := token.RunID.Validate(); err != nil {
		return err
	}
	if err := token.PinID.Validate(); err != nil {
		return err
	}
	if !token.State.Valid() {
		return errors.New("artifact writer token state is invalid")
	}
	return nil
}

func (w *sessionWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return 0, errors.New("artifact writer is terminal")
	}
	return w.inner.Write(p)
}

func (w *sessionWriter) Finalize(ctx context.Context) (domain.PendingArtifact, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return domain.PendingArtifact{}, errors.New("artifact writer is already terminal")
	}
	pending, err := blob.FinalizeWithLedger(ctx, w.inner,
		func(callbackCtx context.Context, ref domain.BlobRef) error {
			err := w.ledger.SealArtifact(callbackCtx, w.tokenID, ref)
			if err == nil {
				w.sealed = true
			}
			return err
		},
		func(callbackCtx context.Context, ref domain.BlobRef) error {
			return w.ledger.FinalizeArtifact(callbackCtx, w.tokenID, ref)
		})
	if err != nil {
		if !w.sealed {
			_ = w.inner.Abort(context.Background())
			_ = w.ledger.ReleaseArtifact(context.Background(), w.tokenID)
		} else if retryable, ok := w.inner.(interface{ MarkRetryable() }); ok {
			retryable.MarkRetryable()
		}
		return domain.PendingArtifact{}, err
	}
	w.ref, w.closed = pending.Blob, true
	return pending, nil
}

func (w *sessionWriter) Abort(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	if w.sealed {
		return errors.New("sealed artifact writer requires recovery or explicit ledger release")
	}
	err := w.inner.Abort(ctx)
	releaseErr := w.ledger.ReleaseArtifact(ctx, w.tokenID)
	w.closed = true
	return errors.Join(err, releaseErr)
}

func (w *sessionWriter) abortIfOpen(ctx context.Context) error { return w.Abort(ctx) }

func (w *sessionWriter) isFinalized() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.closed
}

var _ PreparedArtifactSession = (*preparedArtifactSession)(nil)

func ReadVerified(ctx context.Context, blobs port.VerifiedBlobReader, ref domain.BlobRef, limit int64) ([]byte, error) {
	if ref.Size > limit {
		return nil, errors.New("verification artifact exceeds byte bound")
	}
	reader, err := blobs.OpenVerified(ctx, ref)
	if err != nil {
		return nil, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
	if err := errors.Join(readErr, reader.Close()); err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("verification artifact exceeds byte bound")
	}
	return raw, nil
}
