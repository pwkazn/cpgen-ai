// Package blob implements the private, content-addressed artifact filesystem.
// It intentionally has no exported raw put operation: callers can only obtain
// a bounded writer from an artifact declaration and can only read verified
// handles.
package blob

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

var (
	ErrBlobNotFound = errors.New("blob does not exist")
	ErrBlobCorrupt  = errors.New("blob is corrupt")
)

type Store struct {
	root       string
	blobs      string
	temporary  string
	quarantine string
	trash      string
	corruption CorruptionLedger
}

// publicationMu serializes publication and quarantine across Store handles.
// A process may open the same private root more than once during recovery;
// using a package lock closes that otherwise cross-handle rename race too.
var publicationMu sync.Mutex

// CorruptionLedger is the narrow durable hook used to keep a SQLite Blob
// projection aligned with filesystem quarantine.
type CorruptionLedger interface {
	QuarantineBlob(context.Context, domain.BlobRef) error
}

func NewStore(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" || !filepath.IsAbs(root) {
		return nil, errors.New("blob root must be an absolute path")
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create private blob directory: %w", err)
	}
	if err := ensurePrivateDirectory(root); err != nil {
		return nil, err
	}
	for _, path := range []string{filepath.Join(root, "blobs"), filepath.Join(root, "tmp"), filepath.Join(root, "quarantine"), filepath.Join(root, "trash")} {
		if err := ensurePrivateDirectoryTree(root, path); err != nil {
			return nil, fmt.Errorf("create private blob directory: %w", err)
		}
		if err := ensurePrivateDirectory(path); err != nil {
			return nil, err
		}
		_ = os.Chmod(path, 0700)
	}
	return &Store{root: root, blobs: filepath.Join(root, "blobs"), temporary: filepath.Join(root, "tmp"), quarantine: filepath.Join(root, "quarantine"), trash: filepath.Join(root, "trash")}, nil
}

// Open is an alias useful to callers that treat the private store as a
// filesystem resource.
func Open(root string) (*Store, error) { return NewStore(root) }

// AttachCorruptionLedger installs the durable state hook before the store is
// handed to a prepared artifact session. It is intentionally narrow and does
// not expose SQLite or a general write capability to blob callers.
func (s *Store) AttachCorruptionLedger(ledger CorruptionLedger) error {
	if s == nil {
		return errors.New("blob store is nil")
	}
	s.corruption = ledger
	return nil
}

type verifiedReadCloser struct {
	*os.File
	ref domain.BlobRef
}

func (r *verifiedReadCloser) BlobRef() domain.BlobRef { return r.ref }

func (s *Store) OpenVerified(ctx context.Context, ref domain.BlobRef) (port.VerifiedReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if s == nil || s.blobs == "" {
		return nil, errors.New("blob store is nil")
	}
	publicationMu.Lock()
	defer publicationMu.Unlock()
	path := s.canonicalPath(ref)
	file, err := s.openRegular(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrBlobNotFound, ref.Digest)
	}
	if err != nil {
		return nil, err
	}
	if err := verifyHandle(ctx, file, ref); err != nil {
		_ = file.Close()
		if errors.Is(err, ErrBlobCorrupt) {
			// Persist CORRUPT before moving bytes. A crash after this point is
			// fail-closed (the canonical row is no longer READY), while a
			// filesystem move failure leaves durable evidence to reconcile.
			if s.corruption != nil {
				if ledgerErr := s.corruption.QuarantineBlob(ctx, ref); ledgerErr != nil {
					return nil, errors.Join(err, ledgerErr)
				}
			}
			if quarantineErr := s.quarantinePathLocked(path, ref); quarantineErr != nil {
				return nil, errors.Join(err, quarantineErr)
			}
		}
		return nil, err
	}
	return &verifiedReadCloser{File: file, ref: ref}, nil
}

func (s *Store) canonicalPath(ref domain.BlobRef) string {
	hex := strings.TrimPrefix(string(ref.Digest), "sha256:")
	return filepath.Join(s.blobs, "sha256", hex[:2], hex)
}

func ensurePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect private blob directory: %w", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("blob path is not a private directory: %s", path)
	}
	return nil
}

func (s *Store) openRegular(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("blob canonical path is not a regular file")
	}
	file, err := openRegularAt(s.blobs, path)
	if err != nil {
		return nil, err
	}
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || !singleLinkHandle(file, info) {
		_ = file.Close()
		return nil, fmt.Errorf("blob canonical path is not a regular file")
	}
	return file, nil
}

func verifyHandle(ctx context.Context, file *os.File, expected domain.BlobRef) error {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	digest, size, err := domain.SumReader(contextReader{ctx: ctx, reader: file})
	if err != nil {
		return err
	}
	if digest != expected.Digest || size != expected.Size {
		return fmt.Errorf("%w: expected %s/%d, got %s/%d", ErrBlobCorrupt, expected.Digest, expected.Size, digest, size)
	}
	_, err = file.Seek(0, io.SeekStart)
	return err
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
