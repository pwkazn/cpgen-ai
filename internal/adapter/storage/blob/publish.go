package blob

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"cpgen/internal/domain"
)

func (s *Store) publish(ctx context.Context, temporary string, ref domain.BlobRef) (int64, error) {
	publicationMu.Lock()
	defer publicationMu.Unlock()
	return s.publishLocked(ctx, temporary, ref)
}

func (s *Store) publishLocked(ctx context.Context, temporary string, ref domain.BlobRef) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	target := s.canonicalPath(ref)
	if err := ensurePrivateDirectoryTree(s.blobs, filepath.Dir(target)); err != nil {
		return 0, err
	}
	if _, err := os.Lstat(target); err == nil {
		// A publisher may still own a temporary hardlink while another
		// publisher observes the canonical target.  Do not apply the normal
		// single-link read invariant here: link count 2 is the expected
		// in-flight state, not corruption.
		file, openErr := s.openPublishTarget(target)
		if openErr != nil {
			return 0, s.corrupt(ctx, target, ref, openErr)
		}
		verifyErr := verifyHandle(ctx, file, ref)
		_ = file.Close()
		if verifyErr != nil {
			return 0, s.corrupt(ctx, target, ref, verifyErr)
		}
		if err := removePrivateFile(s.temporary, temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
			return 0, fmt.Errorf("remove deduplicated temporary blob: %w", err)
		}
		if err := syncDirectoryFn(filepath.Dir(target)); err != nil {
			return 0, fmt.Errorf("sync deduplicated blob directory: %w", err)
		}
		return 0, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := linkPrivateFile(s.temporary, temporary, s.blobs, target); err != nil {
		if errors.Is(err, os.ErrExist) {
			return s.publishLocked(ctx, temporary, ref)
		}
		return 0, fmt.Errorf("publish blob without overwrite: %w", err)
	}
	if err := removePrivateFile(s.temporary, temporary); err != nil {
		// Preserve both links when cleanup fails. ResumeStaged can verify the
		// canonical link and safely remove the duplicate after a crash; removing
		// target here would destroy the only recoverable publication evidence.
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("remove published temporary blob: %w", err)
	}
	if err := syncDirectoryFn(filepath.Dir(target)); err != nil {
		return 0, fmt.Errorf("sync published blob directory: %w", err)
	}
	return ref.Size, nil
}

func (s *Store) openPublishTarget(path string) (*os.File, error) {
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
	if err != nil || !info.Mode().IsRegular() || !singleLinkAtLeastOne(file, info) {
		_ = file.Close()
		return nil, fmt.Errorf("blob canonical path is not a regular file")
	}
	return file, nil
}

// singleLinkAtLeastOne is deliberately weaker than OpenVerified's final
// single-link check.  During publication another process may transiently
// hold the staging hardlink; bytes are still verified from this exact handle.
func singleLinkAtLeastOne(file *os.File, info os.FileInfo) bool {
	if file == nil || info == nil {
		return false
	}
	return info.Mode().IsRegular()
}

func (s *Store) corrupt(ctx context.Context, target string, ref domain.BlobRef, cause error) error {
	if s.corruption != nil {
		if err := s.corruption.QuarantineBlob(ctx, ref); err != nil {
			return errors.Join(fmt.Errorf("verify blob: %w", cause), err)
		}
	}
	if err := s.quarantinePathLocked(target, ref); err != nil {
		return errors.Join(fmt.Errorf("verify blob: %w", cause), err)
	}
	return fmt.Errorf("verify blob: %w", cause)
}

func (s *Store) quarantinePath(target string, ref domain.BlobRef) error {
	publicationMu.Lock()
	defer publicationMu.Unlock()
	return s.quarantinePathLocked(target, ref)
}

func (s *Store) quarantinePathLocked(target string, ref domain.BlobRef) error {
	if _, err := os.Lstat(target); err != nil {
		return err
	}
	name := fmt.Sprintf("%s.corrupt", string(ref.Digest)[len("sha256:"):])
	if _, err := os.Lstat(filepath.Join(s.quarantine, name)); err == nil {
		// A second quarantine of the same digest is already represented by the
		// fixed private name; leave the prior evidence untouched.
		if err := removePrivateFile(s.blobs, target); err != nil {
			return err
		}
		return syncDirectoryFn(filepath.Dir(target))
	}
	if err := syncDirectoryFn(s.quarantine); err != nil {
		return err
	}
	if err := movePrivateFile(s.blobs, target, s.quarantine, filepath.Join(s.quarantine, name)); err != nil {
		return err
	}
	if err := syncDirectoryFn(filepath.Dir(target)); err != nil {
		return err
	}
	return syncDirectoryFn(s.quarantine)
}
