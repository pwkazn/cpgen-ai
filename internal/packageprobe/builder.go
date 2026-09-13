package packageprobe

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"

	"cpgen/internal/domain"
)

const (
	buildMaxPathBytes  = 240
	buildMaxFiles      = 256
	buildMaxFileBytes  = int64(16 << 20)
	buildMaxTotalBytes = int64(64 << 20)
)

func Build(ctx context.Context, root *os.Root, problem Problem) (Manifest, error) {
	if ctx == nil || root == nil {
		return Manifest{}, fmt.Errorf("package build context and root are required")
	}
	if err := ctx.Err(); err != nil {
		return Manifest{}, err
	}
	if err := problem.Validate(buildMaxPathBytes); err != nil {
		return Manifest{}, err
	}
	if len(problem.Files)+1 > buildMaxFiles {
		return Manifest{}, fmt.Errorf("package contains too many files")
	}
	if err := requireEmptyRoot(root); err != nil {
		return Manifest{}, err
	}
	files := make([]ProblemFile, len(problem.Files))
	copy(files, problem.Files)
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	entries := make([]FileEntry, 0, len(files))
	var total int64
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return Manifest{}, err
		}
		if int64(len(file.Data)) > buildMaxFileBytes || total > buildMaxTotalBytes-int64(len(file.Data)) {
			return Manifest{}, fmt.Errorf("package file or total byte limit exceeded")
		}
		if err := ensurePackageParents(root, file.Path); err != nil {
			return Manifest{}, err
		}
		if err := writeExclusive(root, file.Path, file.Data); err != nil {
			return Manifest{}, err
		}
		total += int64(len(file.Data))
		entries = append(entries, FileEntry{Path: file.Path, SHA256: domain.SumBytes(file.Data), Size: int64(len(file.Data)), Role: file.Role})
	}
	manifest, err := NewManifest(problem, entries)
	if err != nil {
		return Manifest{}, err
	}
	manifestBytes, err := EncodeManifest(manifest)
	if err != nil {
		return Manifest{}, err
	}
	if int64(len(manifestBytes)) > buildMaxFileBytes || total > buildMaxTotalBytes-int64(len(manifestBytes)) {
		return Manifest{}, fmt.Errorf("manifest exceeds package build limits")
	}
	if err := writeExclusive(root, "manifest.json", manifestBytes); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func requireEmptyRoot(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("package build root must be empty")
	}
	return nil
}

func ensurePackageParents(root *os.Root, filePath domain.SafeRelPath) error {
	parent := path.Dir(string(filePath))
	if parent == "." {
		return nil
	}
	current := ""
	for _, component := range strings.Split(parent, "/") {
		if current == "" {
			current = component
		} else {
			current += "/" + component
		}
		if err := root.Mkdir(current, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("package parent %q is not a direct directory", current)
		}
	}
	return nil
}

func writeExclusive(root *os.Root, filePath domain.SafeRelPath, data []byte) (returnErr error) {
	file, err := root.OpenFile(string(filePath), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() {
		returnErr = errors.Join(returnErr, file.Close())
	}()
	written, err := file.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return fmt.Errorf("short package write for %q", filePath)
	}
	return file.Sync()
}
