package web

import (
	"bytes"
	"context"
	"errors"
	"log"
	"strings"
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
	lastError := m.observe(id).LastError
	if lastError == "" || !strings.Contains(lastError, "诊断编号") || strings.Contains(lastError, "secret-provider-token") {
		t.Fatal("panic not surfaced")
	}
	r, err = m.reserve()
	if err != nil {
		t.Fatal(err)
	}
	r.release()
}

func TestRedactLocalErrorRemovesProviderCredentials(t *testing.T) {
	message := `request failed Authorization: Bearer header-secret {"api_key":"config-secret","authorization":"Bearer json-secret"} https://user:pass@example.test/search?token=query-secret`
	redacted := redactLocalError(message, []string{"config-secret"})
	for _, secret := range []string{"header-secret", "config-secret", "json-secret", "user:pass", "query-secret"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("local diagnostic retained credential %q: %s", secret, redacted)
		}
	}
	if !strings.Contains(redacted, "[REDACTED]") {
		t.Fatalf("credential markers were not included: %s", redacted)
	}
}

func TestTaskFailureKeepsDetailsInRedactedLocalLogOnly(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })

	m := newTaskManager(1, "fixture-provider-secret")
	r, err := m.reserve()
	if err != nil {
		t.Fatal(err)
	}
	const id = "run_00000000000000000000000000000042"
	if err = r.start(id, func(context.Context) error {
		return errors.New("provider rejected request; api_key=fixture-provider-secret")
	}); err != nil {
		t.Fatal(err)
	}
	awaitManagerEmpty(t, m)

	lastError := m.observe(id).LastError
	if strings.Contains(lastError, "fixture-provider-secret") || strings.Contains(lastError, "provider rejected") || !strings.Contains(lastError, "诊断编号") {
		t.Fatalf("API observation leaked detail or lost its diagnostic reference: %s", lastError)
	}
	localLog := output.String()
	if !strings.Contains(localLog, "provider rejected") || !strings.Contains(localLog, "[REDACTED]") || strings.Contains(localLog, "fixture-provider-secret") {
		t.Fatalf("local diagnostic missing or retained provider credential: %s", localLog)
	}
}
