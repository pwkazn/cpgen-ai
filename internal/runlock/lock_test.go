package runlock_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/runlock"
)

func newManager(t *testing.T, interval time.Duration) (*runlock.Manager, string) {
	t.Helper()
	root := t.TempDir()
	m, err := runlock.NewManager(root, runlock.Options{PollInterval: interval})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	return m, root
}

func testRunID(suffix string) domain.RunID {
	return domain.RunID("run_0123456789abcdef0123456789abcde" + suffix)
}

// TestLockContentionAndSharedAccess catches using an in-memory or no-op lock
// implementation that fails to exclude conflicting guards.
func TestLockContentionAndSharedAccess(t *testing.T) {
	t.Parallel()
	// Separate the configured retry interval from scheduling/OS overhead so
	// race instrumentation cannot turn this into a one-millisecond benchmark.
	m, _ := newManager(t, 10*time.Second)
	id := testRunID("d")
	exclusive, err := m.TryAcquireRun(id, runlock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer exclusive.Close()
	if _, err := m.TryAcquireRun(id, runlock.Exclusive); !errors.Is(err, runlock.ErrBusy) {
		t.Fatalf("exclusive conflict = %v, want ErrBusy", err)
	}
	started := time.Now()
	if _, err := m.TryAcquireRun(id, runlock.Exclusive); !errors.Is(err, runlock.ErrBusy) {
		t.Fatalf("second exclusive conflict = %v, want ErrBusy", err)
	}
	if elapsed := time.Since(started); elapsed >= time.Second {
		t.Fatalf("TryAcquire polled instead of making one attempt: %v", elapsed)
	}
	if _, err := m.TryAcquireRun(id, runlock.Shared); !errors.Is(err, runlock.ErrBusy) {
		t.Fatalf("shared/exclusive conflict = %v, want ErrBusy", err)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatal(err)
	}
	first, err := m.TryAcquireRun(id, runlock.Shared)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := m.TryAcquireRun(id, runlock.Shared)
	if err != nil {
		t.Fatalf("shared/shared acquire: %v", err)
	}
	defer second.Close()
}

// TestLockAcquireHonorsContextAndPollInterval catches a blocking acquisition
// that spins, ignores cancellation, or polls at an unconfigured cadence.
func TestLockAcquireHonorsContextAndPollInterval(t *testing.T) {
	t.Parallel()
	interval := 25 * time.Millisecond
	m, _ := newManager(t, interval)
	held, err := m.TryAcquireArtifacts(runlock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = m.AcquireArtifacts(ctx, runlock.Exclusive)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("AcquireArtifacts = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed < interval {
		t.Fatalf("AcquireArtifacts returned before configured poll interval: %v < %v", elapsed, interval)
	}
}

// TestLockAcquireRejectsCancelledContextBeforeTrying catches acquiring an
// available lock after its caller has already cancelled the operation.
func TestLockAcquireRejectsCancelledContextBeforeTrying(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t, time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if guard, err := m.AcquireArtifacts(ctx, runlock.Exclusive); !errors.Is(err, context.Canceled) || guard != nil {
		if guard != nil {
			_ = guard.Close()
		}
		t.Fatalf("AcquireArtifacts with cancelled context = %v, %v; want nil, context.Canceled", guard, err)
	}
}

// TestLockAcquireChecksCancellationAtPollBoundary catches retrying after a
// cancellation races the polling timer and the conflicting guard is released.
func TestLockAcquireChecksCancellationAtPollBoundary(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t, time.Millisecond)
	held, err := m.TryAcquireArtifacts(runlock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &boundaryCancelledContext{}
	type result struct {
		guard *runlock.Guard
		err   error
	}
	resultCh := make(chan result, 1)
	go func() {
		guard, err := m.AcquireArtifacts(ctx, runlock.Exclusive)
		resultCh <- result{guard, err}
	}()
	select {
	case result := <-resultCh:
		if result.guard != nil || !errors.Is(result.err, context.Canceled) {
			t.Fatalf("AcquireArtifacts at cancellation boundary = %v, %v; want nil, context.Canceled", result.guard, result.err)
		}
	case <-time.After(100 * time.Millisecond):
		_ = held.Close()
		result := <-resultCh
		if result.guard != nil {
			_ = result.guard.Close()
		}
		t.Fatal("AcquireArtifacts retried instead of observing cancellation at the poll boundary")
	}
	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
}

type boundaryCancelledContext struct {
	checks int
	done   chan struct{}
}

func (c *boundaryCancelledContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *boundaryCancelledContext) Done() <-chan struct{} {
	return c.doneChannel()
}
func (c *boundaryCancelledContext) Err() error {
	c.checks++
	if c.checks > 1 {
		close(c.doneChannel())
		return context.Canceled
	}
	return nil
}
func (c *boundaryCancelledContext) Value(any) any { return nil }
func (c *boundaryCancelledContext) doneChannel() chan struct{} {
	if c.done == nil {
		c.done = make(chan struct{})
	}
	return c.done
}

// TestLockScopesAndLifetime catches accepting malformed identifiers, deleting
// lock files, or returning guards with incorrect scope/lifetime semantics.
func TestLockScopesAndLifetime(t *testing.T) {
	t.Parallel()
	m, root := newManager(t, time.Millisecond)
	if _, err := m.TryAcquireRun(domain.RunID("../../escape"), runlock.Exclusive); err == nil {
		t.Fatal("invalid RunID selected a lock path")
	}
	id := testRunID("e")
	guard, err := m.TryAcquireRun(id, runlock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	if guard.Scope() != runlock.ScopeRun || !guard.Held() {
		t.Fatal("run guard has wrong scope or is not held")
	}
	if got, ok := guard.RunID(); !ok || got != id {
		t.Fatalf("RunID() = %q, %v", got, ok)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if guard.Held() {
		t.Fatal("closed guard remains held")
	}
	if err := guard.Close(); err != nil {
		t.Fatalf("double Close: %v", err)
	}
	info, err := os.Lstat(filepath.Join(root, string(id)+".lock"))
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("permanent regular lock file: %v, mode=%v", err, info.Mode())
	}
	if err := os.Link(filepath.Join(root, string(id)+".lock"), filepath.Join(root, "duplicate.lock")); err != nil {
		t.Fatalf("prepare hard-linked lock attack: %v", err)
	}
	if _, err := m.TryAcquireRun(id, runlock.Exclusive); err == nil {
		t.Fatal("hard-linked lock file was accepted")
	}
	if err := (&runlock.Guard{}).Close(); err == nil {
		t.Fatal("zero Guard Close succeeded")
	}
	artifact, err := m.TryAcquireArtifacts(runlock.Shared)
	if err != nil {
		t.Fatal(err)
	}
	defer artifact.Close()
	if artifact.Scope() != runlock.ScopeArtifacts {
		t.Fatal("artifact guard has wrong scope")
	}
	if _, ok := artifact.RunID(); ok {
		t.Fatal("artifact guard exposed a fake RunID")
	}
}

// TestLockDifferentRunsProceedConcurrently catches accidentally using a global
// lock for all runs instead of deterministic per-run ownership.
func TestLockDifferentRunsProceedConcurrently(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t, time.Millisecond)
	first, err := m.TryAcquireRun(testRunID("f"), runlock.Exclusive)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := m.TryAcquireRun(testRunID("0"), runlock.Exclusive)
	if err != nil {
		t.Fatalf("different run blocked: %v", err)
	}
	defer second.Close()
}

// TestLockManagerCloseIsSafe catches a manager close racing normal teardown.
func TestLockManagerCloseIsSafe(t *testing.T) {
	t.Parallel()
	m, _ := newManager(t, time.Millisecond)
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("double manager Close: %v", err)
	}
	if _, err := m.TryAcquireArtifacts(runlock.Exclusive); err == nil {
		t.Fatal("closed manager acquired a guard")
	}
}
