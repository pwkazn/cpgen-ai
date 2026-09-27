package application_test

import (
	"context"
	"cpgen/internal/application"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkbenchReconstructsStatementWithoutProviderRequests(t *testing.T) {
	f := newGenerationReaderFixtureWithPolicy(t, true)
	r, err := application.NewWorkbenchReader(f.store, f.blobs)
	if err != nil {
		t.Fatal(err)
	}
	d, err := r.ReadRunDetail(context.Background(), f.runID)
	if err != nil {
		t.Fatal(err)
	}
	reads := f.httpCalls.Load()
	found := false
	for _, a := range d.Artifacts {
		if a.StageName != "statement" {
			continue
		}
		found = true
		content, err := r.ReadArtifact(context.Background(), f.runID, a.OccurrenceID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(content.Content), f.problem.Title) {
			t.Fatalf("missing statement title: %s", content.Content)
		}
	}
	if !found {
		t.Fatal("no statement projection")
	}
	if f.httpCalls.Load() != reads {
		t.Fatal("read dispatched external provider")
	}
	stage, err := f.store.ReadCommittedLLMStage(context.Background(), f.runID, "idea")
	if err != nil {
		t.Fatal(err)
	}
	hex := strings.TrimPrefix(string(stage.Artifacts[0].Blob.Blob.Digest), "sha256:")
	if err := os.WriteFile(filepath.Join(f.blobRoot, "blobs", "sha256", hex[:2], hex), []byte("corrupted receipt"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, a := range d.Artifacts {
		if a.StageName == "statement" {
			if _, err := r.ReadArtifact(context.Background(), f.runID, a.OccurrenceID, 0); err == nil {
				t.Fatal("corrupt receipt accepted")
			}
		}
	}
	if f.httpCalls.Load() != reads {
		t.Fatal("corrupt receipt triggered regeneration")
	}
}
