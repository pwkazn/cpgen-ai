package blob

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"cpgen/internal/domain"
)

// MoveToTrash is the only maintenance operation that moves canonical bytes.
// Its destination is derived from the content digest under this Store's
// private root; callers cannot provide a path to be followed.
func (s *Store) MoveToTrash(ctx context.Context, ref domain.BlobRef) (bool, error) {
	return s.moveToTrash(ctx, ref)
}

func (s *Store) moveToTrash(ctx context.Context, ref domain.BlobRef) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := ref.Validate(); err != nil {
		return false, err
	}
	if s == nil || s.trash == "" {
		return false, errors.New("blob store is nil")
	}
	publicationMu.Lock()
	defer publicationMu.Unlock()
	canonical := s.canonicalPath(ref)
	trash := s.trashPath(ref)
	if _, err := os.Lstat(trash); err == nil {
		_, canonicalErr := os.Lstat(canonical)
		if errors.Is(canonicalErr, os.ErrNotExist) {
			// A previous rename may have failed while syncing its directories.
			// Re-establish durability before maintenance can commit removal.
			return false, s.syncGarbageDirectories(ref)
		}
		if canonicalErr == nil {
			return false, errors.New("canonical and trash copies both exist")
		}
		return false, canonicalErr
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Lstat(canonical); errors.Is(err, os.ErrNotExist) {
		if syncErr := s.syncGarbageDirectories(ref); syncErr != nil {
			return false, syncErr
		}
		return false, fmt.Errorf("%w: %s", ErrBlobNotFound, ref.Digest)
	} else if err != nil {
		return false, err
	}
	if err := syncDirectoryFn(filepath.Dir(canonical)); err != nil {
		return false, err
	}
	if err := syncDirectoryFn(s.trash); err != nil {
		return false, err
	}
	if err := movePrivateFile(s.blobs, canonical, s.trash, trash); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, existsErr := os.Lstat(trash); existsErr == nil {
				return false, nil
			}
		}
		return false, err
	}
	if err := syncDirectoryFn(filepath.Dir(canonical)); err != nil {
		return true, err
	}
	if err := syncDirectoryFn(s.trash); err != nil {
		return true, err
	}
	return true, nil
}

// Sync even on a replay where rename/unlink already completed. The canonical
// shard may not exist for a missing blob; syncing the nearest surviving
// ancestor durably records that absence without creating new directories.
func (s *Store) syncGarbageDirectories(ref domain.BlobRef) error {
	parent := filepath.Dir(s.canonicalPath(ref))
	for {
		if err := syncDirectoryFn(parent); err != nil {
			if errors.Is(err, os.ErrNotExist) && parent != s.blobs {
				parent = filepath.Dir(parent)
				continue
			}
			return err
		}
		break
	}
	return syncDirectoryFn(s.trash)
}

// RemoveTrash deletes only a deterministic private trash file. Missing trash
// is already reconciled and therefore idempotent.
func (s *Store) RemoveTrash(ctx context.Context, ref domain.BlobRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	if s == nil || s.trash == "" {
		return errors.New("blob store is nil")
	}
	publicationMu.Lock()
	defer publicationMu.Unlock()
	if err := removePrivateFile(s.trash, s.trashPath(ref)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectoryFn(s.trash)
}

// StatTrash returns metadata for a deterministic trash file through the
// private root's no-follow open boundary.  Maintenance must use this helper
// instead of os.Stat: a replaced symlink or junction must fail closed rather
// than redirecting the check outside the Blob store.
func (s *Store) StatTrash(ctx context.Context, ref domain.BlobRef) (os.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	if s == nil || s.trash == "" {
		return nil, errors.New("blob store is nil")
	}
	publicationMu.Lock()
	defer publicationMu.Unlock()
	file, err := openRegularAt(s.trash, s.trashPath(ref))
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !singleLinkHandle(file, info) {
		return nil, errors.New("blob trash path is not a private regular file")
	}
	return info, nil
}

func (s *Store) trashPath(ref domain.BlobRef) string {
	return filepath.Join(s.trash, strings.TrimPrefix(string(ref.Digest), "sha256:")+".trash")
}

// TrashPath exposes the deterministic private path for maintenance tests and
// failpoint inspection; it does not authorize arbitrary path access.
func (s *Store) TrashPath(ref domain.BlobRef) string { return s.trashPath(ref) }

// TrashReferences enumerates only the private trash directory and rejects any
// name that is not a digest-derived regular file. It is intended for explicit
// reconciliation, never for ordinary workflow execution.
func (s *Store) TrashReferences(ctx context.Context) ([]domain.BlobRef, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s == nil || s.trash == "" {
		return nil, errors.New("blob store is nil")
	}
	trashDir, err := openPrivateDirectory(s.root, s.trash)
	if err != nil {
		return nil, err
	}
	defer trashDir.Close()
	entries, err := trashDir.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	refs := make([]domain.BlobRef, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() || !strings.HasSuffix(entry.Name(), ".trash") {
			return nil, fmt.Errorf("unexpected private trash entry %q", entry.Name())
		}
		hex := strings.TrimSuffix(entry.Name(), ".trash")
		ref, err := domain.ParseDigest("sha256:" + hex)
		if err != nil {
			return nil, err
		}
		// Re-open by digest through root-anchored O_NOFOLLOW APIs. Never use
		// DirEntry.Info/Stat here: those path-based calls can follow a file
		// replacement after the directory scan.
		file, err := openRegularAt(s.trash, s.trashPath(domain.BlobRef{Digest: ref}))
		if err != nil {
			return nil, fmt.Errorf("trash entry is not a private regular file: %s: %w", entry.Name(), err)
		}
		info, err := file.Stat()
		_ = file.Close()
		if err != nil {
			return nil, err
		}
		refs = append(refs, domain.BlobRef{Digest: ref, Size: info.Size()})
	}
	return refs, nil
}
