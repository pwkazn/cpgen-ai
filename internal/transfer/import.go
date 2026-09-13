package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strings"

	"cpgen/internal/domain"
)

func Import(ctx context.Context, root *os.Root, input io.Reader, limits ImportLimits) (files []ImportedFile, returnErr error) {
	if root == nil || input == nil {
		return nil, fmt.Errorf("import root and input are required")
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	created := make([]domain.SafeRelPath, 0, limits.MaxFiles)
	defer func() {
		if returnErr == nil {
			return
		}
		for index := len(created) - 1; index >= 0; index-- {
			_ = root.Remove(string(created[index]))
		}
		files = nil
	}()

	seen := make(map[domain.SafeRelPath]struct{}, limits.MaxFiles)
	var total int64
	reader := contextReader{ctx: ctx, reader: input}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header, terminated, err := readFrameHeader(reader)
		if err != nil {
			return nil, err
		}
		if terminated {
			var trailing [1]byte
			if _, err := io.ReadFull(reader, trailing[:]); err != io.EOF {
				if err == nil {
					return nil, fmt.Errorf("transfer stream contains bytes after terminator")
				}
				return nil, fmt.Errorf("verify transfer stream terminator: %w", err)
			}
			return files, nil
		}
		if len(files) >= limits.MaxFiles {
			return nil, fmt.Errorf("import file count exceeds limit %d", limits.MaxFiles)
		}
		if _, exists := seen[header.Path]; exists {
			return nil, fmt.Errorf("duplicate import path %q", header.Path)
		}
		if header.Size > limits.MaxTotalBytes-total {
			return nil, fmt.Errorf("import bytes exceed limit %d", limits.MaxTotalBytes)
		}
		seen[header.Path] = struct{}{}
		if err := ensureDirectories(root, header.Path); err != nil {
			return nil, err
		}

		file, err := root.OpenFile(string(header.Path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return nil, fmt.Errorf("create import file %q: %w", header.Path, err)
		}
		created = append(created, header.Path)
		hash := sha256.New()
		_, copyErr := io.CopyN(io.MultiWriter(file, hash), reader, header.Size)
		if copyErr == nil {
			mode := os.FileMode(0o444)
			if header.Mode == FrameModeExecutable {
				mode = 0o555
			}
			copyErr = file.Chmod(mode)
		}
		if copyErr == nil {
			copyErr = file.Sync()
		}
		closeErr := file.Close()
		if copyErr != nil {
			return nil, fmt.Errorf("write import file %q: %w", header.Path, copyErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close import file %q: %w", header.Path, closeErr)
		}
		digest := domain.Digest("sha256:" + hex.EncodeToString(hash.Sum(nil)))
		files = append(files, ImportedFile{Path: header.Path, Digest: digest, Size: header.Size, Mode: header.Mode})
		total += header.Size
	}
}

func ensureDirectories(root *os.Root, filePath domain.SafeRelPath) error {
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
		info, err := root.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := root.Mkdir(current, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
				return fmt.Errorf("create import directory %q: %w", current, err)
			}
			info, err = root.Lstat(current)
		}
		if err != nil {
			return fmt.Errorf("inspect import directory %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("import parent %q is not a direct directory", current)
		}
	}
	return nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}
