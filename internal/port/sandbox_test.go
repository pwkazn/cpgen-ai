package port_test

import (
	"encoding/json"
	"slices"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestPortEnumsRejectUnknownJSONValues(t *testing.T) {
	t.Parallel()
	for _, target := range []any{new(port.Language), new(port.ProgramRole), new(port.ContainerRole)} {
		if err := json.Unmarshal([]byte(`"UNKNOWN"`), target); err == nil {
			t.Fatalf("%T accepted an unknown enum value", target)
		}
	}
}

func TestSourceBundleDigestIsCanonicalAndCoversEntryAndPaths(t *testing.T) {
	files := []port.SourceFile{
		{Path: "main.cpp", Blob: domain.BlobRef{Digest: domain.SumBytes([]byte("main")), Size: 4}},
		{Path: "lib/helper.h", Blob: domain.BlobRef{Digest: domain.SumBytes([]byte("helper")), Size: 6}},
	}
	manifest := port.SourceBundleManifest{SchemaVersion: "cpgen.source-bundle/v1", EntryPoint: "main.cpp", Files: files}
	digest, err := port.ComputeSourceBundleDigest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	reordered := manifest
	reordered.Files = slices.Clone(files)
	slices.Reverse(reordered.Files)
	got, err := port.ComputeSourceBundleDigest(reordered)
	if err != nil || got != digest {
		t.Fatalf("reordered digest = %q err=%v, want %q", got, err, digest)
	}
	changed := manifest
	changed.EntryPoint = "lib/helper.h"
	if other, err := port.ComputeSourceBundleDigest(changed); err != nil || other == digest {
		t.Fatalf("entry change digest = %q err=%v", other, err)
	}
	manifest.Digest = digest
	if err := manifest.Validate(); err != nil {
		t.Fatal(err)
	}
	manifest.Digest = domain.SumBytes([]byte("forged"))
	if err := manifest.Validate(); err == nil {
		t.Fatal("forged bundle digest was accepted")
	}
}
