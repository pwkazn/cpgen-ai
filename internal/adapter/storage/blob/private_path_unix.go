//go:build !windows

package blob

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openStagingFile creates a deterministic staging file without following a
// pre-existing symlink. O_EXCL is intentional: a PREPARED token owns no
// recoverable bytes, so an existing path is evidence of an interrupted or
// tampered write and must not be truncated.
func openStagingFile(root, path string) (*os.File, error) {
	dirFD, base, err := walkPrivateParent(root, path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = unix.Close(dirFD) }()
	fd, err := unix.Openat(dirFD, base, unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// ensurePrivateDirectoryTree creates each missing component with mkdirat and
// immediately reopens it with O_NOFOLLOW. No path-based mkdir can therefore
// follow a component that was replaced by a symlink during setup.
func ensurePrivateDirectoryTree(root, path string) error {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return errors.New("blob path escapes private root")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) == 0 || parts[0] == "" {
		return errors.New("blob directory path is invalid")
	}
	dirFD, err := unix.Open(filepath.Clean(root), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(dirFD) }()
	for _, part := range parts {
		if err := unix.Mkdirat(dirFD, part, 0700); err != nil && !errors.Is(err, unix.EEXIST) {
			return err
		}
		nextFD, err := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return err
		}
		_ = unix.Close(dirFD)
		dirFD = nextFD
	}
	return nil
}

func walkPrivateParent(root, path string) (fd int, base string, err error) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return -1, "", errors.New("blob path escapes private root")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) < 1 || parts[len(parts)-1] == "" {
		return -1, "", errors.New("blob path is invalid")
	}
	dirFD, err := unix.Open(filepath.Clean(root), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, "", err
	}
	for _, part := range parts[:len(parts)-1] {
		nextFD, openErr := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = unix.Close(dirFD)
			return -1, "", openErr
		}
		_ = unix.Close(dirFD)
		dirFD = nextFD
	}
	return dirFD, parts[len(parts)-1], nil
}

// linkPrivateFile performs the publication operation relative to directory
// handles. Even if an attacker replaces a path component after validation,
// linkat still addresses the already-open private directories and cannot
// escape the blob roots.
func linkPrivateFile(sourceRoot, source, targetRoot, target string) error {
	sourceFD, sourceBase, err := walkPrivateParent(sourceRoot, source)
	if err != nil {
		return err
	}
	defer unix.Close(sourceFD)
	targetFD, targetBase, err := walkPrivateParent(targetRoot, target)
	if err != nil {
		return err
	}
	defer unix.Close(targetFD)
	if err := unix.Linkat(sourceFD, sourceBase, targetFD, targetBase, 0); err != nil {
		return fmt.Errorf("link private blob: %w", err)
	}
	return nil
}

func removePrivateFile(root, path string) error {
	dirFD, base, err := walkPrivateParent(root, path)
	if err != nil {
		return err
	}
	defer unix.Close(dirFD)
	if err := unix.Unlinkat(dirFD, base, 0); err != nil {
		return err
	}
	return nil
}

// movePrivateFile renames between two private directory trees using opened
// parent handles, so a concurrent ancestor replacement cannot redirect the
// move outside either root.
func movePrivateFile(sourceRoot, source, targetRoot, target string) error {
	sourceFD, sourceBase, err := walkPrivateParent(sourceRoot, source)
	if err != nil {
		return err
	}
	defer unix.Close(sourceFD)
	targetFD, targetBase, err := walkPrivateParent(targetRoot, target)
	if err != nil {
		return err
	}
	defer unix.Close(targetFD)
	return unix.Renameat(sourceFD, sourceBase, targetFD, targetBase)
}
