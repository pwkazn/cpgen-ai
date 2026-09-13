package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	dockersandbox "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/toolchain"
)

type dockerRunner func(context.Context, ...string) (string, error)

type dependencies struct {
	projectRoot   string
	docker        dockerRunner
	buildTransfer func(context.Context, string, string) error
	goos          string
}

func main() {
	projectRoot, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "find project root: %v\n", err)
		os.Exit(1)
	}
	os.Exit(runWithDependencies(os.Args[1:], os.Stdout, os.Stderr, dependencies{
		projectRoot:   projectRoot,
		docker:        runDocker,
		buildTransfer: buildTransferBinary,
		goos:          runtime.GOOS,
	}))
}

func runWithDependencies(args []string, stdout, stderr io.Writer, deps dependencies) int {
	flags := flag.NewFlagSet("cpgen-image-lock", flag.ContinueOnError)
	flags.SetOutput(stderr)
	output := flags.String("output", "", "toolchain lock output path")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *output == "" {
		fmt.Fprintln(stderr, "usage: cpgen-image-lock --output PATH")
		return 2
	}
	if deps.projectRoot == "" || deps.docker == nil || deps.buildTransfer == nil {
		fmt.Fprintln(stderr, "image-lock dependencies are not configured")
		return 1
	}
	endpoint, err := localDockerEndpoint(deps.goos)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	outputPath := *output
	if !filepath.IsAbs(outputPath) {
		outputPath = filepath.Join(deps.projectRoot, outputPath)
	}
	outputPath = filepath.Clean(outputPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := ensurePinnedBases(ctx, deps.docker, endpoint); err != nil {
		fmt.Fprintf(stderr, "verify pinned base images: %v\n", err)
		return 1
	}
	ids, err := buildImages(ctx, deps, endpoint)
	if err != nil {
		fmt.Fprintf(stderr, "build sandbox images: %v\n", err)
		return 1
	}
	lock, err := toolchain.NewDockerV1Lock(ids[0], ids[1], ids[2])
	if err != nil {
		fmt.Fprintf(stderr, "construct toolchain lock: %v\n", err)
		return 1
	}
	encoded, err := lock.MarshalIndent()
	if err != nil {
		fmt.Fprintf(stderr, "encode toolchain lock: %v\n", err)
		return 1
	}
	if err := writeAtomic(outputPath, encoded); err != nil {
		fmt.Fprintf(stderr, "write toolchain lock: %v\n", err)
		return 1
	}
	digest, _ := lock.Digest()
	fmt.Fprintf(stdout, "wrote %s (%s)\n", outputPath, digest)
	return 0
}

func localDockerEndpoint(goos string) (string, error) {
	var raw string
	switch goos {
	case "windows":
		raw = "npipe:////./pipe/docker_engine"
	case "linux":
		raw = "unix:///var/run/docker.sock"
	default:
		return "", fmt.Errorf("unsupported image-lock operating system %q", goos)
	}
	endpoint, err := dockersandbox.ParseLocalEndpoint(raw, goos)
	if err != nil {
		return "", err
	}
	return endpoint.String(), nil
}

func ensurePinnedBases(ctx context.Context, runner dockerRunner, endpoint string) error {
	for _, reference := range []string{toolchain.GoBaseRef, toolchain.DebianBaseRef} {
		if _, err := runner(ctx, "--host", endpoint, "image", "inspect", "--format", "{{.Id}}", reference); err != nil {
			return fmt.Errorf("base %s is not present locally: %w", reference, err)
		}
	}
	return nil
}

func buildImages(ctx context.Context, deps dependencies, endpoint string) ([3]domain.Digest, error) {
	var ids [3]domain.Digest
	temporaryDirectory, err := os.MkdirTemp("", "cpgen-image-lock-*")
	if err != nil {
		return ids, err
	}
	defer os.RemoveAll(temporaryDirectory)
	transferContext := filepath.Join(temporaryDirectory, "transfer")
	if err := os.Mkdir(transferContext, 0o700); err != nil {
		return ids, err
	}
	if err := deps.buildTransfer(ctx, deps.projectRoot, filepath.Join(transferContext, "cpgen-transfer")); err != nil {
		return ids, fmt.Errorf("build trusted transfer helper: %w", err)
	}
	roles := []string{"builder", "runtime", "transfer"}
	for index, role := range roles {
		dockerfile := filepath.Join(deps.projectRoot, "build", "docker", role, "Dockerfile")
		if info, err := os.Stat(dockerfile); err != nil || !info.Mode().IsRegular() {
			if err == nil {
				err = fmt.Errorf("not a regular file")
			}
			return ids, fmt.Errorf("Dockerfile %s: %w", dockerfile, err)
		}
		iidPath := filepath.Join(temporaryDirectory, role+".iid")
		buildContext := deps.projectRoot
		if role == "transfer" {
			buildContext = transferContext
		}
		if _, err := deps.docker(ctx,
			"--host", endpoint,
			"build", "--pull=false", "--network=none", "--provenance=false",
			"--file", dockerfile, "--iidfile", iidPath, buildContext,
		); err != nil {
			return ids, fmt.Errorf("%s image: %w", role, err)
		}
		rawID, err := os.ReadFile(iidPath)
		if err != nil {
			return ids, fmt.Errorf("read %s image ID: %w", role, err)
		}
		imageID, err := domain.ParseDigest(strings.TrimSpace(string(rawID)))
		if err != nil {
			return ids, fmt.Errorf("validate %s image ID: %w", role, err)
		}
		if err := inspectLabels(ctx, deps.docker, endpoint, role, imageID); err != nil {
			return ids, err
		}
		ids[index] = imageID
	}
	return ids, nil
}

func inspectLabels(ctx context.Context, runner dockerRunner, endpoint, role string, imageID domain.Digest) error {
	raw, err := runner(ctx, "--host", endpoint, "image", "inspect", "--format", "{{json .Config.Labels}}", string(imageID))
	if err != nil {
		return fmt.Errorf("inspect %s image labels: %w", role, err)
	}
	var labels map[string]string
	if err := json.Unmarshal([]byte(strings.TrimSpace(raw)), &labels); err != nil {
		return fmt.Errorf("decode %s image labels: %w", role, err)
	}
	for key, value := range toolchain.RequiredImageLabels(role) {
		if labels[key] != value {
			return fmt.Errorf("%s image label %q is %q, want %q", role, key, labels[key], value)
		}
	}
	return nil
}

func runDocker(ctx context.Context, arguments ...string) (string, error) {
	command := exec.CommandContext(ctx, "docker", arguments...)
	command.Env = dockerCommandEnvironment(os.Environ())
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func dockerCommandEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_API_VERSION", "DOCKER_TLS_VERIFY", "DOCKER_CERT_PATH":
			continue
		default:
			filtered = append(filtered, entry)
		}
	}
	return filtered
}
