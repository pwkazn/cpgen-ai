//go:build windows

package blob

import (
	"os"

	"golang.org/x/sys/windows"
)

func singleLinkHandle(file *os.File, _ os.FileInfo) bool {
	count, ok := stagedLinkCount(file, nil)
	return ok && count == 1
}

func stagedLinkCount(file *os.File, _ os.FileInfo) (uint64, bool) {
	if file == nil {
		return 0, false
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &info); err != nil {
		return 0, false
	}
	return uint64(info.NumberOfLinks), true
}

func stagedLinkCountOK(file *os.File, _ os.FileInfo) bool {
	count, known := stagedLinkCount(file, nil)
	return known && (count == 1 || count == 2)
}

func singleLink(_ os.FileInfo) bool { return false }
