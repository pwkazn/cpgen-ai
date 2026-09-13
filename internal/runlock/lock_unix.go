//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package runlock

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func lockFile(file *os.File, mode Mode) error {
	how := unix.LOCK_NB
	if mode == Shared {
		how |= unix.LOCK_SH
	} else {
		how |= unix.LOCK_EX
	}
	if err := unix.Flock(int(file.Fd()), how); err != nil {
		if errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.EWOULDBLOCK) {
			return ErrBusy
		}
		return fmt.Errorf("lock file: %w", err)
	}
	return nil
}

func unlockFile(file *os.File) error {
	if err := unix.Flock(int(file.Fd()), unix.LOCK_UN); err != nil {
		return fmt.Errorf("unlock file: %w", err)
	}
	return nil
}

func validateSingleLink(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat lock file: %w", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return errors.New("lock file must have exactly one link")
	}
	return nil
}
