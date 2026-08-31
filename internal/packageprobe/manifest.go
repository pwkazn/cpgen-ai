package packageprobe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sort"

	"cpgen/internal/domain"
)

func NewManifest(problem Problem, entries []FileEntry) (Manifest, error) {
	if err := problem.Validate(1 << 20); err != nil {
		return Manifest{}, err
	}
	files := slices.Clone(entries)
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	manifest := Manifest{
		SchemaVersion: PackageSchemaVersion, RunID: problem.RunID, Problem: problem.Problem,
		Limits: problem.Limits, Checker: problem.Checker, ToolchainManifestDigest: problem.ToolchainManifestDigest,
		TestGroups: cloneGroups(problem.TestGroups), Files: files, Verification: problem.Verification, ProvenancePath: problem.ProvenancePath,
	}
	packageID, err := ComputePackageID(manifest)
	if err != nil {
		return Manifest{}, err
	}
	manifest.PackageID = packageID
	if err := manifest.Validate(1 << 20); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func ComputePackageID(manifest Manifest) (domain.Digest, error) {
	canonical := manifest
	canonical.PackageID = ""
	canonical.Files = slices.Clone(manifest.Files)
	sort.Slice(canonical.Files, func(left, right int) bool { return canonical.Files[left].Path < canonical.Files[right].Path })
	canonical.TestGroups = cloneGroups(manifest.TestGroups)
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode canonical package manifest: %w", err)
	}
	return domain.SumBytes(encoded), nil
}

func EncodeManifest(manifest Manifest) ([]byte, error) {
	if err := manifest.Validate(1 << 20); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(encoded, '\n'), nil
}

func DecodeManifest(data []byte) (Manifest, error) {
	var manifest Manifest
	if err := decodeStrict(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode package manifest: %w", err)
	}
	if err := manifest.Validate(1 << 20); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func decodeStrict(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return fmt.Errorf("JSON contains a trailing value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
