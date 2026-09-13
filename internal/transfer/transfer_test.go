package transfer_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/transfer"
)

func TestImportWritesBoundedReadOnlyFilesAndHashesExactBytes(t *testing.T) {
	var input bytes.Buffer
	appendFrame(t, &input, transfer.FrameHeader{
		SchemaVersion: transfer.FrameSchemaVersion,
		Path:          domain.SafeRelPath("nested/input.txt"),
		Size:          3,
		Mode:          transfer.FrameModeRegular,
	}, 3, []byte("abc"))
	appendFrame(t, &input, transfer.FrameHeader{
		SchemaVersion: transfer.FrameSchemaVersion,
		Path:          domain.SafeRelPath("program"),
		Size:          0,
		Mode:          transfer.FrameModeRegular,
	}, 0, nil)
	appendTerminator(t, &input)

	rootPath := t.TempDir()
	root := openRoot(t, rootPath)
	files, err := transfer.Import(context.Background(), root, &input, transfer.ImportLimits{MaxFiles: 2, MaxTotalBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Digest != domain.SumBytes([]byte("abc")) || files[1].Size != 0 {
		t.Fatalf("unexpected imported files: %#v", files)
	}
	got, err := os.ReadFile(filepath.Join(rootPath, "nested", "input.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "abc" {
		t.Fatalf("imported bytes = %q", got)
	}
	info, err := os.Stat(filepath.Join(rootPath, "nested", "input.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o222 != 0 {
		t.Fatalf("imported mode = %o, want read-only", info.Mode().Perm())
	}
}

func TestImportRejectsMalformedOrOverLimitStreams(t *testing.T) {
	tests := []struct {
		name  string
		input func(*testing.T) []byte
		limit transfer.ImportLimits
	}{
		{
			name: "duplicate path",
			input: func(t *testing.T) []byte {
				var buffer bytes.Buffer
				header := transfer.FrameHeader{SchemaVersion: transfer.FrameSchemaVersion, Path: "same", Size: 1, Mode: transfer.FrameModeRegular}
				appendFrame(t, &buffer, header, 1, []byte("a"))
				appendFrame(t, &buffer, header, 1, []byte("b"))
				appendTerminator(t, &buffer)
				return buffer.Bytes()
			},
			limit: transfer.ImportLimits{MaxFiles: 2, MaxTotalBytes: 2},
		},
		{
			name: "declared-size underflow",
			input: func(t *testing.T) []byte {
				var buffer bytes.Buffer
				appendFrame(t, &buffer, transfer.FrameHeader{SchemaVersion: transfer.FrameSchemaVersion, Path: "short", Size: 3, Mode: transfer.FrameModeRegular}, 3, []byte("ab"))
				return buffer.Bytes()
			},
			limit: transfer.ImportLimits{MaxFiles: 1, MaxTotalBytes: 3},
		},
		{
			name: "declared-size overflow",
			input: func(t *testing.T) []byte {
				var buffer bytes.Buffer
				appendFrame(t, &buffer, transfer.FrameHeader{SchemaVersion: transfer.FrameSchemaVersion, Path: "long", Size: 1, Mode: transfer.FrameModeRegular}, 2, []byte("ab"))
				appendTerminator(t, &buffer)
				return buffer.Bytes()
			},
			limit: transfer.ImportLimits{MaxFiles: 1, MaxTotalBytes: 2},
		},
		{
			name: "total limit plus one",
			input: func(t *testing.T) []byte {
				var buffer bytes.Buffer
				appendFrame(t, &buffer, transfer.FrameHeader{SchemaVersion: transfer.FrameSchemaVersion, Path: "large", Size: 4, Mode: transfer.FrameModeRegular}, 4, []byte("abcd"))
				appendTerminator(t, &buffer)
				return buffer.Bytes()
			},
			limit: transfer.ImportLimits{MaxFiles: 1, MaxTotalBytes: 3},
		},
		{
			name: "unknown header field",
			input: func(t *testing.T) []byte {
				var buffer bytes.Buffer
				appendFrame(t, &buffer, map[string]any{
					"schema_version": transfer.FrameSchemaVersion, "path": "file", "size": 0,
					"mode": transfer.FrameModeRegular, "future": true,
				}, 0, nil)
				appendTerminator(t, &buffer)
				return buffer.Bytes()
			},
			limit: transfer.ImportLimits{MaxFiles: 1, MaxTotalBytes: 1},
		},
		{
			name: "bytes after terminator",
			input: func(t *testing.T) []byte {
				var buffer bytes.Buffer
				appendTerminator(t, &buffer)
				buffer.WriteByte(1)
				return buffer.Bytes()
			},
			limit: transfer.ImportLimits{MaxFiles: 1, MaxTotalBytes: 1},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			root := openRoot(t, rootPath)
			if _, err := transfer.Import(context.Background(), root, bytes.NewReader(test.input(t)), test.limit); err == nil {
				t.Fatal("invalid transfer stream was accepted")
			}
			entries, err := os.ReadDir(rootPath)
			if err != nil {
				t.Fatal(err)
			}
			if test.name == "duplicate path" && len(entries) != 0 {
				t.Fatalf("unexpected root entries after rollback: %#v", entries)
			}
		})
	}
}

func TestExportPreflightsDeclaredRegularFilesAndStreamsExactHandles(t *testing.T) {
	rootPath := t.TempDir()
	if err := os.Mkdir(filepath.Join(rootPath, "result"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "result", "answer.txt"), []byte("42\n"), 0o444); err != nil {
		t.Fatal(err)
	}
	root := openRoot(t, rootPath)
	plan := transfer.ExportPlan{
		SchemaVersion: transfer.ExportPlanSchemaVersion,
		Files:         []port.OutputDeclaration{{Path: "result/answer.txt", MaxBytes: 3}},
		MaxFiles:      1,
		MaxTotalBytes: 3,
	}
	var output bytes.Buffer
	files, err := transfer.Export(context.Background(), root, plan, &output)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Digest != domain.SumBytes([]byte("42\n")) {
		t.Fatalf("unexpected exported files: %#v", files)
	}
	header, data := readOneFrame(t, &output)
	if header.Path != "result/answer.txt" || header.Size != 3 || string(data) != "42\n" {
		t.Fatalf("unexpected exported frame: %#v %q", header, data)
	}
	var terminator uint32
	if err := binary.Read(&output, binary.BigEndian, &terminator); err != nil || terminator != 0 || output.Len() != 0 {
		t.Fatalf("invalid export terminator: value=%d err=%v remaining=%d", terminator, err, output.Len())
	}
}

func TestExportRejectsDuplicateSpecialAndOverLimitFilesBeforeWriting(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*testing.T, string)
		files   []port.OutputDeclaration
		max     int64
	}{
		{
			name:    "duplicate declaration",
			prepare: func(t *testing.T, root string) { writeFile(t, root, "file", "x") },
			files:   []port.OutputDeclaration{{Path: "file", MaxBytes: 1}, {Path: "file", MaxBytes: 1}}, max: 2,
		},
		{
			name: "directory",
			prepare: func(t *testing.T, root string) {
				if err := os.Mkdir(filepath.Join(root, "directory"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			files: []port.OutputDeclaration{{Path: "directory", MaxBytes: 1}}, max: 1,
		},
		{
			name:    "file limit plus one",
			prepare: func(t *testing.T, root string) { writeFile(t, root, "large", "ab") },
			files:   []port.OutputDeclaration{{Path: "large", MaxBytes: 1}}, max: 2,
		},
		{
			name:    "zero file limit",
			prepare: func(t *testing.T, root string) { writeFile(t, root, "empty", "") },
			files:   []port.OutputDeclaration{{Path: "empty", MaxBytes: 0}}, max: 0,
		},
		{
			name:    "aggregate limit plus one",
			prepare: func(t *testing.T, root string) { writeFile(t, root, "large", "ab") },
			files:   []port.OutputDeclaration{{Path: "large", MaxBytes: 2}}, max: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			test.prepare(t, rootPath)
			root := openRoot(t, rootPath)
			var output bytes.Buffer
			_, err := transfer.Export(context.Background(), root, transfer.ExportPlan{
				SchemaVersion: transfer.ExportPlanSchemaVersion,
				Files:         test.files, MaxFiles: len(test.files), MaxTotalBytes: test.max,
			}, &output)
			if err == nil {
				t.Fatal("unsafe export plan was accepted")
			}
			if output.Len() != 0 {
				t.Fatalf("export wrote %d bytes before preflight failed", output.Len())
			}
		})
	}
}

func TestKeepReturnsAfterContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := transfer.Keep(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestImportSupportsOnlyFixedReadOnlyAndExecutableModes(t *testing.T) {
	var input bytes.Buffer
	appendFrame(t, &input, transfer.FrameHeader{
		SchemaVersion: transfer.FrameSchemaVersion,
		Path:          "program/main",
		Size:          3,
		Mode:          transfer.FrameModeExecutable,
	}, 3, []byte("bin"))
	appendTerminator(t, &input)
	rootPath := t.TempDir()
	root := openRoot(t, rootPath)
	files, err := transfer.Import(context.Background(), root, &input, transfer.ImportLimits{MaxFiles: 1, MaxTotalBytes: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Mode != transfer.FrameModeExecutable {
		t.Fatalf("imported files = %#v", files)
	}
	info, err := os.Stat(filepath.Join(rootPath, "program", "main"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o555 {
		t.Fatalf("executable mode = %o, want 555", info.Mode().Perm())
	}
}

func TestWriteAndReceiveExportStreamRoutesOnlyDeclaredFiles(t *testing.T) {
	files := []transfer.StreamFile{
		{Path: "files/main", Mode: transfer.FrameModeExecutable, Data: []byte("binary")},
		{Path: "files/log.txt", Mode: transfer.FrameModeRegular, Data: []byte("log")},
	}
	var stream bytes.Buffer
	written, err := transfer.WriteStream(context.Background(), &stream, files)
	if err != nil {
		t.Fatal(err)
	}
	var program, log bytes.Buffer
	received, err := transfer.ReceiveExport(context.Background(), &stream, []transfer.ReceiveTarget{
		{Declaration: port.OutputDeclaration{Path: "files/main", MaxBytes: 6}, Writer: &program},
		{Declaration: port.OutputDeclaration{Path: "files/log.txt", MaxBytes: 3}, Writer: &log},
	}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if string(program.Bytes()) != "binary" || string(log.Bytes()) != "log" || len(written) != 2 || len(received) != 2 {
		t.Fatalf("written=%#v received=%#v program=%q log=%q", written, received, program.Bytes(), log.Bytes())
	}

	var unexpected bytes.Buffer
	if _, err := transfer.WriteStream(context.Background(), &unexpected, []transfer.StreamFile{{Path: "files/extra", Mode: transfer.FrameModeRegular, Data: nil}}); err != nil {
		t.Fatal(err)
	}
	if _, err := transfer.ReceiveExport(context.Background(), &unexpected, []transfer.ReceiveTarget{{
		Declaration: port.OutputDeclaration{Path: "files/main", MaxBytes: 1}, Writer: io.Discard,
	}}, 1); err == nil {
		t.Fatal("unexpected export path was accepted")
	}
}

func appendFrame(t *testing.T, destination *bytes.Buffer, header any, dataLength uint64, data []byte) {
	t.Helper()
	headerBytes, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	if err := binary.Write(destination, binary.BigEndian, uint32(len(headerBytes))); err != nil {
		t.Fatal(err)
	}
	destination.Write(headerBytes)
	if err := binary.Write(destination, binary.BigEndian, dataLength); err != nil {
		t.Fatal(err)
	}
	destination.Write(data)
}

func appendTerminator(t *testing.T, destination *bytes.Buffer) {
	t.Helper()
	if err := binary.Write(destination, binary.BigEndian, uint32(0)); err != nil {
		t.Fatal(err)
	}
}

func readOneFrame(t *testing.T, source io.Reader) (transfer.FrameHeader, []byte) {
	t.Helper()
	var headerLength uint32
	if err := binary.Read(source, binary.BigEndian, &headerLength); err != nil {
		t.Fatal(err)
	}
	headerBytes := make([]byte, headerLength)
	if _, err := io.ReadFull(source, headerBytes); err != nil {
		t.Fatal(err)
	}
	var header transfer.FrameHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		t.Fatal(err)
	}
	var dataLength uint64
	if err := binary.Read(source, binary.BigEndian, &dataLength); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, dataLength)
	if _, err := io.ReadFull(source, data); err != nil {
		t.Fatal(err)
	}
	return header, data
}

func openRoot(t *testing.T, path string) *os.Root {
	t.Helper()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func writeFile(t *testing.T, root, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o444); err != nil {
		t.Fatal(err)
	}
}
