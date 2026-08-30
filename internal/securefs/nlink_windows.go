//go:build windows

package securefs

import (
	"os"

	"golang.org/x/sys/windows"
)

func hasSingleLink(file *os.File, _ os.FileInfo) (bool, error) {
	var information windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(file.Fd()), &information); err != nil {
		return false, err
	}
	return information.NumberOfLinks == 1, nil
}
