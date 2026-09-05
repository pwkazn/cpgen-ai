package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"sync"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// WriterIdentity is the immutable evidence attached to a prepared local
// artifact write. All IDs are checked before a temporary file is created.
type WriterIdentity struct {
	CallID        domain.AttemptCallID
	ReservationID domain.ReservationID
	WriterTokenID domain.ArtifactWriterTokenID
	PinID         domain.BlobPinID
}

func (v WriterIdentity) Validate() error {
	for name, err := range map[string]error{
		"call id": v.CallID.Validate(), "reservation id": v.ReservationID.Validate(),
		"writer token id": v.WriterTokenID.Validate(), "pin id": v.PinID.Validate(),
	} {
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	return nil
}

type writer struct {
	mu          sync.Mutex
	store       *Store
	declaration port.ArtifactDeclaration
	identity    WriterIdentity
	file        *os.File
	tempPath    string
	hash        hash.Hash
	size        int64
	terminal    bool
	staged      bool
	published   bool
	retryable   bool
	ref         domain.BlobRef
	physicalNew int64
}

func (s *Store) Prepare(ctx context.Context, declaration port.ArtifactDeclaration, identity WriterIdentity) (port.ArtifactWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := declaration.Validate(); err != nil {
		return nil, err
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if s == nil || s.temporary == "" {
		return nil, errors.New("blob store is nil")
	}
	// The token id is the durable staging identity.  A deterministic private
	// name lets a fresh process resume a SEALED writer after a crash without
	// putting the temporary path in the database or guessing from a directory
	// scan. An existing path is never truncated here. PREPARED owns no
	// recoverable bytes, while OPEN/SEALED recovery is coordinated by the
	// durable token.
	tempPath := s.stagingPath(identity.WriterTokenID)
	file, err := openStagingFile(s.temporary, tempPath)
	if err != nil {
		return nil, fmt.Errorf("create private artifact temp: %w", err)
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		_ = removePrivateFile(s.temporary, file.Name())
		return nil, err
	}
	return &writer{store: s, declaration: declaration, identity: identity, file: file, tempPath: tempPath, hash: sha256.New()}, nil
}

// ResumeStaged reconstructs the filesystem half of a durable SEALED token.
// The token id determines the staging path, so this operation is safe across
// process restarts and never adopts an unrelated temporary file.
func (s *Store) ResumeStaged(ctx context.Context, declaration port.ArtifactDeclaration, identity WriterIdentity, ref domain.BlobRef) (port.ArtifactWriter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := declaration.Validate(); err != nil {
		return nil, err
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if s == nil || s.temporary == "" {
		return nil, errors.New("blob store is nil")
	}
	tempPath := s.stagingPath(identity.WriterTokenID)
	if file, err := openRegularAt(s.temporary, tempPath); err == nil {
		info, statErr := file.Stat()
		linkCount, linkCountKnown := stagedLinkCount(file, info)
		if statErr != nil || !info.Mode().IsRegular() || !stagedLinkCountOK(file, info) {
			_ = file.Close()
			return nil, errors.New("staged artifact is not a private regular file")
		}
		if verifyErr := verifyHandle(ctx, file, ref); verifyErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("verify staged artifact: %w", verifyErr)
		}
		if err := file.Close(); err != nil {
			return nil, err
		}
		if linkCountKnown && linkCount == 2 {
			// A crash after linking temporary to canonical and before removing
			// temporary leaves two hardlinks. Adopt it only when the deterministic
			// canonical path verifies byte-for-byte, then remove the duplicate.
			canonical := s.canonicalPath(ref)
			canonicalFile, openErr := s.openPublishTarget(canonical)
			if openErr != nil {
				return nil, fmt.Errorf("recover staged duplicate link canonical: %w", openErr)
			}
			verifyErr := verifyHandle(ctx, canonicalFile, ref)
			closeErr := canonicalFile.Close()
			if verifyErr != nil {
				return nil, fmt.Errorf("verify staged duplicate canonical: %w", verifyErr)
			}
			if closeErr != nil {
				return nil, closeErr
			}
			if removeErr := removePrivateFile(s.temporary, tempPath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return nil, fmt.Errorf("remove recovered duplicate staging link: %w", removeErr)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &writer{store: s, declaration: declaration, identity: identity, tempPath: tempPath,
		ref: ref, terminal: true, staged: true, retryable: true, hash: sha256.New()}, nil
}

func (s *Store) stagingPath(id domain.ArtifactWriterTokenID) string {
	return filepath.Join(s.temporary, string(id)+".stage")
}

func (w *writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminal {
		return 0, errors.New("artifact writer is terminal")
	}
	if err := validateWriteSize(w.size, int64(len(p)), w.declaration.MaxBytes); err != nil {
		return 0, err
	}
	if err := contextError(context.Background()); err != nil {
		return 0, err
	}
	written, err := writeAll(w.file, p)
	if written > 0 {
		if _, hashErr := w.hash.Write(p[:written]); err == nil {
			err = hashErr
		}
		w.size += int64(written)
	}
	return written, err
}

func validateWriteSize(current, incoming, limit int64) error {
	if incoming < 0 || current > limit-incoming {
		return fmt.Errorf("artifact exceeds declared byte limit %d", limit)
	}
	return nil
}

func writeAll(file *os.File, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := file.Write(p[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func (w *writer) Finalize(ctx context.Context) (domain.PendingArtifact, error) {
	pending, err := w.stage(ctx)
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	physicalNew, err := w.publish(ctx)
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	pending.PhysicalNewBytes = physicalNew
	return pending, nil
}

// Stage closes and fsyncs the private temporary file and fixes the digest.
// It deliberately does not publish a canonical file.  Prepared sessions use
// this seam to commit the durable SEALED/pin row before the filesystem link.
func (w *writer) stage(ctx context.Context) (domain.PendingArtifact, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return domain.PendingArtifact{}, err
	}
	if w.terminal && w.staged && w.retryable {
		return w.pending(0), nil
	}
	if w.terminal {
		return domain.PendingArtifact{}, errors.New("artifact writer is already terminal")
	}
	w.terminal = true
	if err := w.file.Sync(); err != nil {
		w.cleanup()
		return domain.PendingArtifact{}, err
	}
	if err := w.file.Close(); err != nil {
		w.cleanup()
		return domain.PendingArtifact{}, err
	}
	w.ref = domain.BlobRef{Digest: domain.Digest(fmt.Sprintf("sha256:%x", w.hash.Sum(nil))), Size: w.size}
	w.staged = true
	return w.pending(0), nil
}

// Publish links the staged bytes into the canonical CAS location without
// overwrite, then verifies the exact opened handle.  It is safe to call only
// after the corresponding durable SEALED transaction has committed.
func (w *writer) publish(ctx context.Context) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if !w.staged {
		return 0, errors.New("artifact writer is not a publishable staged writer")
	}
	if w.published {
		verified, err := w.store.OpenVerified(ctx, w.ref)
		if err != nil {
			return 0, err
		}
		if err := verified.Close(); err != nil {
			return 0, err
		}
		return 0, nil
	}
	physicalNew, err := w.store.publish(ctx, w.tempPath, w.ref)
	if err != nil {
		// A failed directory fsync may have left a durable canonical file.  Keep
		// the sealed state and staging evidence for retry/reconciliation; the
		// session layer deliberately does not release this token automatically.
		return 0, err
	}
	verified, err := w.store.OpenVerified(ctx, w.ref)
	if err != nil {
		return 0, err
	}
	if err := verified.Close(); err != nil {
		return 0, err
	}
	w.physicalNew, w.published = physicalNew, true
	return physicalNew, nil
}

type stagedWriter interface {
	stage(context.Context) (domain.PendingArtifact, error)
	publish(context.Context) (int64, error)
}

// MarkRetryable allows the application recovery coordinator to retain a
// staged writer after a post-seal failure for an in-process retry.
func (w *writer) MarkRetryable() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.retryable = true
}

// FinalizeWithLedger coordinates the cross-store boundary for prepared
// writers. The callbacks each perform only a short durable transaction. The
// staged writer path commits Seal before linking canonical bytes; other writer
// implementations retain the original finalize callback order.
func FinalizeWithLedger(ctx context.Context, writer port.ArtifactWriter,
	seal func(context.Context, domain.BlobRef) error,
	finalize func(context.Context, domain.BlobRef) error,
) (domain.PendingArtifact, error) {
	if writer == nil || seal == nil || finalize == nil {
		return domain.PendingArtifact{}, errors.New("artifact finalization callbacks are required")
	}
	if staged, ok := writer.(stagedWriter); ok {
		pending, err := staged.stage(ctx)
		if err != nil {
			return domain.PendingArtifact{}, err
		}
		if err := seal(ctx, pending.Blob); err != nil {
			return domain.PendingArtifact{}, err
		}
		physicalNew, err := staged.publish(ctx)
		if err != nil {
			return domain.PendingArtifact{}, err
		}
		if err := finalize(ctx, pending.Blob); err != nil {
			return domain.PendingArtifact{}, err
		}
		pending.PhysicalNewBytes = physicalNew
		return pending, nil
	}
	pending, err := writer.Finalize(ctx)
	if err != nil {
		return domain.PendingArtifact{}, err
	}
	if err := seal(ctx, pending.Blob); err != nil {
		return domain.PendingArtifact{}, err
	}
	if err := finalize(ctx, pending.Blob); err != nil {
		return domain.PendingArtifact{}, err
	}
	return pending, nil
}

func (w *writer) pending(physicalNew int64) domain.PendingArtifact {
	return domain.PendingArtifact{Blob: w.ref, MediaType: w.declaration.MediaType, Role: w.declaration.Role, LogicalPath: w.declaration.LogicalPath,
		CallID: w.identity.CallID, ReservationID: w.identity.ReservationID, WriterTokenID: w.identity.WriterTokenID, PinID: w.identity.PinID,
		PhysicalNewBytes: physicalNew, Provenance: w.declaration.Provenance}
}

func (w *writer) Abort(context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.terminal && w.published {
		return nil
	}
	w.terminal = true
	w.cleanup()
	return nil
}

func (w *writer) cleanup() {
	if w.file != nil {
		_ = w.file.Close()
	}
	if w.tempPath != "" {
		_ = removePrivateFile(w.store.temporary, w.tempPath)
	}
}

func contextError(ctx context.Context) error { return ctx.Err() }
