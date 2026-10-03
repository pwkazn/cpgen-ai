package packageprobe

import (
	"archive/zip"
	"bytes"
	"context"
	"os"
	"reflect"
	"testing"
)

func TestGenerationArchiveRoundTripAndTampering(t *testing.T) {
	ctx := context.Background()
	p := generationProblem(t, "cpp")
	raw, manifest, err := BuildArchive(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := ReadArchive(ctx, raw)
	if err != nil {
		t.Fatalf("archive read: %v", err)
	}
	if !reflect.DeepEqual(verified.Manifest, manifest) {
		t.Fatal("archive read changed the manifest")
	}
	if len(verified.Files) != len(p.Files) {
		t.Fatal("archive read changed the file count")
	}
	for _, want := range p.Files {
		found := false
		for _, got := range verified.Files {
			if got.Entry.Path != want.Path {
				continue
			}
			found = true
			if got.Entry.Role != want.Role || !bytes.Equal(got.Bytes, want.Data) {
				t.Fatalf("archive read changed file %s", want.Path)
			}
			break
		}
		if !found {
			t.Fatalf("archive read omitted file %s", want.Path)
		}
	}
	again, _, err := BuildArchive(ctx, p)
	if err != nil || !bytes.Equal(raw, again) {
		t.Fatal("archive is not deterministic")
	}
	for _, bad := range [][]byte{append(bytes.Clone(raw), 'x'), raw[:len(raw)-1]} {
		if _, err := ReadArchive(ctx, bad); err == nil {
			t.Fatal("accepted truncated or appended archive")
		}
	}
	for _, mode := range []string{"duplicate", "extra", "missing", "content", "traversal", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			r, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
			if err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			w := zip.NewWriter(&out)
			for i, file := range r.File {
				if mode == "missing" && i == 1 {
					continue
				}
				if mode == "content" && i == 1 {
					dst, _ := w.Create(file.Name)
					_, _ = dst.Write([]byte("changed"))
					continue
				}
				if err := w.Copy(file); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "duplicate" {
				if err := w.Copy(r.File[0]); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "extra" || mode == "traversal" || mode == "symlink" {
				name := "extra.txt"
				if mode == "traversal" {
					name = "../escape"
				}
				header := &zip.FileHeader{Name: name}
				header.SetMode(0o644)
				if mode == "symlink" {
					header.SetMode(0o777 | os.ModeSymlink)
				}
				dst, err := w.CreateHeader(header)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = dst.Write([]byte("payload"))
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadArchive(ctx, out.Bytes()); err == nil {
				t.Fatal("accepted malformed archive")
			}
		})
	}
}
