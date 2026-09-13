//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package securefs

import (
	"fmt"
	"os"
	"syscall"
)

func hasSingleLink(_ *os.File, info os.FileInfo) (bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("unexpected file metadata type %T", info.Sys())
	}
	return stat.Nlink == 1, nil
}
