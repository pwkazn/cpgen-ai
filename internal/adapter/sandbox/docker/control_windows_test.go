//go:build windows

package docker

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestOwnerOnlyACLDoesNotRequireChangingExistingOwner(t *testing.T) {
	path := t.TempDir()
	user, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	// Reproduce a data-drive directory: current user owns it, but its DACL
	// grants Modify (no WRITE_OWNER). Ownership still permits WRITE_DAC.
	descriptor, err := windows.SecurityDescriptorFromString("D:P(A;;GA;;;SY)(A;;0x1301bf;;;" + user.String() + ")")
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		user, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
	if err := applyOwnerOnlyACL(path); err != nil {
		t.Fatalf("secure existing owner directory without WRITE_OWNER: %v", err)
	}
	if err := validateOwnerOnlyACL(path); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareWatchdogControlRefusesActiveSameNoncePipe(t *testing.T) {
	base := t.TempDir()
	// Named pipes are machine-wide, even when the control directory is private
	// to this test. Independent test processes must not share a pipe identity.
	nonce, err := randomControlNonce()
	if err != nil {
		t.Fatal(err)
	}
	listener, _, directory, err := prepareWatchdogControl(base, nonce)
	if err != nil {
		t.Fatalf("prepare initial watchdog control: %v", err)
	}
	defer listener.Close()
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("initial watchdog directory missing: %v", err)
	}
	if _, _, _, err := prepareWatchdogControl(base, nonce); err == nil {
		t.Fatal("same-nonce replay removed an active watchdog control")
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatalf("active watchdog directory was removed: %v", err)
	}
}

func TestWatchdogCleanupWaitsForConcurrentWindowsDeletion(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "watchdog-owned")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	path, err := windows.UTF16PtrFromString(directory)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(path, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	var closeOnce sync.Once
	closeHandle := func() { closeOnce.Do(func() { _ = windows.CloseHandle(handle) }) }
	defer closeHandle()
	if err := windows.RemoveDirectory(path); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		time.Sleep(50 * time.Millisecond)
		closeHandle()
		close(done)
	}()
	if err := cleanupWatchdogControl(directory, filepath.Join(directory, "control.json")); err != nil {
		t.Fatal(err)
	}
	<-done
	if _, err := os.Lstat(directory); !os.IsNotExist(err) {
		t.Fatalf("directory is not absent: %v", err)
	}
}

func TestWatchdogCleanupPreservesUnexpectedContents(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "unrelated")
	if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := cleanupWatchdogControl(directory, filepath.Join(directory, "control.json")); err == nil {
		t.Fatal("nonempty control directory cleanup was reported successful")
	}
	if raw, err := os.ReadFile(path); err != nil || string(raw) != "keep" {
		t.Fatalf("unexpected contents changed: %q %v", raw, err)
	}
}
