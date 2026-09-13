package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"cpgen/internal/domain"
	"cpgen/internal/securefs"
)

type openedExport struct {
	path domain.SafeRelPath
	size int64
	mode FrameMode
	file *os.File
}

func Export(ctx context.Context, root *os.Root, plan ExportPlan, output io.Writer) ([]ExportedFile, error) {
	if root == nil || output == nil {
		return nil, fmt.Errorf("export root and output are required")
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	opened := make([]openedExport, 0, len(plan.Files))
	defer func() {
		for _, item := range opened {
			_ = item.file.Close()
		}
	}()

	var total int64
	for _, declaration := range plan.Files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, info, err := securefs.OpenRegular(root, declaration.Path, true)
		if err != nil {
			return nil, fmt.Errorf("open export file %q: %w", declaration.Path, err)
		}
		size := info.Size()
		if size < 0 || size > declaration.MaxBytes {
			_ = file.Close()
			return nil, fmt.Errorf("export file %q size %d exceeds limit %d", declaration.Path, size, declaration.MaxBytes)
		}
		if size > plan.MaxTotalBytes-total {
			_ = file.Close()
			return nil, fmt.Errorf("export bytes exceed limit %d", plan.MaxTotalBytes)
		}
		total += size
		mode := FrameModeRegular
		if info.Mode().Perm()&0o111 != 0 {
			mode = FrameModeExecutable
		}
		opened = append(opened, openedExport{path: declaration.Path, size: size, mode: mode, file: file})
	}

	files := make([]ExportedFile, 0, len(opened))
	writer := contextWriter{ctx: ctx, writer: output}
	for _, item := range opened {
		header := FrameHeader{SchemaVersion: FrameSchemaVersion, Path: item.path, Size: item.size, Mode: item.mode}
		if err := writeFrameHeader(writer, header); err != nil {
			return nil, err
		}
		hash := sha256.New()
		if _, err := io.CopyN(io.MultiWriter(writer, hash), item.file, item.size); err != nil {
			return nil, fmt.Errorf("stream export file %q: %w", item.path, err)
		}
		var extra [1]byte
		if count, err := item.file.Read(extra[:]); count != 0 || err != io.EOF {
			if err == nil {
				return nil, fmt.Errorf("export file %q grew after verification", item.path)
			}
			return nil, fmt.Errorf("verify export file %q end: %w", item.path, err)
		}
		digest := domain.Digest("sha256:" + hex.EncodeToString(hash.Sum(nil)))
		files = append(files, ExportedFile{Path: item.path, Digest: digest, Size: item.size, Mode: item.mode})
	}
	if err := writeTerminator(writer); err != nil {
		return nil, err
	}
	return files, nil
}

type contextWriter struct {
	ctx    context.Context
	writer io.Writer
}

func (w contextWriter) Write(buffer []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.writer.Write(buffer)
}
