package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	dockersandbox "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/cli"
	"cpgen/internal/domain"
)

func TestVersionJSON(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{"version", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	var output map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatalf("decode version JSON: %v", err)
	}
	if output["schema_version"] != "cpgen.cli-version/v1" || output["version"] == "" || output["go_version"] == "" {
		t.Fatalf("unexpected output: %#v", output)
	}
}

func TestDoctorJSONUsesExplicitFlagsAndReportsHealthy(t *testing.T) {
	t.Parallel()
	digest := domain.SumBytes([]byte("image"))
	var gotConfig dockersandbox.Config
	dependencies := cli.Dependencies{
		GOOS: "windows",
		CheckDocker: func(_ context.Context, config dockersandbox.Config) (dockersandbox.StaticReport, error) {
			gotConfig = config
			return dockersandbox.StaticReport{EngineIdentityDigest: domain.SumBytes([]byte("engine"))}, nil
		},
	}
	args := []string{
		"doctor", "--json",
		"--engine-endpoint", "npipe:////./pipe/docker_engine",
		"--api-version", "1.55",
		"--builder-image", string(digest),
		"--runtime-image", string(digest),
		"--transfer-image", string(digest),
		"--execution-protocol", "docker-direct-v2",
	}
	var stdout, stderr bytes.Buffer
	code := cli.RunWithDependencies(args, &stdout, &stderr, dependencies)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	var output struct {
		SchemaVersion        string        `json:"schema_version"`
		Status               string        `json:"status"`
		EngineIdentityDigest domain.Digest `json:"engine_identity_digest"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.SchemaVersion != "cpgen.doctor/v1" || output.Status != "HEALTHY" || output.EngineIdentityDigest == "" {
		t.Fatalf("unexpected doctor output: %#v", output)
	}
	if gotConfig.EngineEndpoint != "npipe:////./pipe/docker_engine" || gotConfig.ExecutionProtocol != "docker-direct-v2" {
		t.Fatalf("unexpected doctor config: %#v", gotConfig)
	}
}

func TestDoctorRejectsMissingFlagsBeforeDispatch(t *testing.T) {
	t.Parallel()
	called := false
	var stdout, stderr bytes.Buffer
	code := cli.RunWithDependencies([]string{"doctor", "--json"}, &stdout, &stderr, cli.Dependencies{
		GOOS: "windows",
		CheckDocker: func(context.Context, dockersandbox.Config) (dockersandbox.StaticReport, error) {
			called = true
			return dockersandbox.StaticReport{}, nil
		},
	})
	if code != 2 || called {
		t.Fatalf("exit code = %d, called = %v, stderr = %q", code, called, stderr.String())
	}
}

func TestDoctorReturnsBlockedJSONForUnavailableEngine(t *testing.T) {
	t.Parallel()
	digest := domain.SumBytes([]byte("image"))
	args := []string{
		"doctor", "--json",
		"--engine-endpoint", "npipe:////./pipe/docker_engine",
		"--api-version", "1.55",
		"--builder-image", string(digest),
		"--runtime-image", string(digest),
		"--transfer-image", string(digest),
		"--execution-protocol", "docker-direct-v2",
	}
	var stdout, stderr bytes.Buffer
	code := cli.RunWithDependencies(args, &stdout, &stderr, cli.Dependencies{
		GOOS: "windows",
		CheckDocker: func(context.Context, dockersandbox.Config) (dockersandbox.StaticReport, error) {
			return dockersandbox.StaticReport{}, &dockersandbox.CheckError{
				Failure: domain.PortFailure{Code: domain.FailureUnavailable, Class: domain.FailureBlocked},
				Cause:   errors.New("dial refused"),
			}
		},
	})
	if code != 10 {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
	var output map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output["status"] != "BLOCKED" || output["failure_code"] != "unavailable" {
		t.Fatalf("unexpected blocked output: %#v", output)
	}
}

func TestUnknownCommandFailsWithoutExposingProbe(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	code := cli.Run([]string{"probe"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "unknown command") {
		t.Fatalf("exit code = %d, stderr = %q", code, stderr.String())
	}
}

func TestWatchdogHiddenCommandRequiresOnlyAbsoluteOwnerControlPath(t *testing.T) {
	var stdout, stderr bytes.Buffer
	called := ""
	control := filepath.Join(t.TempDir(), "control.json")
	code := cli.RunWithDependencies([]string{"sandbox-watchdog", "--control", control}, &stdout, &stderr, cli.Dependencies{
		RunWatchdog: func(_ context.Context, path string) error {
			called = path
			return nil
		},
	})
	if code != 0 || called != control || stdout.Len() != 0 {
		t.Fatalf("code=%d called=%q stdout=%q stderr=%q", code, called, stdout.String(), stderr.String())
	}
	for _, args := range [][]string{
		{"sandbox-watchdog", "--control", "relative.json"},
		{"sandbox-watchdog", "--control", control, "extra"},
		{"sandbox-watchdog", "--control", control, "--engine-endpoint", "tcp://attacker"},
	} {
		called = ""
		stdout.Reset()
		stderr.Reset()
		if code := cli.RunWithDependencies(args, &stdout, &stderr, cli.Dependencies{RunWatchdog: func(context.Context, string) error {
			called = "called"
			return nil
		}}); code != 2 || called != "" {
			t.Fatalf("args=%#v code=%d called=%q", args, code, called)
		}
	}
	stdout.Reset()
	stderr.Reset()
	cli.Run([]string{"help"}, &stdout, &stderr)
	if strings.Contains(stdout.String(), "sandbox-watchdog") {
		t.Fatal("hidden watchdog command appeared in help")
	}
}
