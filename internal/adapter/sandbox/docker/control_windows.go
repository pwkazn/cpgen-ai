//go:build windows

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
	"strings"
	"sync"
	"syscall"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
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
	if err := applyOwnerOnlyACL(base); err != nil {
		return nil, "", "", err
	}
	if info, err := os.Lstat(base); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = fmt.Errorf("watchdog control base must be a direct directory")
		}
		return nil, "", "", err
	}
	directory := filepath.Join(base, "watchdog-"+nonce)
	if err := os.Mkdir(directory, 0o700); err != nil {
		if !os.IsExist(err) {
			return nil, "", "", err
		}
		if err := removeStaleWatchdogControl(directory); err != nil {
			return nil, "", "", err
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			return nil, "", "", err
		}
	}
	if err := applyOwnerOnlyACL(directory); err != nil {
		_ = os.Remove(directory)
		return nil, "", "", err
	}
	if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = fmt.Errorf("watchdog control directory must be a direct directory")
		}
		_ = os.Remove(directory)
		return nil, "", "", err
	}
	address := `\\.\pipe\cpgen-watchdog-` + nonce
	sddl, err := ownerOnlySDDL()
	if err != nil {
		_ = os.Remove(directory)
		return nil, "", "", err
	}
	listener, err := winio.ListenPipe(address, &winio.PipeConfig{
		SecurityDescriptor: sddl, MessageMode: false, InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10,
	})
	if err != nil {
		_ = os.Remove(directory)
		return nil, "", "", err
	}
	return listener, address, directory, nil
}

func prepareWatchdogControlDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("watchdog control directory must be absolute")
	}
	if err := applyOwnerOnlyACL(path); err != nil {
		return err
	}
	return validateOwnerOnlyACL(path)
}

func removeStaleWatchdogControl(directory string) error {
	if err := validateOwnerOnlyACL(directory); err != nil {
		return fmt.Errorf("refusing unsafe stale watchdog directory: %w", err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "control.json" {
			return fmt.Errorf("refusing stale watchdog directory with unexpected entry %q", entry.Name())
		}
		path := filepath.Join(directory, entry.Name())
		info, statErr := os.Lstat(path)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refusing unsafe stale watchdog entry %q", entry.Name())
		}
		if err := validateOwnerOnlyACL(path); err != nil {
			return err
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
	if err := applyOwnerOnlyACL(path); err != nil {
		return err
	}
	return validateOwnerOnlyACL(path)
}

func secureReadWatchdogControl(path string, maxBytes int64) ([]byte, error) {
	if !filepath.IsAbs(path) || maxBytes <= 0 {
		return nil, fmt.Errorf("absolute watchdog control path and positive limit are required")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("watchdog control is not a direct regular file")
	}
	if err := validateOwnerOnlyACL(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if err := validateOwnerOnlyACL(path); err != nil {
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

func applyOwnerOnlyACL(path string) error {
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	sddl := "D:P(A;;GA;;;SY)(A;;GA;;;" + user.String() + ")"
	descriptor, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user, nil, dacl, nil)
}

func validateOwnerOnlyACL(path string) error {
	user, err := currentUserSID()
	if err != nil {
		return err
	}
	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := descriptor.Owner()
	if err != nil || !owner.Equals(user) {
		return fmt.Errorf("watchdog control owner is not the current user")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 2 {
		return fmt.Errorf("watchdog control DACL is not owner-only")
	}
	encoded := descriptor.String()
	if !strings.Contains(encoded, ";;;SY)") || !strings.Contains(encoded, ";;;"+user.String()+")") ||
		strings.Contains(encoded, ";;;WD)") || strings.Contains(encoded, ";;;AU)") || strings.Contains(encoded, ";;;BU)") || strings.Contains(encoded, ";;;BA)") {
		return fmt.Errorf("watchdog control DACL grants an unexpected principal")
	}
	return nil
}

func ownerOnlySDDL() (string, error) {
	user, err := currentUserSID()
	if err != nil {
		return "", err
	}
	return "D:P(A;;GA;;;SY)(A;;GA;;;" + user.String() + ")", nil
}

func currentUserSID() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid, nil
}

func dialWatchdogControl(ctx context.Context, address string) (net.Conn, error) {
	return winio.DialPipeContext(ctx, address)
}

func detachWatchdogCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow: true, CreationFlags: windows.CREATE_NEW_PROCESS_GROUP | windows.DETACHED_PROCESS | windows.CREATE_NO_WINDOW,
		NoInheritHandles: true,
	}
}

func cleanupWatchdogControl(directory, controlPath string) error {
	var failures []error
	if err := os.Remove(controlPath); err != nil && !os.IsNotExist(err) {
		failures = append(failures, err)
	}
	if err := os.Remove(directory); err != nil && !os.IsNotExist(err) {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

var watchdogControlMu sync.Mutex
