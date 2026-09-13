//go:build windows

package blob

import "golang.org/x/sys/windows"

func syncDirectory(path string) error {
	handle, err := windows.CreateFile(windows.StringToUTF16Ptr(path), windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	if err := windows.FlushFileBuffers(handle); err != nil {
		// Windows does not permit FlushFileBuffers on directory handles on
		// several supported filesystem providers. The file and link operations
		// above are still checked; there is no directory-fsync primitive to use
		// in this case.
		if err == windows.ERROR_ACCESS_DENIED || err == windows.ERROR_INVALID_FUNCTION {
			return nil
		}
		return err
	}
	return nil
}
