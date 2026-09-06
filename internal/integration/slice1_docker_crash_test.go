//go:build cpgen_slice0_probe

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	dockersandbox "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/port"
	"cpgen/internal/watchdog"
)

func TestSlice1DockerCrash(t *testing.T) {
	// Perform capability probing in the parent so a missing daemon is an
	// explicit canary skip rather than a child that silently exits before READY.
	_, _, _ = loadSlice1DockerCanary(t)
	first := startSlice1DockerProcess(t, true)
	first.kill(t)
	firstOutput := first.output()
	second := startSlice1DockerProcess(t, false)
	secondOutput := waitSlice1DockerProcess(t, second)
	firstTrace := dockerTraceLine(firstOutput)
	secondTrace := dockerTraceLine(secondOutput)
	if firstTrace == "" || secondTrace == "" {
		t.Fatalf("Docker crash canary omitted trace: first=%q second=%q", firstOutput, secondOutput)
	}
	if firstTrace == secondTrace {
		t.Fatalf("Docker recovery reused the killed physical trace: %q", firstTrace)
	}
}

func TestSlice1DockerWatchdogFailure(t *testing.T) {
	_, _, _ = loadSlice1DockerCanary(t)
	marker := filepath.Join(t.TempDir(), "watchdog-result")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSlice1DockerWatchdogOwnerHelper$", "-test.v")
	cmd.Env = append(os.Environ(), "CPGEN_RUN_DOCKER_CANARY=1", "CPGEN_SLICE1_DOCKER_WATCHDOG_OWNER=1", "CPGEN_SLICE1_DOCKER_WATCHDOG_MARKER="+marker)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	var output bytes.Buffer
	go func() {
		scanner := bufio.NewScanner(stdout)
		seenReady := false
		for scanner.Scan() {
			line := scanner.Text()
			output.WriteString(line)
			output.WriteByte('\n')
			if strings.HasPrefix(line, "READY watchdog ") && !seenReady {
				seenReady = true
				ready <- nil
			}
		}
		if !seenReady {
			ready <- errors.Join(scanner.Err(), fmt.Errorf("watchdog owner exited before READY"))
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("watchdog owner readiness: %v; stderr=%q", err, stderr.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("watchdog owner readiness timed out; stderr=%q", stderr.String())
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("kill watchdog owner: %v", err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("watchdog owner unexpectedly exited cleanly")
	}
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if data, readErr := os.ReadFile(marker); readErr == nil {
			if string(data) != "ok" {
				t.Fatalf("watchdog EOF result = %q; owner output=%q stderr=%q", data, output.String(), stderr.String())
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("watchdog child did not observe owner EOF; output=%q stderr=%q", output.String(), stderr.String())
}

// TestSlice1DockerCrashHelper is a real owner process. Its first invocation
// is killed by the parent after the probe has produced its durable call trace;
// a second process reopens the Docker client and runs the same immutable probe
// path. This catches cleanup/retry state that accidentally lives in memory.
func TestSlice1DockerCrashHelper(t *testing.T) {
	if os.Getenv("CPGEN_SLICE1_DOCKER_HELPER") != "1" {
		return
	}
	harness := newSlice1DockerHarness(t)
	result, err := harness.Probe(context.Background(), port.ProfileCompileV2)
	if err != nil {
		t.Fatal(err)
	}
	if err := result.CallTrace.Validate(); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(result.CallTrace.PhysicalAttemptCallIDs))
	for i, id := range result.CallTrace.PhysicalAttemptCallIDs {
		ids[i] = string(id)
	}
	fmt.Fprintf(os.Stdout, "READY probe\nDONE trace=%s\n", strings.Join(ids, ","))
	if os.Getenv("CPGEN_SLICE1_DOCKER_HELPER_HOLD") == "1" {
		select {}
	}
}

type slice1DockerProcess struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	done   chan struct{}
	mu     sync.Mutex
}

func startSlice1DockerProcess(t *testing.T, hold bool) *slice1DockerProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSlice1DockerCrashHelper$", "-test.v")
	env := append(os.Environ(), "CPGEN_RUN_DOCKER_CANARY=1", "CPGEN_SLICE1_DOCKER_HELPER=1")
	if hold {
		env = append(env, "CPGEN_SLICE1_DOCKER_HELPER_HOLD=1")
	}
	cmd.Env = env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	process := &slice1DockerProcess{cmd: cmd, done: make(chan struct{})}
	cmd.Stderr = &process.stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		defer close(process.done)
		scanner := bufio.NewScanner(stdout)
		seenReady := false
		for scanner.Scan() {
			line := scanner.Text()
			process.mu.Lock()
			process.stdout.WriteString(line)
			process.stdout.WriteByte('\n')
			process.mu.Unlock()
			if strings.HasPrefix(line, "READY ") && !seenReady {
				seenReady = true
				ready <- nil
			}
		}
		if !seenReady {
			ready <- errors.Join(scanner.Err(), fmt.Errorf("Docker helper exited before READY"))
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("Docker helper readiness: %v; stderr=%q", err, process.stderr.String())
		}
	case <-time.After(2 * time.Minute):
		t.Fatalf("Docker helper readiness timed out; stderr=%q", process.stderr.String())
	}
	return process
}

func (p *slice1DockerProcess) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.stdout.String()
}

