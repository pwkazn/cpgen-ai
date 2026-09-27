package web

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestTaskReservationReleaseAfterStartDoesNotCreateCapacity(t *testing.T) {
	m := newTaskManager(1)
	reservation, err := m.reserve()
	if err != nil {
		t.Fatal(err)
	}
	releaseTask := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(releaseTask) }) }
	t.Cleanup(stop)
	if err := reservation.start("run_00000000000000000000000000000031", func(ctx context.Context) error {
		<-releaseTask
		return ctx.Err()
	}); err != nil {
		t.Fatal(err)
	}
	reservation.release()
	reservation.release()
	if extra, err := m.reserve(); !errors.Is(err, ErrCapacityFull) {
		if extra != nil {
			extra.release()
		}
		t.Fatalf("active task lost its capacity reservation: %v", err)
	}
	stop()
	awaitManagerEmpty(t, m)
	reused, err := m.reserve()
	if err != nil {
		t.Fatalf("completed task did not release capacity: %v", err)
	}
	reused.release()
}

func TestTaskManagerRejectsDuplicateRunWithoutConsumingAnotherSlot(t *testing.T) {
	m := newTaskManager(2)
	first, err := m.reserve()
	if err != nil {
		t.Fatal(err)
	}
	releaseTask := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(releaseTask) }) }
	t.Cleanup(stop)
	const id = "run_00000000000000000000000000000031"
	if err := first.start(id, func(context.Context) error { <-releaseTask; return nil }); err != nil {
		t.Fatal(err)
	}
	duplicate, err := m.reserve()
	if err != nil {
		t.Fatal(err)
	}
	if err := duplicate.start(id, func(context.Context) error {
		t.Error("duplicate executor was started")
		return nil
	}); !errors.Is(err, ErrRunActive) {
		t.Fatalf("duplicate run result: %v", err)
	}
	duplicate.release()
	other, err := m.reserve()
	if err != nil {
		t.Fatalf("duplicate start leaked capacity: %v", err)
	}
	other.release()
	stop()
	awaitManagerEmpty(t, m)
}

func TestDrainPreservesAdmittedTaskContext(t *testing.T) {
	m := newTaskManager(1)
	reservation, err := m.reserve()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan context.Context, 1)
	releaseTask := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(releaseTask) }) }
	t.Cleanup(stop)
	if err := reservation.start("run_00000000000000000000000000000031", func(ctx context.Context) error {
		started <- ctx
		<-releaseTask
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var taskContext context.Context
	select {
	case taskContext = <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("task did not start")
	}
	m.beginDrain()
	if extra, err := m.reserve(); !errors.Is(err, ErrDraining) {
		if extra != nil {
			extra.release()
		}
		t.Fatalf("draining accepted another task: %v", err)
	}
	if err := taskContext.Err(); err != nil {
		t.Fatalf("draining canceled the admitted task: %v", err)
	}
	stop()
	awaitManagerEmpty(t, m)
}

func awaitManagerEmpty(t *testing.T, m *taskManager) {
	t.Helper()
	done := make(chan struct{})
	go func() { m.waitEmpty(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("manager did not release all tasks and reservations")
	}
}

func TestExecutorPanicReleasesCapacityAndDoesNotExposeSecrets(t *testing.T) {
	m := newTaskManager(1)
	r, err := m.reserve()
	if err != nil {
		t.Fatal(err)
	}
	const id = "run_00000000000000000000000000000041"
	if err = r.start(id, func(context.Context) error { panic("secret-provider-token") }); err != nil {
		t.Fatal(err)
	}
	awaitManagerEmpty(t, m)
	if m.observe(id).LastError == "" {
		t.Fatal("panic not surfaced")
	}
	r, err = m.reserve()
	if err != nil {
		t.Fatal(err)
	}
	r.release()
}
