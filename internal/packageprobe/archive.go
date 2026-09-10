package packageprobe

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"sort"

	"cpgen/internal/domain"
)

// BuildArchive creates a bounded, deterministic ZIP. Store avoids compressor
// version drift; all names, metadata and ordering are owned by the host.
func BuildArchive(ctx context.Context, problem Problem) ([]byte, Manifest, error) {
	if err := problem.Validate(buildMaxPathBytes); err != nil {
		return nil, Manifest{}, err
	}
	entries := make([]FileEntry, len(problem.Files))
	for i, file := range problem.Files {
		entries[i] = FileEntry{Path: file.Path, Role: file.Role, SHA256: domain.SumBytes(file.Data), Size: int64(len(file.Data))}
	}
	manifest, err := NewManifest(problem, entries)
	if err != nil {
		return nil, Manifest{}, err
	}
	files := make(map[domain.SafeRelPath][]byte, len(problem.Files)+1)
	for _, file := range problem.Files {
		files[file.Path] = file.Data
	}
	files["manifest.json"], err = EncodeManifest(manifest)
	if err != nil {
		return nil, Manifest{}, err
	}
	raw, err := encodeArchive(ctx, files)
	return raw, manifest, err
}

func encodeArchive(ctx context.Context, files map[domain.SafeRelPath][]byte) ([]byte, error) {
	if ctx == nil || len(files) > buildMaxFiles {
		return nil, errors.New("invalid package archive context or file count")
	}
	paths := make([]domain.SafeRelPath, 0, len(files))
	var total int64
	for path, raw := range files {
		if int64(len(raw)) > buildMaxFileBytes || total > buildMaxTotalBytes-int64(len(raw)) {
			return nil, errors.New("package archive exceeds byte bounds")
		}
		total += int64(len(raw))
		paths = append(paths, path)
	}
	if err := validatePackagePaths(paths, buildMaxPathBytes); err != nil {
		return nil, err
	}
	sort.Slice(paths, func(i, j int) bool { return paths[i] < paths[j] })
	var out bytes.Buffer
	w := zip.NewWriter(&out)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header := &zip.FileHeader{Name: string(path), Method: zip.Store, ModifiedDate: 33}
		header.SetMode(0o644)
		file, err := w.CreateHeader(header)
		if err != nil {
			return nil, err
		}
		if _, err := file.Write(files[path]); err != nil {
			return nil, err
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ReadArchive verifies bytes without extracting paths to the filesystem. It
// accepts only the canonical archive format emitted by BuildArchive, so hidden
// local entries, trailing payloads, symlinks and alternative encodings fail.
func ReadArchive(ctx context.Context, raw []byte) (VerifiedProblem, error) {
	var empty VerifiedProblem
	if ctx == nil || int64(len(raw)) > buildMaxTotalBytes+(1<<20) {
		return empty, errors.New("invalid package archive context or byte count")
	}
	r, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil || len(r.File) > buildMaxFiles {
		return empty, errors.New("invalid package ZIP or file count")
	}
	files := make(map[domain.SafeRelPath][]byte, len(r.File))
	var total int64
	for _, file := range r.File {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		path := domain.SafeRelPath(file.Name)
		if _, exists := files[path]; exists || !file.Mode().IsRegular() || file.Method != zip.Store || file.UncompressedSize64 > uint64(buildMaxFileBytes) || total > buildMaxTotalBytes-int64(file.UncompressedSize64) {
			return empty, errors.New("invalid package ZIP entry")
		}
		handle, err := file.Open()
		if err != nil {
			return empty, err
		}
		data, readErr := io.ReadAll(io.LimitReader(handle, int64(file.UncompressedSize64)+1))
		if err := errors.Join(readErr, handle.Close()); err != nil {
			return empty, err
		}
		if uint64(len(data)) != file.UncompressedSize64 {
			return empty, errors.New("package ZIP entry size differs")
		}
		total += int64(len(data))
		files[path] = data
	}
	manifest, err := DecodeManifest(files["manifest.json"])
	if err != nil || len(files) != len(manifest.Files)+1 {
		return empty, errors.New("package ZIP manifest membership differs")
	}
	verified := VerifiedProblem{Manifest: manifest}
	for _, entry := range manifest.Files {
		if _, exists := files[entry.Path]; !exists {
			return empty, errors.New("package ZIP omitted a manifest file")
		}
		verified.Files = append(verified.Files, VerifiedFile{Entry: entry, Bytes: files[entry.Path]})
	}
	if err := verified.Validate(); err != nil {
		return empty, err
	}
	canonical, err := encodeArchive(ctx, files)
	if err != nil || !bytes.Equal(raw, canonical) {
		return empty, errors.New("package ZIP is not canonical")
	}
	return verified, nil
}
