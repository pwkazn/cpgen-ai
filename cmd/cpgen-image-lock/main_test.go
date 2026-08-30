package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/toolchain"
)

func TestRunBuildsAllImagesBeforeAtomicallyWritingTheLock(t *testing.T) {
	projectRoot := makeProjectRoot(t)
	output := filepath.Join(t.TempDir(), "docker-v1.lock.json")
	fake := newFakeDocker()
	var stdout, stderr bytes.Buffer
	code := runWithDependencies([]string{"--output", output}, &stdout, &stderr, dependencies{
		projectRoot: projectRoot,
		docker:      fake.run,
		goos:        "windows",
	})
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	file, err := os.Open(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lock, err := toolchain.LoadLock(file)
	if err != nil {
		t.Fatal(err)
	}
	if lock.Builder.ImageID != fake.ids[0] || lock.Runtime.ImageID != fake.ids[1] || lock.Transfer.ImageID != fake.ids[2] {
		t.Fatalf("unexpected generated lock: %#v", lock)
	}
	if fake.buildCount != 3 || fake.inspectLabelCount != 3 {
		t.Fatalf("builds=%d label inspections=%d", fake.buildCount, fake.inspectLabelCount)
	}
}

func TestRunLeavesExistingLockUntouchedWhenABuildFails(t *testing.T) {
	projectRoot := makeProjectRoot(t)
	directory := t.TempDir()
	output := filepath.Join(directory, "docker-v1.lock.json")
	if err := os.WriteFile(output, []byte("existing-lock"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := newFakeDocker()
	fake.failBuild = 3
	var stdout, stderr bytes.Buffer
	code := runWithDependencies([]string{"--output", output}, &stdout, &stderr, dependencies{
		projectRoot: projectRoot,
		docker:      fake.run,
		goos:        "windows",
	})
	if code != 1 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	got, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "existing-lock" {
		t.Fatalf("existing lock changed to %q", got)
	}
	temps, err := filepath.Glob(filepath.Join(directory, ".docker-v1.lock.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 0 {
		t.Fatalf("temporary locks leaked: %#v", temps)
	}
}

type fakeDocker struct {
	ids               [3]domain.Digest
	buildCount        int
	inspectLabelCount int
	failBuild         int
	roles             map[string]string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{
		ids: [3]domain.Digest{
			"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		},
		roles: map[string]string{},
	}
}

func (f *fakeDocker) run(_ context.Context, arguments ...string) (string, error) {
	if len(arguments) < 3 || arguments[0] != "--host" || arguments[1] != "npipe:////./pipe/docker_engine" {
		return "", errors.New("Docker command did not use the explicit local endpoint")
	}
	arguments = arguments[2:]
	if len(arguments) >= 2 && arguments[0] == "image" && arguments[1] == "inspect" {
		if strings.Contains(strings.Join(arguments, " "), ".Config.Labels") {
			f.inspectLabelCount++
			imageID := arguments[len(arguments)-1]
			labels := toolchain.RequiredImageLabels(f.roles[imageID])
			encoded, _ := json.Marshal(labels)
			return string(encoded), nil
		}
		return "present", nil
	}
	if len(arguments) > 0 && arguments[0] == "build" {
		f.buildCount++
		if f.buildCount == f.failBuild {
			return "", errors.New("simulated build failure")
		}
		iidPath := argumentAfter(arguments, "--iidfile")
		role := []string{"builder", "runtime", "transfer"}[f.buildCount-1]
		imageID := f.ids[f.buildCount-1]
		f.roles[string(imageID)] = role
		if err := os.WriteFile(iidPath, []byte(string(imageID)+"\n"), 0o600); err != nil {
			return "", err
		}
		return "built", nil
	}
	return "", errors.New("unexpected Docker command")
}

func argumentAfter(arguments []string, name string) string {
	for index := 0; index+1 < len(arguments); index++ {
		if arguments[index] == name {
			return arguments[index+1]
		}
	}
	return ""
}

func makeProjectRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, role := range []string{"builder", "runtime", "transfer"} {
		directory := filepath.Join(root, "build", "docker", role)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, "Dockerfile"), []byte("fixture"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}
