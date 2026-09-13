//go:build !windows

package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

func prepareWatchdogControl(base, nonce string) (net.Listener, string, string, error) {
	watchdogControlMu.Lock()
	defer watchdogControlMu.Unlock()
	if !filepath.IsAbs(base) {
		return nil, "", "", fmt.Errorf("watchdog control base must be absolute")
	}
	if !operationNoncePattern.MatchString(nonce) {
		return nil, "", "", fmt.Errorf("watchdog control nonce is invalid")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		return nil, "", "", err
	}
	if err := os.Chmod(base, 0o700); err != nil {
		return nil, "", "", err
	}
	if err := validateOwnerOnlyDirectory(base); err != nil {
		return nil, "", "", err
	}
	directory := filepath.Join(base, "watchdog-"+nonce)
	if err := os.Mkdir(directory, 0o700); err != nil {
		if !os.IsExist(err) {
			return nil, "", "", err
		}
		// A live listener means the same sealed identity is still owned by
		// another process. Never remove its directory while trying to replay
		// the operation; stale-but-unbound socket files are safe to retire.
		socket := filepath.Join(directory, "control.sock")
		if conn, dialErr := net.DialTimeout("unix", socket, 50*time.Millisecond); dialErr == nil {
			_ = conn.Close()
			return nil, "", "", fmt.Errorf("watchdog control for this identity is already active")
		} else if !errors.Is(dialErr, syscall.ECONNREFUSED) && !errors.Is(dialErr, syscall.ENOENT) {
			return nil, "", "", fmt.Errorf("watchdog control socket may be active: %w", dialErr)
		}
		if err := removeStaleWatchdogControl(directory); err != nil {
			return nil, "", "", err
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			return nil, "", "", err
		}
	}
	if err := validateOwnerOnlyDirectory(directory); err != nil {
		return nil, "", "", err
	}
	address := filepath.Join(directory, "control.sock")
	listener, err := net.Listen("unix", address)
	if err != nil {
		_ = os.Remove(directory)
		return nil, "", "", err
	}
	if err := os.Chmod(address, 0o600); err != nil {
		listener.Close()
		_ = os.Remove(address)
		_ = os.Remove(directory)
		return nil, "", "", err
	}
	return listener, address, directory, nil
}

// prepareWatchdogControlDirectory applies the platform-neutral owner-only
// directory policy used by the in-process controller. It exists behind the
// platform file so portable builds never reference Windows ACL symbols.
func prepareWatchdogControlDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("watchdog control directory must be absolute")
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return validateOwnerOnlyDirectory(path)
}

func validateOwnerOnlyDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("watchdog control directory must be a direct 0700 directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("watchdog control directory owner is not the current user")
	}
	return nil
}

func removeStaleWatchdogControl(directory string) error {
	if err := validateOwnerOnlyDirectory(directory); err != nil {
		return fmt.Errorf("refusing unsafe stale watchdog directory: %w", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if name != "control.json" && name != "control.sock" {
			return fmt.Errorf("refusing stale watchdog directory with unexpected entry %q", name)
		}
		path := filepath.Join(directory, name)
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || (!info.Mode().IsRegular() && info.Mode()&os.ModeSocket == 0) {
			return fmt.Errorf("refusing unsafe stale watchdog entry %q", name)
		}
		if name == "control.json" && info.Mode().Perm() != 0o600 {
			return fmt.Errorf("refusing stale watchdog control with unsafe permissions")
		}
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return os.Remove(directory)
}

func secureWriteWatchdogControl(path string, data []byte) (returnErr error) {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("watchdog control path must be absolute")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); returnErr == nil && err != nil {
			returnErr = err
		}
		if returnErr != nil {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	return validateOwnerOnlyFile(path)
}

func secureReadWatchdogControl(path string, maxBytes int64) ([]byte, error) {
	if !filepath.IsAbs(path) || maxBytes <= 0 {
		return nil, fmt.Errorf("absolute watchdog control path and positive limit are required")
	}
	if err := validateOwnerOnlyFile(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("watchdog control exceeds %d bytes", maxBytes)
	}
	return data, nil
}

func validateOwnerOnlyFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("watchdog control must be a direct 0600 regular file")
	}
	parent, err := os.Stat(filepath.Dir(path))
	if err != nil || !parent.IsDir() || parent.Mode().Perm() != 0o700 {
		return fmt.Errorf("watchdog control directory must be 0700")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("watchdog control owner is not the current user")
	}
	return nil
}

func dialWatchdogControl(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", address)
}

func detachWatchdogCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func cleanupWatchdogControl(directory, controlPath string) error {
	var failures []error
	for _, path := range []string{controlPath, filepath.Join(directory, "control.sock"), directory} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

var watchdogControlMu sync.Mutex
