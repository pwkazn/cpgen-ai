package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"cpgen/internal/application"
	"cpgen/internal/config"
)

func TestMalformedCommandsFailBeforeConfigurationAndBootstrap(t *testing.T) {
	id := "run_00000000000000000000000000000001"
	cases := []struct {
		args []string
		code string
		exit int
	}{
		{[]string{"generate"}, "usage", 2},
		{[]string{"generate", "--unknown"}, "usage", 2},
		{[]string{"run", "unknown"}, "unknown_command", 2},
		{[]string{"run", "resume", "bad-id"}, "invalid_id", 3},
		{[]string{"run", "cancel", id}, "usage", 2},
		{[]string{"run", "events", id, "--after-version", "-1"}, "usage", 2},
		{[]string{"run", "list", "--limit", "-1"}, "invalid_argument", 2},
		{[]string{"run", "export", id}, "usage", 2},
		{[]string{"config", "effective"}, "usage", 2},
		{[]string{"config", "validate", "--unknown"}, "usage", 2},
		{[]string{"review", "retry", id, "--reviewer", "test", "--reason", "test", "--evidence", "bad"}, "digest_invalid", 2},
		{[]string{"review", "reject", id, "--reviewer", "test", "--reason", "test", "--patch", "file"}, "usage", 2},
	}
	bootstrap := func(context.Context, config.Config) (*application.Application, error) {
		t.Fatal("invalid arguments reached bootstrap")
		return nil, errors.New("unexpected bootstrap")
	}
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	for _, tc := range cases {
		t.Run(tc.args[0]+"_"+tc.code, func(t *testing.T) {
			var out, diagnostic bytes.Buffer
			args := append([]string{"--config", missing}, tc.args...)
			exit := RunWithDependencies(args, &out, &diagnostic, Dependencies{Bootstrap: bootstrap, BootstrapLocal: bootstrap})
			var result envelope
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatalf("%v: %v (%s)", args, err, diagnostic.String())
			}
			if exit != tc.exit || result.Error == nil || result.Error.Code != tc.code {
				t.Fatalf("%v: exit=%d output=%s", args, exit, out.String())
			}
		})
	}
}

func TestParsedRunIDIsIndependentOfFlagValues(t *testing.T) {
	id := "run_00000000000000000000000000000001"
	for _, args := range [][]string{
		{"run", "cancel", id, "--reason", "-foo"},
		{"run", "cancel", "--reason", "-foo", id},
		{"run", "cancel", "-reason=-foo", id},
		{"run", "cancel", "--reason=-foo", "--", id},
	} {
		flags := commandFlags("run cancel")
		reason := flags.String("reason", "", "reason")
		runID, err := parseRunFlags(flags, args[2:])
		if err != nil {
			t.Fatalf("%v: %v", args, err.err)
		}
		if string(runID) != id || *reason != "-foo" {
			t.Fatalf("%v: id=%s reason=%s", args, runID, *reason)
		}
	}
	command, err := parseStatefulCommand([]string{"run", "cancel", id, "--reason", "run_00000000000000000000000000000002"})
	if err != nil || string(command.runID) != id {
		t.Fatalf("reason changed run ID: %+v %v", command, err)
	}
}

func TestOptionBoundariesArePreserved(t *testing.T) {
	id := "run_00000000000000000000000000000001"
	for _, args := range [][]string{
		{"run", "cancel", id, "--reason"},
		{"run", "cancel", id, "--unknown", "value", "--reason", "test"},
		{"run", "cancel", "--reason", "test", "--", "--unknown"},
		{"run", "cancel", id, "--", "--reason", "test"},
		{"run", "events", "--after-version=2", id, "extra"},
	} {
		if command, err := parseStatefulCommand(args); err == nil {
			t.Fatalf("accepted %v: %+v", args, command)
		}
	}
	flags := commandFlags("run cancel")
	reason := flags.String("reason", "", "reason")
	runID, err := parseRunFlags(flags, []string{id, "--reason", "--"})
	if err != nil || *reason != "--" || string(runID) != id {
		t.Fatalf("lost literal flag value: id=%s reason=%s error=%v", runID, *reason, err)
	}
}

func TestDependencyDefaultsPreserveOverrides(t *testing.T) {
	d := (Dependencies{}).withDefaults()
	if d.GOOS == "" || d.Bootstrap == nil || d.BootstrapLocal == nil || d.CheckDocker == nil || d.RunWatchdog == nil {
		t.Fatal("incomplete dependency defaults")
	}
	called := false
	d = (Dependencies{GOOS: "custom", RunWatchdog: func(context.Context, string) error { called = true; return nil }}).withDefaults()
	if err := d.RunWatchdog(context.Background(), "control"); err != nil || !called || d.GOOS != "custom" {
		t.Fatal("defaults replaced injected dependency")
	}
}

func TestRootConfigFlagParsing(t *testing.T) {
	i, err := parseInvocation([]string{"--config=example.yaml", "run", "list"})
	if err != nil || i.configPath != "example.yaml" {
		t.Fatalf("inline config: %+v %v", i, err)
	}
	if _, err := parseInvocation([]string{"--config", "a", "--config", "b", "run", "list"}); err == nil {
		t.Fatal("accepted duplicate config")
	}
}
