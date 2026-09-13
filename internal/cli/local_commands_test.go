package cli_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/cli"
	"cpgen/internal/config"
)

func TestLocalCommandsDoNotBootstrapExecution(t *testing.T) {
	root := t.TempDir()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "mvp.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.ReplaceAll(string(raw), "D:/cpgen-private/mvp", filepath.ToSlash(filepath.Join(root, "state"))))
	raw = []byte(strings.ReplaceAll(string(raw), "D:/cpgen-private/toolchains/docker-v1.lock.json", filepath.ToSlash(filepath.Join(root, "missing.lock.json"))))
	path := filepath.Join(root, "cpgen.yaml")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CPGEN_LLM_API_KEY", "")
	t.Setenv("CPGEN_SIMILARITY_API_KEY", "")
	executionCalls := 0
	deps := cli.Dependencies{Bootstrap: func(context.Context, config.Config) (*application.Application, error) {
		executionCalls++
		return nil, errors.New("execution dependency unavailable")
	}}
	id := "run_00000000000000000000000000000000"
	cases := []struct {
		args []string
		code int
	}{
		{[]string{"run", "list"}, 0},
		{[]string{"run", "show", id}, 3},
		{[]string{"run", "events", id}, 3},
		{[]string{"review", "show", id}, 3},
		{[]string{"run", "export", id, "--output", filepath.Join(root, "out.zip")}, 3},
	}
	for _, tc := range cases {
		t.Run(strings.Join(tc.args[:2], "_"), func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			code := cli.RunWithDependencies(append([]string{"--config", path}, tc.args...), &out, &diagnostic, deps)
			if code != tc.code {
				t.Fatalf("code=%d expected=%d: %s %s", code, tc.code, out.String(), diagnostic.String())
			}
		})
	}
	if executionCalls != 0 {
		t.Fatalf("read commands bootstrapped execution %d times", executionCalls)
	}
	for _, malformed := range [][]string{{"run", "unknown"}, {"run", "resume"}, {"run", "resume", "bad-id"}, {"review", "unknown"}} {
		var out, diagnostic bytes.Buffer
		code := cli.RunWithDependencies(append([]string{"--config", path}, malformed...), &out, &diagnostic, deps)
		if (code != 2 && code != 3) || executionCalls != 0 {
			t.Fatalf("malformed command bootstrapped execution: %v code=%d calls=%d", malformed, code, executionCalls)
		}
	}
	var out, diagnostic bytes.Buffer
	code := cli.RunWithDependencies([]string{"--config", path, "run", "resume", id}, &out, &diagnostic, deps)
	if code != 9 || executionCalls != 1 {
		t.Fatalf("resume bypassed execution bootstrap: code=%d calls=%d", code, executionCalls)
	}
}
