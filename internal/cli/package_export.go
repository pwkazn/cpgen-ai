package cli

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"

	"cpgen/internal/application"
	"cpgen/internal/domain"
	"cpgen/internal/packageprobe"
)

func runPackageExport(args []string, app *application.Application, stdout, stderr io.Writer) int {
	if len(args) < 1 {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: run export RUN_ID --output PATH"))
	}
	id, err := parseRunID(args[0])
	if err != nil {
		return writeStateError(stdout, stderr, 3, "invalid_id", err)
	}
	flags := flag.NewFlagSet("run export", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "", "destination ZIP (must not exist)")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || *output == "" {
		return writeStateError(stdout, stderr, 2, "usage", errors.New("usage: run export RUN_ID --output PATH"))
	}
	destination, err := filepath.Abs(*output)
	if err != nil {
		return writeStateError(stdout, stderr, 2, "output_path", err)
	}
	reader, ok := app.Runs.(interface {
		ReadPackageArchive(context.Context, domain.RunID) ([]byte, domain.VerifiedPackageRecord, error)
	})
	if !ok {
		return writeStateError(stdout, stderr, 5, "package_unavailable", errors.New("workflow has no verified package export"))
	}
	raw, record, err := reader.ReadPackageArchive(context.Background(), id)
	if err != nil {
		return writeStateError(stdout, stderr, exitForError(err), codeForError(err), err)
	}
	verified, err := packageprobe.ReadArchive(context.Background(), raw)
	manifestRaw, manifestErr := packageprobe.EncodeManifest(verified.Manifest)
	if err != nil || manifestErr != nil || record.Validate() != nil || record.RunID != id || verified.Manifest.RunID != id || verified.Manifest.PackageID != record.Binding.PackageID || domain.SumBytes(manifestRaw) != record.Binding.ManifestDigest || record.Binding.Archive.Digest != domain.SumBytes(raw) || record.Binding.Archive.Size != int64(len(raw)) {
		return writeStateError(stdout, stderr, 9, "package_invalid", errors.New("export archive differs from its verified record"))
	}
	if err := publishPackageExport(destination, raw); err != nil {
		return writeStateError(stdout, stderr, 9, "export_failed", err)
	}
	return encodeEnvelope(stdout, stderr, envelope{SchemaVersion: cliSchema, Status: "EXPORTED", Data: struct {
		Path    string                       `json:"path"`
		Package domain.VerifiedPackageRecord `json:"package"`
	}{destination, record}}, 0)
}

// Publish a fully written and synced temporary file through an exclusive hard
// link. A crash cannot expose a partial destination, and existing destinations
// (including symlinks) are never replaced. All operations stay within one root.
func publishPackageExport(destination string, raw []byte) error {
	root, err := os.OpenRoot(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer root.Close()
	id, err := domain.NewID("export")
	if err != nil {
		return err
	}
	temporary := "." + id + ".tmp"
	f, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer root.Remove(temporary)
	_, writeErr := f.Write(raw)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return root.Link(temporary, filepath.Base(destination))
}
