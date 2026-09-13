//go:build windows

package blob

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// openStagingFile uses CREATE_NEW and OPEN_REPARSE_POINT. A deterministic
// staging path is never allowed to follow an existing symlink or junction,
// and an existing path is treated as tampering/recovery evidence rather than
// truncated.
func openStagingFile(root, path string) (*os.File, error) {
	ancestors, err := openWindowsPrivateAncestors(root, path)
	if err != nil {
		return nil, err
	}
	defer closeWindowsHandles(ancestors)
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(filepath.Clean(path)), windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.CREATE_NEW,
		windows.FILE_ATTRIBUTE_NORMAL|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return nil, err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("staging path is a reparse point or directory")
	}
	return os.NewFile(uintptr(handle), path), nil
}

// ensurePrivateDirectoryTree creates and validates one component at a time.
// Every existing component is opened with OPEN_REPARSE_POINT and with delete
// sharing disabled before the next component is resolved.
func ensurePrivateDirectoryTree(root, path string) error {
	return ensurePrivateDirectoryTreeWithOps(root, path, func(path string) error {
		if err := windows.CreateDirectory(windows.StringToUTF16Ptr(path), nil); err != nil && err != windows.ERROR_ALREADY_EXISTS {
			return err
		}
		return nil
	}, openWindowsDirectory, closeWindowsHandles)
}

// ensurePrivateDirectoryTreeWithOps keeps every validated ancestor handle
// live until all missing components have been created and reopened. Windows
// has no public mkdirat equivalent, so the no-delete-sharing handles are the
// root-anchored boundary that prevents a validated ancestor from being
// replaced by a junction while the remaining absolute path components are
// resolved.
func ensurePrivateDirectoryTreeWithOps(root, path string, createDirectory func(string) error, openDirectory func(string) (windows.Handle, error), closeHandles func([]windows.Handle)) error {
	_, parts, err := privateRelativeParts(root, path)
	if err != nil {
		return err
	}
	current := filepath.Clean(root)
	rootHandle, err := openDirectory(current)
	if err != nil {
		return err
	}
	ancestors := []windows.Handle{rootHandle}
	defer func() { closeHandles(ancestors) }()
	for _, part := range parts {
		current = filepath.Join(current, part)
		if err := createDirectory(current); err != nil {
			return err
		}
		directory, err := openDirectory(current)
		if err != nil {
			return err
		}
		ancestors = append(ancestors, directory)
	}
	return nil
}

// linkPrivateFile holds both parent directory handles with delete sharing
// disabled while CreateHardLink resolves the paths. Windows has no public
// linkat equivalent; the handles make ancestor junction replacement fail
// closed for the complete link operation.
func linkPrivateFile(sourceRoot, source, targetRoot, target string) error {
	sourceAncestors, err := openWindowsPrivateAncestors(sourceRoot, source)
	if err != nil {
		return err
	}
	defer closeWindowsHandles(sourceAncestors)
	targetAncestors, err := openWindowsPrivateAncestors(targetRoot, target)
	if err != nil {
		return err
	}
	defer closeWindowsHandles(targetAncestors)
	sourceParent, sourceBase := filepath.Dir(source), filepath.Base(source)
	targetParent, targetBase := filepath.Dir(target), filepath.Base(target)
	// Open the source file with delete and write sharing disabled. This keeps
	// the exact source object stable while the path-based hard-link syscall is
	// performed under the already-held private parent handles.
	sourceFile, err := windows.CreateFile(windows.StringToUTF16Ptr(filepath.Join(sourceParent, sourceBase)), windows.GENERIC_READ,
		windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(sourceFile)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(sourceFile, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 || info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		return errors.New("staging path is a reparse point or directory")
	}
	return windows.CreateHardLink(windows.StringToUTF16Ptr(filepath.Join(targetParent, targetBase)), windows.StringToUTF16Ptr(filepath.Join(sourceParent, sourceBase)), 0)
}

func privateParentPath(root, path string) (string, string, error) {
	_, parts, err := privateRelativeParts(root, path)
	if err != nil {
		return "", "", err
	}
	base := parts[len(parts)-1]
	if base == "." || base == "" || base == string(filepath.Separator) {
		return "", "", errors.New("blob path is invalid")
	}
	return filepath.Join(filepath.Clean(root), filepath.Join(parts[:len(parts)-1]...)), base, nil
}

func removePrivateFile(root, path string) error {
	ancestors, err := openWindowsPrivateAncestors(root, path)
	if err != nil {
		return err
	}
	defer closeWindowsHandles(ancestors)
	return windows.DeleteFile(windows.StringToUTF16Ptr(filepath.Clean(path)))
}

// movePrivateFile keeps every source and destination ancestor open with
// delete sharing disabled while the path-based Windows rename is performed.
// Any junction/reparse replacement therefore fails closed at the opened
// ancestor boundary.
func movePrivateFile(sourceRoot, source, targetRoot, target string) error {
	sourceAncestors, err := openWindowsPrivateAncestors(sourceRoot, source)
	if err != nil {
		return err
	}
	defer closeWindowsHandles(sourceAncestors)
	targetAncestors, err := openWindowsPrivateAncestors(targetRoot, target)
	if err != nil {
		return err
	}
	defer closeWindowsHandles(targetAncestors)
	return windows.MoveFileEx(windows.StringToUTF16Ptr(filepath.Clean(source)), windows.StringToUTF16Ptr(filepath.Clean(target)), 0)
}

func privateRelativeParts(root, path string) (string, []string, error) {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", nil, errors.New("blob path escapes private root")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) < 1 || parts[len(parts)-1] == "" || parts[len(parts)-1] == "." {
		return "", nil, errors.New("blob path is invalid")
	}
	return relative, parts, nil
}

func openWindowsPrivateAncestors(root, path string) ([]windows.Handle, error) {
	_, parts, err := privateRelativeParts(root, path)
	if err != nil {
		return nil, err
	}
	handles := make([]windows.Handle, 0, len(parts))
	closeOnError := func() {
		closeWindowsHandles(handles)
	}
	current := filepath.Clean(root)
	handle, err := openWindowsDirectory(current)
	if err != nil {
		return nil, err
	}
	handles = append(handles, handle)
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		directory, err := openWindowsDirectory(current)
		if err != nil {
			closeOnError()
			return nil, err
		}
		handles = append(handles, directory)
	}
	return handles, nil
}

func closeWindowsHandles(handles []windows.Handle) {
	for _, handle := range handles {
		_ = windows.CloseHandle(handle)
	}
}
