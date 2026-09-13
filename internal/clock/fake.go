package clock

import (
	"sync"
	"time"
)

type fakeTimer struct {
	deadline time.Time
	channel  chan time.Time
}

type Fake struct {
	mu     sync.Mutex
	now    time.Time
	timers []fakeTimer
}

func NewFake(now time.Time) *Fake { return &Fake{now: now} }

func (f *Fake) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *Fake) After(duration time.Duration) <-chan time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	channel := make(chan time.Time, 1)
	deadline := f.now.Add(duration)
	if duration <= 0 {
		channel <- f.now
		return channel
	}
	f.timers = append(f.timers, fakeTimer{deadline: deadline, channel: channel})
	return channel
}

func (f *Fake) Advance(duration time.Duration) {
	if duration < 0 {
		panic("fake clock cannot move backwards")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(duration)
	pending := f.timers[:0]
	for _, timer := range f.timers {
		if !timer.deadline.After(f.now) {
			timer.channel <- timer.deadline
			continue
		}
		pending = append(pending, timer)
	}
	f.timers = pending
}
