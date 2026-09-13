package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPackageExportPublishesCompleteFileWithoutReplacingDestination(t *testing.T) {
	root := t.TempDir()
	destination := filepath.Join(root, "problem.zip")
	first, second := bytes.Repeat([]byte("first"), 32768), bytes.Repeat([]byte("second"), 32768)
	var group sync.WaitGroup
	errs := make(chan error, 2)
	for _, raw := range [][]byte{first, second} {
		group.Add(1)
		go func(raw []byte) { defer group.Done(); errs <- publishPackageExport(destination, raw) }(raw)
	}
	group.Wait()
	close(errs)
	winners := 0
	for err := range errs {
		if err == nil {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("exclusive export winners: %d", winners)
	}
	actual, err := os.ReadFile(destination)
	if err != nil || (!bytes.Equal(actual, first) && !bytes.Equal(actual, second)) {
		t.Fatalf("partial or mixed export: %v", err)
	}
	if err := publishPackageExport(destination, []byte("replacement")); err == nil {
		t.Fatal("existing destination was replaced")
	}
	again, err := os.ReadFile(destination)
	if err != nil || !bytes.Equal(actual, again) {
		t.Fatal("failed export changed destination")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 1 || entries[0].Name() != "problem.zip" {
		t.Fatalf("export leaked temporary files: %v %v", entries, err)
	}
}

func TestPackageExportDoesNotFollowDestinationSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "target")
	if err := os.WriteFile(target, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(root, "problem.zip")
	if err := os.Symlink(target, destination); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := publishPackageExport(destination, []byte("changed")); err == nil {
		t.Fatal("export followed destination symlink")
	}
	raw, err := os.ReadFile(target)
	if err != nil || string(raw) != "preserve" {
		t.Fatal("export changed symlink target")
	}
}
