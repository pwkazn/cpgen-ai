package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Compile the trusted helper with the project's installed Go toolchain and
// cached modules. The untrusted-program builder remains at its locked compiler
// version; neither this build nor the Docker build may download dependencies.
func buildTransferBinary(ctx context.Context, projectRoot, output string) error {
	command := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-mod=readonly", "-trimpath", "-ldflags=-s -w -buildid=", "-o", output, "./cmd/cpgen-transfer")
	command.Dir = projectRoot
	command.Env = transferBuildEnvironment(os.Environ())
	raw, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("offline Linux/amd64 Go build: %w: %s", err, strings.TrimSpace(string(raw)))
	}
	info, err := os.Lstat(output)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() == 0 {
		return fmt.Errorf("transfer compiler did not produce a regular executable")
	}
	return nil
}

func transferBuildEnvironment(environment []string) []string {
	filtered := make([]string, 0, len(environment)+12)
	for _, entry := range environment {
		name, _, _ := strings.Cut(entry, "=")
		switch strings.ToUpper(name) {
		case "GOOS", "GOARCH", "GOAMD64", "CGO_ENABLED", "GOTOOLCHAIN", "GOPROXY", "GOSUMDB", "GOWORK", "GOFLAGS", "GOEXPERIMENT", "GOENV":
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, "GOOS=linux", "GOARCH=amd64", "GOAMD64=v1", "CGO_ENABLED=0", "GOTOOLCHAIN=local", "GOPROXY=off", "GOSUMDB=off", "GOWORK=off", "GOFLAGS=", "GOEXPERIMENT=", "GOENV=off")
}
