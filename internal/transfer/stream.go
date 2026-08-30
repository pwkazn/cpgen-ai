package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type StreamFile struct {
	Path domain.SafeRelPath
	Mode FrameMode
	Data []byte
}

func WriteStream(ctx context.Context, output io.Writer, files []StreamFile) ([]ExportedFile, error) {
	if ctx == nil || output == nil {
		return nil, fmt.Errorf("stream context and output are required")
	}
	seen := make(map[domain.SafeRelPath]struct{}, len(files))
	for index, file := range files {
		if err := file.Path.Validate(); err != nil {
			return nil, fmt.Errorf("stream file %d: %w", index, err)
		}
		if !file.Mode.Valid() {
			return nil, fmt.Errorf("stream file %d has invalid mode %q", index, file.Mode)
		}
		if _, exists := seen[file.Path]; exists {
			return nil, fmt.Errorf("duplicate stream path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
	}
	writer := contextWriter{ctx: ctx, writer: output}
	result := make([]ExportedFile, 0, len(files))
	for _, file := range files {
		header := FrameHeader{SchemaVersion: FrameSchemaVersion, Path: file.Path, Size: int64(len(file.Data)), Mode: file.Mode}
		if err := writeFrameHeader(writer, header); err != nil {
			return nil, err
		}
		if _, err := writer.Write(file.Data); err != nil {
			return nil, fmt.Errorf("write stream file %q: %w", file.Path, err)
		}
		result = append(result, ExportedFile{Path: file.Path, Digest: domain.SumBytes(file.Data), Size: int64(len(file.Data)), Mode: file.Mode})
	}
	if err := writeTerminator(writer); err != nil {
		return nil, err
	}
	return result, nil
}

type ReceiveTarget struct {
	Declaration port.OutputDeclaration
	Writer      io.Writer
}

func ReceiveExport(ctx context.Context, input io.Reader, targets []ReceiveTarget, maxTotalBytes int64) ([]ExportedFile, error) {
	if ctx == nil || input == nil {
		return nil, fmt.Errorf("receive context and input are required")
	}
	if maxTotalBytes < 0 {
		return nil, fmt.Errorf("maximum export bytes must be non-negative")
	}
	expected := make(map[domain.SafeRelPath]ReceiveTarget, len(targets))
	for index, target := range targets {
		if err := target.Declaration.Path.Validate(); err != nil {
			return nil, fmt.Errorf("receive target %d: %w", index, err)
		}
		if target.Declaration.MaxBytes <= 0 || target.Writer == nil {
			return nil, fmt.Errorf("receive target %d has no positive limit or writer", index)
		}
		if _, exists := expected[target.Declaration.Path]; exists {
			return nil, fmt.Errorf("duplicate receive target %q", target.Declaration.Path)
		}
		expected[target.Declaration.Path] = target
	}

	reader := contextReader{ctx: ctx, reader: input}
	received := make(map[domain.SafeRelPath]struct{}, len(targets))
	result := make([]ExportedFile, 0, len(targets))
	var total int64
	for {
		header, terminated, err := readFrameHeader(reader)
		if err != nil {
			return nil, err
		}
		if terminated {
			var trailing [1]byte
			if _, err := io.ReadFull(reader, trailing[:]); err != io.EOF {
				if err == nil {
					return nil, fmt.Errorf("export stream contains bytes after terminator")
				}
				return nil, fmt.Errorf("verify export stream terminator: %w", err)
			}
			if len(received) != len(expected) {
				return nil, fmt.Errorf("export stream ended after %d of %d declared files", len(received), len(expected))
			}
			return result, nil
		}
		target, exists := expected[header.Path]
		if !exists {
			return nil, fmt.Errorf("export stream contains undeclared path %q", header.Path)
		}
		if _, duplicate := received[header.Path]; duplicate {
			return nil, fmt.Errorf("export stream repeats path %q", header.Path)
		}
		if header.Size > target.Declaration.MaxBytes {
			return nil, fmt.Errorf("export path %q exceeds its byte limit", header.Path)
		}
		if header.Size > maxTotalBytes-total {
			return nil, fmt.Errorf("export stream exceeds total byte limit %d", maxTotalBytes)
		}
		hash := sha256.New()
		if _, err := io.CopyN(io.MultiWriter(target.Writer, hash), reader, header.Size); err != nil {
			return nil, fmt.Errorf("receive export path %q: %w", header.Path, err)
		}
		total += header.Size
		received[header.Path] = struct{}{}
		result = append(result, ExportedFile{
			Path: header.Path, Size: header.Size, Mode: header.Mode,
			Digest: domain.Digest("sha256:" + hex.EncodeToString(hash.Sum(nil))),
		})
	}
}
