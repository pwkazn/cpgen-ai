//go:build !windows

package blob

import (
	"os"

	"golang.org/x/sys/unix"
)

// openPrivateDirectory opens a directory through already-rooted directory
// handles. O_NOFOLLOW is applied to every component, so a replaced trash
// directory cannot redirect a scan outside the private artifact root.
func openPrivateDirectory(root, path string) (*os.File, error) {
	dirFD, base, err := walkPrivateParent(root, path)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Openat(dirFD, base, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	_ = unix.Close(dirFD)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
