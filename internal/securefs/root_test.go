package securefs_test

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/securefs"
)

func TestOpenRegularReadsFromTheVerifiedHandle(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	originalPath := filepath.Join(directory, "nested", "answer.txt")
	if err := os.WriteFile(originalPath, []byte("verified bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	file, info, err := securefs.OpenRegular(root, domain.SafeRelPath("nested/answer.txt"), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	handleInfo, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(info, handleInfo) {
		t.Fatal("returned metadata does not identify the returned handle")
	}

	movedPath := filepath.Join(directory, "nested", "moved.txt")
	if err := os.Rename(originalPath, movedPath); err != nil {
		t.Fatalf("rename verified path while handle is open: %v", err)
	}
	if err := os.WriteFile(originalPath, []byte("replacement"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "verified bytes" {
		t.Fatalf("read %q, want bytes from the verified handle", got)
	}
}

func TestOpenRegularRejectsUnsafePathsAndFileTypes(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "file.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(directory, "directory"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	for _, unsafe := range []domain.SafeRelPath{"", "../file.txt", "/file.txt", `C:\\file.txt`, "nested/../../file.txt"} {
		if file, _, err := securefs.OpenRegular(root, unsafe, true); err == nil {
			_ = file.Close()
			t.Fatalf("unsafe path %q was accepted", unsafe)
		}
	}
	if file, _, err := securefs.OpenRegular(root, domain.SafeRelPath("directory"), true); err == nil {
		_ = file.Close()
		t.Fatal("directory was accepted as a regular file")
	}
}

func TestOpenRegularRejectsSymlinkAndHardlink(t *testing.T) {
	directory := t.TempDir()
	original := filepath.Join(directory, "original.txt")
	if err := os.WriteFile(original, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })

	symlink := filepath.Join(directory, "symlink.txt")
	if err := os.Symlink("original.txt", symlink); err == nil {
		if file, _, err := securefs.OpenRegular(root, domain.SafeRelPath("symlink.txt"), true); err == nil {
			_ = file.Close()
			t.Fatal("symlink was accepted")
		}
	} else {
		t.Logf("symlink test unavailable: %v", err)
	}

	hardlink := filepath.Join(directory, "hardlink.txt")
	if err := os.Link(original, hardlink); err != nil {
		t.Skipf("hardlinks unavailable: %v", err)
	}
	if file, _, err := securefs.OpenRegular(root, domain.SafeRelPath("original.txt"), true); err == nil {
		_ = file.Close()
		t.Fatal("multiply-linked file was accepted when a single link was required")
	}
	file, _, err := securefs.OpenRegular(root, domain.SafeRelPath("hardlink.txt"), false)
	if err != nil {
		t.Fatalf("regular hardlink rejected when link count was not required: %v", err)
	}
	_ = file.Close()
}
