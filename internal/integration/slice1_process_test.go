package integration_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"cpgen/internal/cli"
	"cpgen/internal/domain"
)

func TestSlice1ProcessOneExecutorPerRunAndForcedKillReleasesLock(t *testing.T) {
	env := newIntegrationEnvironment(t, "review")
	app := openIntegrationApp(t, env)
	snapshot, err := app.Runs.Generate(context.Background(), integrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != domain.RunNeedsReview {
		t.Fatalf("generated snapshot = %+v, want NEEDS_REVIEW", snapshot)
	}
	if err := app.Close(); err != nil {
		t.Fatal(err)
	}

	first := startIntegrationHelper(t, env, "hold-lock", snapshot.RunID)
	_ = startIntegrationHelperExpectBusy(t, env, "hold-lock", snapshot.RunID)
	first.kill(t)
	// Once the first process dies, the OS advisory lock is released. A fresh
	// real process can acquire it without a stale owner file or lease.
	restarted := startIntegrationHelper(t, env, "hold-lock", snapshot.RunID)
	restarted.kill(t)
}

func TestSlice1ProcessDifferentRunsProgressConcurrently(t *testing.T) {
	env := newIntegrationEnvironment(t, "review")
	app := openIntegrationApp(t, env)
	first, err := app.Runs.Generate(context.Background(), integrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	second, err := app.Runs.Generate(context.Background(), integrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	left := startIntegrationHelper(t, env, "hold-lock", first.RunID)
	right := startIntegrationHelper(t, env, "hold-lock", second.RunID)
	left.kill(t)
	right.kill(t)
}

func TestSlice1ProcessReadOnlyViewsAndCancelDuringExecution(t *testing.T) {
	env := newIntegrationEnvironment(t, "review")
	app := openIntegrationApp(t, env)
	snapshot, err := app.Runs.Generate(context.Background(), integrationRequest())
	if err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	holder := startIntegrationHelper(t, env, "hold-lock", snapshot.RunID)
	defer holder.kill(t)

	showCode, showOut, showErr := runCLIDirect(env.configPath, "run", "show", string(snapshot.RunID))
	if showCode != 0 || len(showErr) != 0 {
		t.Fatalf("run show while owner holds lock: code=%d stderr=%q", showCode, showErr)
	}
	show := decodeEnvelope(t, showOut)
	if show["status"] != string(domain.RunNeedsReview) {
		t.Fatalf("show status = %#v", show["status"])
	}
	eventsCode, eventsOut, eventsErr := runCLIDirect(env.configPath, "run", "events", string(snapshot.RunID))
	if eventsCode != 0 || len(eventsErr) != 0 {
		t.Fatalf("run events while owner holds lock: code=%d stderr=%q", eventsCode, eventsErr)
	}
	events := decodeEnvelope(t, eventsOut)
	if events["status"] != "OK" {
		t.Fatalf("events status = %#v", events["status"])
	}

	patchPath := filepath.Join(env.root, "revision.patch")
	if err := os.WriteFile(patchPath, []byte("integration revision"), 0o600); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		reviewCode, reviewOut, reviewErr := runCLI(t, env.configPath, "review", "revise", string(snapshot.RunID), "--reviewer", "integration", "--reason", "lock-boundary", "--step", "checkpoint", "--patch", patchPath)
		if reviewCode != 4 || len(reviewErr) != 0 {
			t.Fatalf("review mutation while owner holds lock: code=%d out=%q err=%q", reviewCode, reviewOut, reviewErr)
		}
		review := decodeEnvelope(t, reviewOut)
		if review["error"].(map[string]any)["code"] != "lock_busy" {
			t.Fatalf("review conflict = %#v", review)
		}
	}

	// Cancellation is a durable request and is allowed to race with a live
	// owner. The owner still owns execution; it must observe the request after
	// the lock is released rather than having the reader mutate its projection.
	request := domain.CancelRequest{ID: "control_00000000000000000000000000000002", RunID: snapshot.RunID, ExpectedRunVersion: snapshot.Version, Reason: "integration cancel", IdempotencyKey: "control_00000000000000000000000000000002", At: time.Now().UTC()}
	cancelApp := openIntegrationApp(t, env)
	if _, err := cancelApp.Runtime.RequestCancel(context.Background(), request); err != nil {
		t.Fatalf("cancel request while owner holds lock: %v", err)
	}
	pending, err := cancelApp.Runtime.PendingCancel(context.Background(), snapshot.RunID)
	if err != nil || pending == nil || pending.ID != request.ID {
		t.Fatalf("pending cancel = %+v, %v", pending, err)
	}
}

func runCLIDirect(configPath string, args ...string) (int, []byte, []byte) {
	var stdout, stderr bytes.Buffer
	code := cli.Run(append([]string{"--config", configPath}, args...), &stdout, &stderr)
	return code, stdout.Bytes(), stderr.Bytes()
}

// startIntegrationHelperExpectBusy verifies the documented conflict without
// leaving a child process around. It still starts the real helper, which must
// report the lock conflict before it can emit READY.
func startIntegrationHelperExpectBusy(t *testing.T, env integrationEnvironment, action string, runID domain.RunID) *helperProcess {
	t.Helper()
	cmd := startRawIntegrationHelper(t, env, action, runID)
	// The child is expected to exit quickly after TryAcquireRun returns
	// ErrBusy. Give it a bounded wait rather than assuming a scheduler order.
	wait := make(chan error, 1)
	go func() { wait <- cmd.cmd.Wait() }()
	select {
	case err := <-wait:
		if err == nil {
			t.Fatalf("busy helper unexpectedly succeeded")
		}
	case <-time.After(5 * time.Second):
		_ = cmd.cmd.Process.Kill()
		<-wait
		t.Fatalf("busy helper did not exit; stderr=%q", cmd.stderr.String())
	}
	return cmd
}

func startRawIntegrationHelper(t *testing.T, env integrationEnvironment, action string, runID domain.RunID) *helperProcess {
	t.Helper()
	cmd := execCommandHelper(env, action, runID)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	helper := &helperProcess{cmd: cmd, stdout: stdout}
	cmd.Stderr = &helper.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return helper
}

func execCommandHelper(env integrationEnvironment, action string, runID domain.RunID) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "-test.run=^TestSlice1IntegrationHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "CPGEN_SLICE1_HELPER=1", "CPGEN_SLICE1_HELPER_ACTION="+action, "CPGEN_SLICE1_CONFIG="+env.configPath, "CPGEN_SLICE1_RUN_ID="+string(runID))
	return cmd
}
