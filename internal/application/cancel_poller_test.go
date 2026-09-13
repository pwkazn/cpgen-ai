package application

import (
	"context"
	"testing"
	"time"

	"cpgen/internal/adapter/fake"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/runlock"
)

type observedPollClock struct {
	*clock.Fake
	scheduled chan time.Duration
}

func (c *observedPollClock) After(d time.Duration) <-chan time.Time {
	ch := c.Fake.After(d)
	c.scheduled <- d
	return ch
}

type cancelPollStore struct {
	port.RuntimeStore
	calls   chan struct{}
	pending bool
}

func (s *cancelPollStore) PendingCancel(context.Context, domain.RunID) (*domain.ControlRequest, error) {
	s.calls <- struct{}{}
	if s.pending {
		return &domain.ControlRequest{}, nil
	}
	return nil, nil
}
func TestCancelPollerUsesConfiguredClockAndStops(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(map[bool]string{false: "context_exit", true: "cancel_signal"}[pending], func(t *testing.T) {
			source := &observedPollClock{Fake: clock.NewFake(time.Now()), scheduled: make(chan time.Duration, 2)}
			store := &cancelPollStore{calls: make(chan struct{}, 2), pending: pending}
			pipeline, err := fake.NewPipeline(fake.NewPrepareStep(fake.PrepareCapabilities{}), fake.NewExerciseStep(fake.ExerciseCapabilities{}), fake.NewCheckpointStep(fake.CheckpointCapabilities{}))
			if err != nil {
				t.Fatal(err)
			}
			service, err := NewRunService(RunServiceConfig{Runtime: store, Clock: source, Locks: &runlock.Manager{}, Pipeline: pipeline, ControlPollInterval: 750 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defaults, err := NewRunService(RunServiceConfig{Runtime: store, Clock: source, Locks: &runlock.Manager{}, Pipeline: pipeline})
			if err != nil || defaults.control.controlPollInterval != 100*time.Millisecond {
				t.Fatalf("default interval: service=%+v err=%v", defaults, err)
			}
			ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			done := make(chan struct{})
			cancelled := make(chan struct{}, 1)
			go service.control.cancelPoller(ctx, "run", func() { cancelled <- struct{}{} }, done)
			if got := awaitPoll(t, source.scheduled); got != 750*time.Millisecond {
				t.Fatalf("interval = %v", got)
			}
			source.Advance(749 * time.Millisecond)
			select {
			case <-store.calls:
				t.Fatal("polled before interval")
			default:
			}
			source.Advance(time.Millisecond)
			awaitPoll(t, store.calls)
			if pending {
				awaitPoll(t, cancelled)
			} else {
				awaitPoll(t, source.scheduled)
				stop()
			}
			awaitPoll(t, done)
			select {
			case <-store.calls:
				t.Fatal("poller continued after exit")
			default:
			}
		})
	}
}

func awaitPoll[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("poller did not respond")
		var zero T
		return zero
	}
}