func (p *slice1DockerProcess) kill(t *testing.T) {
	t.Helper()
	if p.cmd.ProcessState == nil {
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Fatalf("kill Docker helper: %v", err)
		}
	}
	if p.cmd.ProcessState == nil {
		if err := p.cmd.Wait(); err != nil && p.cmd.ProcessState == nil {
			t.Fatalf("wait Docker helper: %v", err)
		}
	}
	<-p.done
}

func waitSlice1DockerProcess(t *testing.T, p *slice1DockerProcess) string {
	t.Helper()
	if err := p.cmd.Wait(); err != nil {
		t.Fatalf("Docker helper resume: %v; stderr=%q", err, p.stderr.String())
	}
	<-p.done
	return p.output()
}

func dockerTraceLine(output string) string {
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "DONE trace=") {
			return strings.TrimPrefix(line, "DONE trace=")
		}
	}
	return ""
}

// TestSlice1DockerWatchdogOwnerHelper keeps a detached watchdog session open.
// The parent kills this owner, causing the watchdog's real control connection
// to receive EOF and run its reconciler before exiting.
func TestSlice1DockerWatchdogOwnerHelper(t *testing.T) {
	if os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_OWNER") != "1" {
		return
	}
	config, _, static := loadSlice1DockerCanary(t)
	marker := os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_MARKER")
	if marker == "" {
		t.Fatal("watchdog marker is required")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := dockersandbox.NewDetachedWatchdogController(dockersandbox.DetachedWatchdogOptions{
		Config: config, EngineIdentity: static.EngineIdentityDigest, ControlDirectory: filepath.Join(filepath.Dir(marker), "watchdog-control"),
		Executable: executable, ArmTimeout: 20 * time.Second,
		CommandFactory: func(executable, controlPath string) *exec.Cmd {
			command := exec.Command(executable, "-test.run=^TestSlice1DockerWatchdogServiceChild$", "-test.v")
			command.Env = append(os.Environ(), "CPGEN_SLICE1_DOCKER_WATCHDOG_CHILD=1", "CPGEN_SLICE1_DOCKER_WATCHDOG_CONTROL="+controlPath, "CPGEN_SLICE1_DOCKER_WATCHDOG_MARKER="+marker)
			return command
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := port.NewContainerPlan(static.EngineIdentityDigest, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	record := watchdog.ControlRecord{
		SchemaVersion: watchdog.ControlRecordSchemaVersion, TokenDigest: controller.TokenDigest(),
		EngineEndpoint: config.EngineEndpoint, EngineIdentityDigest: static.EngineIdentityDigest,
		LogicalOperationID: "slice1-watchdog-eof", Plan: plan,
		SafetyDeadlineUTC: time.Now().Add(20 * time.Second).UTC(), CleanupDeadlineUTC: time.Now().Add(40 * time.Second).UTC(),
	}
	session, err := controller.Arm(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	evidence, ok := session.(dockersandbox.WatchdogSessionEvidence)
	if !ok {
		t.Fatal("detached watchdog omitted control-file evidence")
	}
	fmt.Fprintf(os.Stdout, "READY watchdog %s\n", evidence.ControlRecordRef())
	select {}
}

func TestSlice1DockerWatchdogServiceChild(t *testing.T) {
	if os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_CHILD") != "1" {
		return
	}
	control := os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_CONTROL")
	marker := os.Getenv("CPGEN_SLICE1_DOCKER_WATCHDOG_MARKER")
	err := dockersandbox.RunWatchdogService(context.Background(), control)
	if err == nil {
		if _, statErr := os.Stat(control); !errors.Is(statErr, os.ErrNotExist) {
			err = fmt.Errorf("watchdog control file was not cleaned: %v", statErr)
		} else if _, statErr := os.Stat(filepath.Dir(control)); !errors.Is(statErr, os.ErrNotExist) {
			err = fmt.Errorf("watchdog control directory was not cleaned: %v", statErr)
		}
	}
	value := "ok"
	if err != nil {
		value = "error: " + err.Error()
	}
	if writeErr := os.WriteFile(marker, []byte(value), 0o600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
}
