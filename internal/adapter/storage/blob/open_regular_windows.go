//go:build windows

package blob

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// openRegularAt validates every ancestor with a no-follow directory handle
// before opening the final component. Windows has no openat equivalent in the
// high-level API, so keeping all ancestor handles live until the final open is
// the fail-closed boundary for junction/reparse replacement attacks.
func openRegularAt(root, path string) (*os.File, error) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return nil, errors.New("blob canonical path escapes private root")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) < 1 || parts[len(parts)-1] == "" {
		return nil, errors.New("blob canonical path is invalid")
	}
	ancestors := make([]windows.Handle, 0, len(parts))
	closeAncestors := func() {
		for _, handle := range ancestors {
			_ = windows.CloseHandle(handle)
		}
	}
	current := filepath.Clean(root)
	rootHandle, err := openWindowsDirectory(current)
	if err != nil {
		return nil, err
	}
	ancestors = append(ancestors, rootHandle)
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		directory, openErr := openWindowsDirectory(current)
		if openErr != nil {
			closeAncestors()
			return nil, openErr
		}
		ancestors = append(ancestors, directory)
	}
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(filepath.Clean(path)), windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		closeAncestors()
		return nil, err
	}
	// The ancestor handles intentionally remain live through the final open.
	// Their delete sharing is disabled, so replacement of a validated parent
	// by a junction cannot race this path-based final operation.
	closeAncestors()
	var info windows.ByHandleFileInformation
	infoErr := windows.GetFileInformationByHandle(handle, &info)
	if infoErr != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		_ = windows.CloseHandle(handle)
		if infoErr != nil {
			return nil, infoErr
		}
		return nil, errors.New("blob canonical path is a reparse point or directory")
	}
	return os.NewFile(uintptr(handle), path), nil
}

func openWindowsDirectory(path string) (windows.Handle, error) {
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return 0, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY == 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		_ = windows.CloseHandle(handle)
		return 0, errors.New("blob path ancestor is a reparse point or non-directory")
	}
	return handle, nil
}
