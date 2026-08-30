package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"time"

	dockersandbox "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
)

var Version = "dev"

type versionOutput struct {
	SchemaVersion string `json:"schema_version"`
	Version       string `json:"version"`
	GoVersion     string `json:"go_version"`
}

type Dependencies struct {
	GOOS        string
	CheckDocker func(context.Context, dockersandbox.Config) (dockersandbox.StaticReport, error)
}

type doctorOutput struct {
	SchemaVersion        string                      `json:"schema_version"`
	Status               string                      `json:"status"`
	EngineIdentityDigest domain.Digest               `json:"engine_identity_digest,omitempty"`
	Checks               []string                    `json:"checks"`
	FailureCode          domain.PortFailureCode      `json:"failure_code,omitempty"`
	FailureClass         domain.FailureClass         `json:"failure_class,omitempty"`
	Message              string                      `json:"message,omitempty"`
	Report               *dockersandbox.StaticReport `json:"report,omitempty"`
}

func Run(args []string, stdout, stderr io.Writer) int {
	return RunWithDependencies(args, stdout, stderr, Dependencies{
		GOOS:        runtime.GOOS,
		CheckDocker: dockersandbox.CheckStatic,
	})
}

func RunWithDependencies(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	if len(args) == 0 {
		writeHelp(stdout)
		return 0
	}
	switch args[0] {
	case "help", "-h", "--help":
		writeHelp(stdout)
		return 0
	case "version":
		if len(args) == 2 && args[1] == "--json" {
			encoder := json.NewEncoder(stdout)
			encoder.SetEscapeHTML(false)
			if err := encoder.Encode(versionOutput{SchemaVersion: "cpgen.cli-version/v1", Version: Version, GoVersion: runtime.Version()}); err != nil {
				fmt.Fprintf(stderr, "write version: %v\n", err)
				return 1
			}
			return 0
		}
		if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: cpgen version [--json]")
			return 2
		}
		fmt.Fprintln(stdout, Version)
		return 0
	case "doctor":
		return runDoctor(args[1:], stdout, stderr, dependencies)
	default:
		fmt.Fprintf(stderr, "unknown command %q\n", args[0])
		fmt.Fprintln(stderr, "run 'cpgen help' for usage")
		return 2
	}
}

func runDoctor(args []string, stdout, stderr io.Writer, dependencies Dependencies) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	jsonOutput := flags.Bool("json", false, "emit versioned JSON")
	engineEndpoint := flags.String("engine-endpoint", "", "explicit local Docker Engine endpoint")
	apiVersion := flags.String("api-version", "", "pinned Docker Engine API version")
	builderImage := flags.String("builder-image", "", "immutable builder image ID")
	runtimeImage := flags.String("runtime-image", "", "immutable runtime image ID")
	transferImage := flags.String("transfer-image", "", "immutable transfer image ID")
	executionProtocol := flags.String("execution-protocol", "", "sandbox execution protocol")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || !*jsonOutput {
		fmt.Fprintln(stderr, "usage: cpgen doctor --json --engine-endpoint ENDPOINT --api-version VERSION --builder-image SHA256 --runtime-image SHA256 --transfer-image SHA256 --execution-protocol docker-direct-v2")
		return 2
	}
	config := dockersandbox.Config{
		EngineEndpoint:    *engineEndpoint,
		APIVersion:        *apiVersion,
		BuilderImage:      *builderImage,
		RuntimeImage:      *runtimeImage,
		TransferImage:     *transferImage,
		ExecutionProtocol: *executionProtocol,
	}
	goos := dependencies.GOOS
	if goos == "" {
		goos = runtime.GOOS
	}
	if _, err := config.Validate(goos); err != nil {
		fmt.Fprintf(stderr, "invalid doctor configuration: %v\n", err)
		return 2
	}
	if dependencies.CheckDocker == nil {
		fmt.Fprintln(stderr, "doctor dependency is not configured")
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	report, err := dependencies.CheckDocker(ctx, config)
	if err == nil {
		return encodeJSON(stdout, stderr, doctorOutput{
			SchemaVersion:        "cpgen.doctor/v1",
			Status:               "HEALTHY",
			EngineIdentityDigest: report.EngineIdentityDigest,
			Checks:               staticDoctorChecks(),
			Report:               &report,
		}, 0)
	}

	var checkErr *dockersandbox.CheckError
	if errors.As(err, &checkErr) {
		if validationErr := checkErr.Failure.Validate(); validationErr != nil {
			fmt.Fprintf(stderr, "invalid doctor failure: %v\n", validationErr)
			return 1
		}
		return encodeJSON(stdout, stderr, doctorOutput{
			SchemaVersion: "cpgen.doctor/v1",
			Status:        string(checkErr.Failure.Class),
			Checks:        staticDoctorChecks(),
			FailureCode:   checkErr.Failure.Code,
			FailureClass:  checkErr.Failure.Class,
			Message:       checkErr.Error(),
		}, 10)
	}
	fmt.Fprintf(stderr, "doctor failed: %v\n", err)
	return 1
}

func staticDoctorChecks() []string {
	return []string{"engine_ping", "server_version", "server_info", "builder_image", "runtime_image", "transfer_image"}
}

func encodeJSON(stdout, stderr io.Writer, value any, successCode int) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		fmt.Fprintf(stderr, "write JSON: %v\n", err)
		return 1
	}
	return successCode
}

func writeHelp(writer io.Writer) {
	fmt.Fprintln(writer, `cpgen - competitive programming problem generator

Usage:
  cpgen help
  cpgen version [--json]
	cpgen doctor --json --engine-endpoint ENDPOINT --api-version VERSION --builder-image SHA256 --runtime-image SHA256 --transfer-image SHA256 --execution-protocol docker-direct-v2

The Slice 0 Docker probe is test-only and is not exposed by this CLI.`)
}
