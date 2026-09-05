//go:build windows

package blob

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// openPrivateDirectory holds all validated ancestors while opening the final
// directory with OPEN_REPARSE_POINT. A junction replacement therefore fails
// closed instead of redirecting the reconciliation scan.
func openPrivateDirectory(root, path string) (*os.File, error) {
	ancestors, err := openWindowsPrivateAncestors(root, path)
	if err != nil {
		return nil, err
	}
	defer closeWindowsHandles(ancestors)
	handle, err := openWindowsDirectory(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("private directory is a reparse point or non-directory")
	}
	return os.NewFile(uintptr(handle), path), nil
}
