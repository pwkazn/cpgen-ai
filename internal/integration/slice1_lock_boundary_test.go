package integration_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"cpgen/internal/cli"
	"cpgen/internal/domain"
)

func TestSlice1LockBoundaryDifferentRunAndReadOnlyProgressWithinBusyBound(t *testing.T) {
	env := newIntegrationEnvironment(t, "review")
	app := openIntegrationApp(t, env)
	first, err := app.Runs.Generate(context.Background(), integrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}
	holder := startIntegrationHelper(t, env, "hold-lock", first.RunID)
	defer holder.kill(t)

	// Holding run A's advisory lock must not hold SQLite's writer connection
	// while a separate process executes run B. This is the observable
	// transaction-boundary contract for network/Docker/blob preparation: all
	// durable preparation is complete before an external call can block.
	started := time.Now()
	secondCode, secondOut, secondErr := runCLIDirect(env.configPath, "generate", "--request", env.requestPath)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("different run remained blocked for %s", elapsed)
	}
	if secondCode != 6 || len(secondErr) != 0 {
		t.Fatalf("different run while run A lock held: code=%d out=%q err=%q", secondCode, secondOut, secondErr)
	}

	// Read-only projections are served from a fresh SQLite connection while
	// the owner remains alive, and must never attempt to acquire the run lock.
	for _, command := range [][]string{{"run", "show", string(first.RunID)}, {"run", "events", string(first.RunID)}} {
		var stdout, stderr bytes.Buffer
		code := runCLIDirectInto(&stdout, &stderr, env.configPath, command...)
		if code != 0 || stderr.Len() != 0 {
			t.Fatalf("read-only %v while lock held: code=%d out=%q err=%q", command, code, stdout.String(), stderr.String())
		}
	}
}

func TestSlice1LockBoundaryCancellationIsDurableAndIdempotent(t *testing.T) {
	env := newIntegrationEnvironment(t, "review")
	app := openIntegrationApp(t, env)
	snapshot, err := app.Runs.Generate(context.Background(), integrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	holder := startIntegrationHelper(t, env, "hold-lock", snapshot.RunID)
	defer holder.kill(t)
	request := domain.CancelRequest{ID: "control_00000000000000000000000000000002", RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, Reason: "lock-boundary", IdempotencyKey: "control_00000000000000000000000000000002", At: time.Now().UTC()}
	cancelApp := openIntegrationApp(t, env)
	first, err := cancelApp.Runtime.RequestCancel(context.Background(), request)
	if err != nil || !first.Active {
		t.Fatalf("first cancel request: %+v, %v", first, err)
	}
	// A second request is sent with the same current version by the CLI. The
	// existing pending request wins, never charging a second event or creating
	// a conflicting control row while the owner still holds the lock.
	second, err := cancelApp.Runtime.RequestCancel(context.Background(), request)
	if err != nil || second.ID != first.ID || second.RunVersion != first.RunVersion {
		t.Fatalf("repeated cancel: %+v, %v", second, err)
	}
	events, err := cancelApp.Runtime.Events(context.Background(), snapshot.RunID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(events)) != snapshot.Version+1 {
		t.Fatalf("repeated cancel appended an event: got %d events", len(events))
	}
}

func runCLIDirectInto(stdout, stderr *bytes.Buffer, configPath string, args ...string) int {
	// Keep this tiny adapter local to the integration package so tests use the
	// same exported CLI API as a caller, without shell parsing or a subprocess.
	return cli.Run(append([]string{"--config", configPath}, args...), stdout, stderr)
}
