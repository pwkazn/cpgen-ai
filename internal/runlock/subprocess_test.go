package runlock_test

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/runlock"
)

// TestSubprocessLockReleaseAfterForcedExit catches a stale ownership scheme:
// the OS must release the advisory lock when the owning process is killed.
func TestSubprocessLockReleaseAfterForcedExit(t *testing.T) {
	if os.Getenv("CPGEN_LOCK_HELPER") == "1" {
		root := os.Getenv("CPGEN_LOCK_ROOT")
		id := domain.RunID(os.Getenv("CPGEN_LOCK_RUN_ID"))
		m, err := runlock.NewManager(root, runlock.Options{PollInterval: time.Millisecond})
		if err != nil {
			os.Exit(2)
		}
		guard, err := m.TryAcquireRun(id, runlock.Exclusive)
		if err != nil {
			os.Exit(3)
		}
		defer guard.Close()
		defer m.Close()
		_, _ = os.Stdout.WriteString("READY\n")
		select {}
	}

	root := t.TempDir()
	id := domain.RunID("run_0123456789abcdef0123456789abcdef")
	cmd := exec.Command(os.Args[0], "-test.run=^TestSubprocessLockReleaseAfterForcedExit$")
	cmd.Env = append(os.Environ(), "CPGEN_LOCK_HELPER=1", "CPGEN_LOCK_ROOT="+root, "CPGEN_LOCK_RUN_ID="+string(id))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := make(chan error, 1)
	go func() {
		line, err := bufio.NewReader(stdout).ReadString('\n')
		if err == nil && line != "READY\n" {
			err = os.ErrInvalid
		}
		ready <- err
	}()
	select {
	case err := <-ready:
		if err != nil {
			t.Fatalf("helper did not acquire lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not report READY")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	m, err := runlock.NewManager(root, runlock.Options{PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	guard, err := m.AcquireRun(ctx, id, runlock.Exclusive)
	if err != nil {
		t.Fatalf("lock remained stale after process death: %v", err)
	}
	defer guard.Close()
}
