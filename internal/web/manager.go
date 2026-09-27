package web

import (
	"context"
	"errors"
	"sync"

	"cpgen/internal/domain"
)

var (
	ErrCapacityFull = errors.New("task capacity is full")
	ErrRunActive    = errors.New("run already has an active executor")
	ErrDraining     = errors.New("server is draining")
)

type taskEntry struct {
	cancel context.CancelFunc
	done   chan struct{}
}
type executionObservation struct {
	Active    bool   `json:"active"`
	LastError string `json:"last_error,omitempty"`
}
type taskManager struct {
	mu                 sync.Mutex
	capacity, reserved int
	draining           bool
	active             map[domain.RunID]taskEntry
	observations       map[domain.RunID]executionObservation
	changed            chan struct{}
}
type slotReservation struct {
	manager *taskManager
	once    sync.Once
}

func newTaskManager(capacity int) *taskManager {
	if capacity < 1 {
		capacity = 1
	}
	return &taskManager{capacity: capacity, active: map[domain.RunID]taskEntry{}, observations: map[domain.RunID]executionObservation{}, changed: make(chan struct{})}
}
func (m *taskManager) signalLocked() { close(m.changed); m.changed = make(chan struct{}) }
func (m *taskManager) reserve() (*slotReservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return nil, ErrDraining
	}
	if len(m.active)+m.reserved >= m.capacity {
		return nil, ErrCapacityFull
	}
	m.reserved++
	m.signalLocked()
	return &slotReservation{manager: m}, nil
}
func (r *slotReservation) release() {
	if r == nil {
		return
	}
	r.once.Do(func() { m := r.manager; m.mu.Lock(); m.reserved--; m.signalLocked(); m.mu.Unlock() })
}
func (r *slotReservation) start(id domain.RunID, fn func(context.Context) error) error {
	if r == nil {
		return errors.New("task slot was not reserved")
	}
	var consumed bool
	r.once.Do(func() { consumed = true })
	if !consumed {
		return errors.New("task reservation was already consumed")
	}
	m := r.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reserved--
	if _, ok := m.active[id]; ok {
		m.signalLocked()
		return ErrRunActive
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := taskEntry{cancel: cancel, done: make(chan struct{})}
	m.active[id] = entry
	m.observations[id] = executionObservation{Active: true}
	m.signalLocked()
	go func() {
		var taskErr error
		defer func() {
			if recover() != nil {
				taskErr = errors.New("executor panic")
			}
			cancel()
			close(entry.done)
			m.mu.Lock()
			observation := executionObservation{}
			if taskErr != nil {
				observation.LastError = "执行器已停止，请检查本地配置或依赖；确认后可显式恢复任务"
			}
			m.observations[id] = observation
			delete(m.active, id)
			m.signalLocked()
			m.mu.Unlock()
		}()
		taskErr = fn(ctx)
	}()
	return nil
}

// Cleanup commands are tracked for shutdown but do not consume generation admission.
func (m *taskManager) reserveControl() (*slotReservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return nil, ErrDraining
	}
	m.reserved++
	m.signalLocked()
	return &slotReservation{manager: m}, nil
}
func (m *taskManager) cancel(id domain.RunID) bool {
	m.mu.Lock()
	entry, ok := m.active[id]
	m.mu.Unlock()
	if ok {
		entry.cancel()
	}
	return ok
}
func (m *taskManager) isActive(id domain.RunID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.active[id]
	return ok
}
func (m *taskManager) observe(id domain.RunID) executionObservation {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.observations[id]
}
func (m *taskManager) beginDrain() { m.mu.Lock(); m.draining = true; m.signalLocked(); m.mu.Unlock() }
func (m *taskManager) waitEmpty() {
	for {
		m.mu.Lock()
		if len(m.active) == 0 && m.reserved == 0 {
			m.mu.Unlock()
			return
		}
		changed := m.changed
		m.mu.Unlock()
		<-changed
	}
}
