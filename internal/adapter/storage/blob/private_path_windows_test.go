//go:build windows

package blob

import (
	"errors"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestEnsurePrivateDirectoryTreeRetainsAncestorsUntilComplete(t *testing.T) {
	root := filepath.Join("C:\\private", "artifacts")
	target := filepath.Join(root, "blobs", "sha256", "ab")
	const expectedCreates = 3
	var createCalls int
	var earlyClose bool
	var completed bool
	var nextHandle windows.Handle = 1

	createDirectory := func(string) error {
		if earlyClose {
			return errors.New("simulated junction replacement after ancestor close")
		}
		createCalls++
		return nil
	}
	openDirectory := func(string) (windows.Handle, error) {
		handle := nextHandle
		nextHandle++
		return handle, nil
	}
	closeHandles := func(handles []windows.Handle) {
		for range handles {
			if !completed && createCalls < expectedCreates {
				earlyClose = true
			}
		}
	}

	if err := ensurePrivateDirectoryTreeWithOps(root, target, createDirectory, openDirectory, func(handles []windows.Handle) {
		closeHandles(handles)
		completed = true
	}); err != nil {
		t.Fatalf("ensurePrivateDirectoryTreeWithOps() error = %v", err)
	}
	if earlyClose {
		t.Fatal("ancestor handle was closed before nested directory creation completed")
	}
	if createCalls != expectedCreates {
		t.Fatalf("CreateDirectory calls = %d, want %d", createCalls, expectedCreates)
	}
}
