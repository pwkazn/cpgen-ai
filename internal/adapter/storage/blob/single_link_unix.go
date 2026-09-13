//go:build !windows

package blob

import (
	"os"
	"reflect"
)

func singleLinkHandle(_ *os.File, info os.FileInfo) bool {
	return singleLink(info)
}

func stagedLinkCount(_ *os.File, info os.FileInfo) (uint64, bool) {
	if info == nil {
		return 0, false
	}
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Ptr && !value.IsNil() {
		value = value.Elem()
	}
	if value.IsValid() && value.Kind() == reflect.Struct {
		field := value.FieldByName("Nlink")
		if field.IsValid() && field.Kind() >= reflect.Uint && field.Kind() <= reflect.Uint64 {
			return field.Uint(), true
		}
	}
	return 0, false
}

func stagedLinkCountOK(file *os.File, info os.FileInfo) bool {
	count, known := stagedLinkCount(file, info)
	return known && (count == 1 || count == 2)
}

func singleLink(info os.FileInfo) bool {
	count, known := stagedLinkCount(nil, info)
	return known && count == 1
}
