//go:build windows

package docker

import (
	"os"
	"strings"
	"testing"
)

func TestPrepareWatchdogControlRefusesActiveSameNoncePipe(t *testing.T) {
	base := t.TempDir()
	nonce := strings.Repeat("a", 32)
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
