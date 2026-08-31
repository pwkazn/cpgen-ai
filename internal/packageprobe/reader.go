package packageprobe

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"cpgen/internal/domain"
	"cpgen/internal/securefs"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

type openedFile struct {
	path domain.SafeRelPath
	file *os.File
	info os.FileInfo
}

type openedPackage struct {
	manifest      Manifest
	manifestBytes []byte
	manifestFile  openedFile
	files         []openedFile
}

func (p *openedPackage) close() error {
	if p == nil {
		return nil
	}
	var failures []error
	if p.manifestFile.file != nil {
		failures = append(failures, p.manifestFile.file.Close())
	}
	for _, file := range p.files {
		failures = append(failures, file.file.Close())
	}
	return errorsJoin(failures)
}

func Inspect(ctx context.Context, root *os.Root, limits ReadLimits) (VerifiedProblem, error) {
	opened, err := openPackage(ctx, root, limits)
	if err != nil {
		return VerifiedProblem{}, err
	}
	defer opened.close()
	return inspectOpened(ctx, opened)
}

func openPackage(ctx context.Context, root *os.Root, limits ReadLimits) (*openedPackage, error) {
	if ctx == nil || root == nil {
		return nil, fmt.Errorf("package inspect context and root are required")
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	result := &openedPackage{}
	all := make([]openedFile, 0)
	paths := make([]domain.SafeRelPath, 0)
	var total int64
	err := fs.WalkDir(root.FS(), ".", func(rawPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if rawPath == "." {
			return nil
		}
		safePath := domain.SafeRelPath(rawPath)
		if err := validatePackagePaths([]domain.SafeRelPath{safePath}, limits.MaxPathBytes); err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("package path %q is a symbolic link", rawPath)
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type() != 0 && !entry.Type().IsRegular() {
			return fmt.Errorf("package path %q is not a regular file", rawPath)
		}
		if len(all)+1 > limits.MaxFiles {
			return fmt.Errorf("package exceeds %d files", limits.MaxFiles)
		}
		file, info, err := securefs.OpenRegular(root, safePath, true)
		if err != nil {
			return err
		}
		if info.Size() < 0 || info.Size() > limits.MaxFileBytes || total > limits.MaxTotalBytes-info.Size() {
			_ = file.Close()
			return fmt.Errorf("package file %q exceeds byte limits", rawPath)
		}
		total += info.Size()
		all = append(all, openedFile{path: safePath, file: file, info: info})
		paths = append(paths, safePath)
		return nil
	})
	if err != nil {
		for _, file := range all {
			_ = file.file.Close()
		}
		return nil, err
	}
	if err := validatePackagePaths(paths, limits.MaxPathBytes); err != nil {
		for _, file := range all {
			_ = file.file.Close()
		}
		return nil, err
	}
	for _, file := range all {
		if file.path == "manifest.json" {
			result.manifestFile = file
		} else {
			result.files = append(result.files, file)
		}
	}
	if result.manifestFile.file == nil {
		_ = result.close()
		return nil, fmt.Errorf("package manifest.json is missing")
	}
	if result.manifestFile.info.Size() > limits.MaxManifestBytes {
		_ = result.close()
		return nil, fmt.Errorf("package manifest exceeds %d bytes", limits.MaxManifestBytes)
	}
	result.manifestBytes, err = readExactHandle(result.manifestFile, result.manifestFile.info.Size())
	if err != nil {
		_ = result.close()
		return nil, err
	}
	if err := decodeStrict(result.manifestBytes, &result.manifest); err != nil {
		_ = result.close()
		return nil, fmt.Errorf("decode package manifest: %w", err)
	}
	if err := result.manifest.Validate(limits.MaxPathBytes); err != nil {
		_ = result.close()
		return nil, err
	}
	if len(result.files) != len(result.manifest.Files) {
		_ = result.close()
		return nil, fmt.Errorf("package tree file count does not match the manifest")
	}
	sort.Slice(result.files, func(left, right int) bool { return result.files[left].path < result.files[right].path })
	for index, entry := range result.manifest.Files {
		if result.files[index].path != entry.Path {
			_ = result.close()
			return nil, fmt.Errorf("package tree has missing or undeclared file near %q", entry.Path)
		}
		if result.files[index].info.Size() != entry.Size {
			_ = result.close()
			return nil, fmt.Errorf("package file %q size does not match manifest", entry.Path)
		}
	}
	return result, nil
}

func inspectOpened(ctx context.Context, opened *openedPackage) (VerifiedProblem, error) {
	verified := VerifiedProblem{Manifest: opened.manifest, Files: make([]VerifiedFile, len(opened.files))}
	for index, file := range opened.files {
		if err := ctx.Err(); err != nil {
			return VerifiedProblem{}, err
		}
		entry := opened.manifest.Files[index]
		data, err := readExactHandle(file, entry.Size)
		if err != nil {
			return VerifiedProblem{}, err
		}
		if domain.SumBytes(data) != entry.SHA256 {
			return VerifiedProblem{}, fmt.Errorf("package file %q digest does not match manifest", entry.Path)
		}
		verified.Files[index] = VerifiedFile{Entry: entry, Bytes: data}
	}
	if err := verified.Validate(); err != nil {
		return VerifiedProblem{}, err
	}
	return verified, nil
}

func readExactHandle(file openedFile, expected int64) ([]byte, error) {
	if _, err := file.file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file.file, expected+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != expected {
		return nil, fmt.Errorf("package file %q changed size while reading", file.path)
	}
	return data, nil
}

func validatePackagePaths(paths []domain.SafeRelPath, maxPathBytes int) error {
	if maxPathBytes <= 0 {
		return fmt.Errorf("package path byte limit must be positive")
	}
	folder := cases.Fold()
	seen := make(map[string]domain.SafeRelPath, len(paths))
	for _, candidate := range paths {
		if err := candidate.Validate(); err != nil {
			return err
		}
		raw := string(candidate)
		if len([]byte(raw)) > maxPathBytes || !utf8.ValidString(raw) || norm.NFC.String(raw) != raw {
			return fmt.Errorf("package path %q is not bounded NFC UTF-8", raw)
		}
		for _, value := range raw {
			if unicode.IsControl(value) {
				return fmt.Errorf("package path %q contains a control character", raw)
			}
		}
		folded := folder.String(raw)
		if previous, exists := seen[folded]; exists {
			return fmt.Errorf("package paths %q and %q collide after case folding", previous, candidate)
		}
		seen[folded] = candidate
	}
	return nil
}

func cloneVerifiedProblem(problem VerifiedProblem) VerifiedProblem {
	result := problem
	result.Manifest.Files = slices.Clone(problem.Manifest.Files)
	result.Manifest.TestGroups = cloneGroups(problem.Manifest.TestGroups)
	result.Files = make([]VerifiedFile, len(problem.Files))
	for index, file := range problem.Files {
		result.Files[index] = VerifiedFile{Entry: file.Entry, Bytes: slices.Clone(file.Bytes)}
	}
	return result
}

func errorsJoin(input []error) error {
	var messages []string
	for _, err := range input {
		if err != nil {
			messages = append(messages, err.Error())
		}
	}
	if len(messages) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(messages, "; "))
}
