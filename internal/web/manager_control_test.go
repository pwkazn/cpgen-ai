package web

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestControlAndGenerationHaveIndependentCapacity(t *testing.T) {
	for _, controlFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("control_first=%t", controlFirst), func(t *testing.T) {
			m := newTaskManager(1)
			release, stop := managerTaskGate(t)
			started := make(chan struct{}, 2)
			fn := func(context.Context) error { started <- struct{}{}; <-release; return nil }
			reserveFirst, reserveSecond := m.reserve, m.reserveControl
			if controlFirst {
				reserveFirst, reserveSecond = reserveSecond, reserveFirst
			}
			first, err := reserveFirst()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(first.release)
			second, err := reserveSecond()
			if err != nil {
				t.Fatalf("one kind of reservation blocked the other: %v", err)
			}
			second.release()
			if err := first.start("run_00000000000000000000000000000051", fn); err != nil {
				t.Fatal(err)
			}
			awaitManagerTaskSignal(t, started)
			second, err = reserveSecond()
			if err != nil {
				t.Fatalf("one kind of executor blocked the other: %v", err)
			}
			t.Cleanup(second.release)
			if err := second.start("run_00000000000000000000000000000052", fn); err != nil {
				t.Fatal(err)
			}
			awaitManagerTaskSignal(t, started)
			if extra, err := m.reserve(); !errors.Is(err, ErrCapacityFull) {
				if extra != nil {
					extra.release()
				}
				t.Fatalf("generation exceeded its capacity: %v", err)
			}
			control, err := m.reserveControl()
			if err != nil {
				t.Fatalf("full generation capacity rejected cleanup: %v", err)
			}
			control.release()
			stop()
			awaitManagerEmpty(t, m)
		})
	}
}

