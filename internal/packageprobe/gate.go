package packageprobe

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const StructuralProbeProducer = "package-structural-probe/v1"

type ImportSink interface {
	port.MeteredArtifactSink
	port.VerifiedBlobReader
}

func StructuralGate(ctx context.Context, root *os.Root, limits ReadLimits) (VerifiedProblem, error) {
	return Inspect(ctx, root, limits)
}

func Import(ctx context.Context, root *os.Root, limits ReadLimits, sink ImportSink) (VerifiedProblem, []domain.PendingArtifact, error) {
	if sink == nil {
		return VerifiedProblem{}, nil, fmt.Errorf("package import sink is required")
	}
	opened, err := openPackage(ctx, root, limits)
	if err != nil {
		return VerifiedProblem{}, nil, err
	}
	defer opened.close()
	if _, err := inspectOpened(ctx, opened); err != nil {
		return VerifiedProblem{}, nil, err
	}

	type importItem struct {
		file        openedFile
		entry       FileEntry
		declaration port.ArtifactDeclaration
		writer      port.ArtifactWriter
		finalized   bool
	}
	manifestRef := domain.BlobRef{Digest: domain.SumBytes(opened.manifestBytes), Size: int64(len(opened.manifestBytes))}
	manifestEntry := FileEntry{Path: "manifest.json", SHA256: manifestRef.Digest, Size: manifestRef.Size, Role: RoleManifest}
	items := make([]importItem, 0, len(opened.files)+1)
	items = append(items, importItem{file: opened.manifestFile, entry: manifestEntry, declaration: artifactDeclaration(manifestEntry)})
	for index, file := range opened.files {
		entry := opened.manifest.Files[index]
		items = append(items, importItem{file: file, entry: entry, declaration: artifactDeclaration(entry)})
	}
	for index := range items {
		writer, prepareErr := sink.Prepare(ctx, items[index].declaration)
		if prepareErr != nil {
			for previous := range index {
				_ = items[previous].writer.Abort(context.Background())
			}
			return VerifiedProblem{}, nil, prepareErr
		}
		items[index].writer = writer
	}
	abortPending := func() {
		for index := range items {
			if !items[index].finalized && items[index].writer != nil {
				_ = items[index].writer.Abort(context.Background())
			}
		}
	}

	artifacts := make([]domain.PendingArtifact, 0, len(items))
	for index := range items {
		item := &items[index]
		if _, err := item.file.file.Seek(0, io.SeekStart); err != nil {
			abortPending()
			return VerifiedProblem{}, nil, err
		}
		hash := sha256.New()
		limited := io.LimitReader(item.file.file, item.entry.Size+1)
		written, copyErr := io.Copy(item.writer, io.TeeReader(limited, hash))
		if copyErr != nil || written != item.entry.Size || domain.Digest("sha256:"+fmt.Sprintf("%x", hash.Sum(nil))) != item.entry.SHA256 {
			abortPending()
			return VerifiedProblem{}, nil, errors.Join(copyErr, fmt.Errorf("package file %q changed while importing", item.entry.Path))
		}
		pending, finalizeErr := item.writer.Finalize(ctx)
		if finalizeErr != nil {
			abortPending()
			return VerifiedProblem{}, nil, finalizeErr
		}
		item.finalized = true
		if err := validateImportedArtifact(pending, item.entry, item.declaration); err != nil {
			abortPending()
			return VerifiedProblem{}, nil, err
		}
		artifacts = append(artifacts, pending)
	}
	verified, err := reverseReadImported(ctx, opened.manifest, artifacts, sink)
	if err != nil {
		return VerifiedProblem{}, nil, err
	}
	return verified, artifacts, nil
}

func artifactDeclaration(entry FileEntry) port.ArtifactDeclaration {
	role := domain.ArtifactSource
	switch entry.Role {
	case RoleManifest, RoleSimilarityReport, RolePrePackageReport, RoleProvenance:
		role = domain.ArtifactEvidence
	case RoleTestInput, RoleTestAnswer:
		role = domain.ArtifactInput
	}
	mediaType := mime.TypeByExtension(filepath.Ext(string(entry.Path)))
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	digest := entry.SHA256
	return port.ArtifactDeclaration{
		MediaType: mediaType, Role: role, LogicalPath: entry.Path, MaxBytes: maxInt64(entry.Size, 1),
		Provenance: domain.ProvenanceCandidate{SchemaVersion: domain.DomainSchemaVersion, Producer: StructuralProbeProducer, InputDigest: &digest},
	}
}

func validateImportedArtifact(pending domain.PendingArtifact, entry FileEntry, declaration port.ArtifactDeclaration) error {
	if err := pending.Validate(); err != nil {
		return err
	}
	if pending.Blob.Digest != entry.SHA256 || pending.Blob.Size != entry.Size || pending.MediaType != declaration.MediaType ||
		pending.Role != declaration.Role || pending.LogicalPath != entry.Path || pending.Provenance != declaration.Provenance {
		return fmt.Errorf("imported artifact %q does not match its fixed declaration", entry.Path)
	}
	return nil
}

func reverseReadImported(ctx context.Context, manifest Manifest, artifacts []domain.PendingArtifact, sink port.VerifiedBlobReader) (VerifiedProblem, error) {
	byPath := make(map[domain.SafeRelPath]domain.PendingArtifact, len(artifacts))
	for _, artifact := range artifacts {
		byPath[artifact.LogicalPath] = artifact
	}
	manifestArtifact, ok := byPath["manifest.json"]
	if !ok {
		return VerifiedProblem{}, fmt.Errorf("imported manifest artifact is missing")
	}
	manifestBytes, err := readVerifiedBlob(ctx, sink, manifestArtifact.Blob)
	if err != nil {
		return VerifiedProblem{}, err
	}
	importedManifest, err := DecodeManifest(manifestBytes)
	if err != nil {
		return VerifiedProblem{}, err
	}
	if importedManifest.PackageID != manifest.PackageID {
		return VerifiedProblem{}, fmt.Errorf("imported manifest identity changed")
	}
	verified := VerifiedProblem{Manifest: importedManifest, Files: make([]VerifiedFile, len(importedManifest.Files))}
	for index, entry := range importedManifest.Files {
		artifact, exists := byPath[entry.Path]
		if !exists {
			return VerifiedProblem{}, fmt.Errorf("imported Blob for %q is missing", entry.Path)
		}
		data, err := readVerifiedBlob(ctx, sink, artifact.Blob)
		if err != nil {
			return VerifiedProblem{}, err
		}
		if int64(len(data)) != entry.Size || domain.SumBytes(data) != entry.SHA256 {
			return VerifiedProblem{}, fmt.Errorf("imported Blob for %q failed reverse verification", entry.Path)
		}
		verified.Files[index] = VerifiedFile{Entry: entry, Bytes: data}
	}
	if err := verified.Validate(); err != nil {
		return VerifiedProblem{}, err
	}
	return verified, nil
}

func readVerifiedBlob(ctx context.Context, source port.VerifiedBlobReader, ref domain.BlobRef) ([]byte, error) {
	reader, err := source.OpenVerified(ctx, ref)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, ref.Size+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(readErr, closeErr)
	}
	if reader.BlobRef() != ref || int64(len(data)) != ref.Size || domain.SumBytes(data) != ref.Digest {
		return nil, fmt.Errorf("verified Blob reader returned mismatched bytes")
	}
	return data, nil
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
