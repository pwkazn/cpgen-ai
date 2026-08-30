//go:build !windows

package securefs_test

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/securefs"
)

func TestOpenRegularRejectsFIFOAndSocket(t *testing.T) {
	directory := t.TempDir()
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	if err := syscall.Mkfifo(filepath.Join(directory, "pipe"), 0o600); err != nil {
		t.Fatal(err)
	}
	if file, _, err := securefs.OpenRegular(root, domain.SafeRelPath("pipe"), true); err == nil {
		_ = file.Close()
		t.Fatal("FIFO was accepted")
	}

	listener, err := net.Listen("unix", filepath.Join(directory, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	if file, _, err := securefs.OpenRegular(root, domain.SafeRelPath("socket"), true); err == nil {
		_ = file.Close()
		t.Fatal("socket was accepted")
	}
}