func TestControlQueueIsSerialAndTrackedThroughDrain(t *testing.T) {
	m := newTaskManager(1)
	const count = 3
	started := make(chan int, count)
	stops := make([]func(), count)
	ids := make([]domain.RunID, count)
	for i := 0; i < count; i++ {
		gate, stop := managerTaskGate(t)
		stops[i] = stop
		ids[i] = domain.RunID(fmt.Sprintf("run_%032d", i+60))
		r, err := m.reserveControl()
		if err != nil {
			t.Fatal(err)
		}
		if err := r.startControl(ids[i], func(context.Context) error {
			started <- i
			<-gate
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	awaitManagerTaskIndex(t, started, 0)
	select {
	case i := <-started:
		t.Fatalf("cleanup %d ran while the first cleanup was blocked", i)
	case <-time.After(20 * time.Millisecond):
	}
	for _, id := range ids {
		if !m.isActive(id) || !m.observe(id).Active {
			t.Fatalf("queued cleanup is not tracked: %s", id)
		}
		duplicate, err := m.reserveControl()
		if err != nil {
			t.Fatal(err)
		}
		if err := duplicate.startControl(id, func(context.Context) error {
			t.Error("duplicate queued or running cleanup ran")
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		duplicate.release()
	}
	for _, reserve := range []func() (*slotReservation, error){m.reserve, m.reserveControl} {
		r, err := reserve()
		if err != nil {
			t.Fatal(err)
		}
		if err := r.start(ids[1], func(context.Context) error {
			t.Error("queued run started a duplicate executor")
			return nil
		}); !errors.Is(err, ErrRunActive) {
			t.Fatalf("queued run was not exclusive: %v", err)
		}
		r.release()
	}
	m.beginDrain()
	for _, reserve := range []func() (*slotReservation, error){m.reserve, m.reserveControl} {
		r, err := reserve()
		if !errors.Is(err, ErrDraining) {
			if r != nil {
				r.release()
			}
			t.Fatalf("draining admitted new work: %v", err)
		}
	}
	empty := make(chan struct{})
	go func() { m.waitEmpty(); close(empty) }()
	for i := 0; i < count; i++ {
		select {
		case <-empty:
			t.Fatalf("drain completed with cleanup %d still pending", i)
		default:
		}
		stops[i]()
		if i+1 < count {
			awaitManagerTaskIndex(t, started, i+1)
		}
	}
	awaitManagerTaskSignal(t, empty)
	for _, id := range ids {
		if m.isActive(id) || m.observe(id).Active {
			t.Fatalf("completed cleanup remained active: %s", id)
		}
	}
}

func TestControlFailureAndPanicAdvanceQueue(t *testing.T) {
	for _, panics := range []bool{false, true} {
		t.Run(fmt.Sprintf("panic=%t", panics), func(t *testing.T) {
			m := newTaskManager(1)
			gate, stop := managerTaskGate(t)
			started := make(chan struct{})
			const failedID = "run_00000000000000000000000000000071"
			r, err := m.reserveControl()
			if err != nil {
				t.Fatal(err)
			}
			if err := r.startControl(failedID, func(context.Context) error {
				close(started)
				<-gate
				if panics {
					panic("cleanup failed")
				}
				return errors.New("cleanup failed")
			}); err != nil {
				t.Fatal(err)
			}
			awaitManagerTaskSignal(t, started)
			nextRan := make(chan struct{})
			r, err = m.reserveControl()
			if err != nil {
				t.Fatal(err)
			}
			if err := r.startControl("run_00000000000000000000000000000072", func(context.Context) error {
				close(nextRan)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			stop()
			awaitManagerTaskSignal(t, nextRan)
			awaitManagerEmpty(t, m)
			if observation := m.observe(failedID); observation.Active || observation.LastError == "" {
				t.Fatalf("cleanup failure was not surfaced: %+v", observation)
			}
			generation, err := m.reserve()
			if err != nil {
				t.Fatalf("cleanup failure affected generation capacity: %v", err)
			}
			generation.release()
		})
	}
}

func TestControlFollowsGenerationExitAndCoalescesDuplicates(t *testing.T) {
	for _, outcome := range []string{"success", "error", "panic"} {
		t.Run(outcome, func(t *testing.T) {
			m := newTaskManager(1)
			generationGate, stopGeneration := managerTaskGate(t)
			controlGate, stopControl := managerTaskGate(t)
			generationStarted := make(chan struct{})
			controlStarted := make(chan struct{})
			const id = "run_00000000000000000000000000000081"
			r, err := m.reserve()
			if err != nil {
				t.Fatal(err)
			}
			if err := r.start(id, func(context.Context) error {
				close(generationStarted)
				<-generationGate
				switch outcome {
				case "error":
					return errors.New("generation failed")
				case "panic":
					panic("generation failed")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			awaitManagerTaskSignal(t, generationStarted)
			r, err = m.reserveControl()
			if err != nil {
				t.Fatal(err)
			}
			if err := r.startControl(id, func(context.Context) error {
				close(controlStarted)
				<-controlGate
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			r.release()
			duplicate, err := m.reserveControl()
			if err != nil {
				t.Fatal(err)
			}
			if err := duplicate.startControl(id, func(context.Context) error {
				t.Error("duplicate pending cleanup ran")
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-controlStarted:
				t.Fatal("cleanup overlapped the generation executor")
			case <-time.After(20 * time.Millisecond):
			}
			m.beginDrain()
			empty := make(chan struct{})
			go func() { m.waitEmpty(); close(empty) }()
			stopGeneration()
			awaitManagerTaskSignal(t, controlStarted)
			if !m.isActive(id) || !m.observe(id).Active {
				t.Fatal("followup cleanup was not tracked")
			}
			select {
			case <-empty:
				t.Fatal("drain did not wait for followup cleanup")
			default:
			}
			stopControl()
			awaitManagerTaskSignal(t, empty)
			if observation := m.observe(id); observation.Active || observation.LastError != "" {
				t.Fatalf("successful cleanup retained generation error: %+v", observation)
			}
		})
	}
}

func TestControlReservationAdmittedBeforeDrainCanStart(t *testing.T) {
	m := newTaskManager(1)
	r, err := m.reserveControl()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.release)
	m.beginDrain()
	empty := make(chan struct{})
	go func() { m.waitEmpty(); close(empty) }()
	select {
	case <-empty:
		t.Fatal("drain ignored the control reservation")
	case <-time.After(20 * time.Millisecond):
	}
	started := make(chan struct{})
	if err := r.startControl("run_00000000000000000000000000000091", func(context.Context) error {
		close(started)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	awaitManagerTaskSignal(t, started)
	awaitManagerTaskSignal(t, empty)
}

func managerTaskGate(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(stop)
	return gate, stop
}

func awaitManagerTaskSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for task manager")
	}
}

func awaitManagerTaskIndex(t *testing.T, started <-chan int, want int) {
	t.Helper()
	select {
	case got := <-started:
		if got != want {
			t.Fatalf("cleanup order: got %d, want %d", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("queued cleanup did not start")
	}
}
