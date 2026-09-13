//go:build !windows

package blob

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// openRegularAt walks every parent from an already validated private root
// using directory handles. This closes the Lstat/Open TOCTOU window for both
// the final file and parent replacement attacks.
func openRegularAt(root, path string) (*os.File, error) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, errors.New("blob canonical path escapes private root")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) < 1 || parts[len(parts)-1] == "" {
		return nil, errors.New("blob canonical path is invalid")
	}
	dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	for _, part := range parts[:len(parts)-1] {
		nextFD, openErr := unix.Openat(dirFD, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(dirFD)
		if openErr != nil {
			return nil, openErr
		}
		dirFD = nextFD
	}
	fileFD, err := unix.Openat(dirFD, parts[len(parts)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	_ = unix.Close(dirFD)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fileFD), path), nil
}
