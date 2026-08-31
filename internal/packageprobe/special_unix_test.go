//go:build !windows

package packageprobe

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestStructuralGateRejectsFIFO(t *testing.T) {
	directory, root := buildPackage(t, minimalProblem(t))
	_ = root.Close()
	path := filepath.Join(directory, "statement", "zh-CN.md")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("FIFO unavailable: %v", err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := Inspect(context.Background(), root, testReadLimits()); err == nil {
		t.Fatal("FIFO package path was accepted")
	}
}
