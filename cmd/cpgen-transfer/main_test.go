package main

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/transfer"
)

func TestRunImportAcceptsOnlyExplicitRootAndLimits(t *testing.T) {
	root := t.TempDir()
	var input bytes.Buffer
	header, err := json.Marshal(transfer.FrameHeader{
		SchemaVersion: transfer.FrameSchemaVersion,
		Path:          domain.SafeRelPath("input.txt"),
		Size:          1,
		Mode:          transfer.FrameModeRegular,
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = binary.Write(&input, binary.BigEndian, uint32(len(header)))
	input.Write(header)
	_ = binary.Write(&input, binary.BigEndian, uint64(1))
	input.WriteByte('x')
	_ = binary.Write(&input, binary.BigEndian, uint32(0))

	var stdout, stderr bytes.Buffer
	code := run([]string{"import", "--root", root, "--max-files", "1", "--max-total-bytes", "1"}, &input, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	got, err := os.ReadFile(filepath.Join(root, "input.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "x" {
		t.Fatalf("imported bytes = %q", got)
	}
}

func TestRunExportRejectsUnknownPlanFields(t *testing.T) {
	root := t.TempDir()
	plan := base64.StdEncoding.EncodeToString([]byte(`{"schema_version":"cpgen.transfer-export-plan/v1","files":[],"max_files":1,"max_total_bytes":1,"future":true}`))
	var stdin, stdout, stderr bytes.Buffer
	code := run([]string{"export", "--root", root, "--plan-base64", plan}, &stdin, &stdout, &stderr)
	if code != 2 || stdout.Len() != 0 {
		t.Fatalf("exit code = %d, stdout bytes = %d, stderr = %q", code, stdout.Len(), stderr.String())
	}
}
