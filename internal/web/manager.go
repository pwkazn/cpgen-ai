package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"cpgen/internal/domain"
)

var (
	ErrCapacityFull = errors.New("task capacity is full")
	ErrRunActive    = errors.New("run already has an active executor")
	ErrDraining     = errors.New("server is draining")
)

type taskEntry struct {
	cancel  context.CancelFunc
	done    chan struct{}
	control bool
}
type controlTask struct {
	id    domain.RunID
	ctx   context.Context
	entry taskEntry
	fn    func(context.Context) error
}
type executionObservation struct {
	Active    bool   `json:"active"`
	LastError string `json:"last_error,omitempty"`
}
type taskManager struct {
	mu                             sync.Mutex
	capacity, reserved             int
	generationActive               int
	controlReserved, controlActive int
	controlQueue                   []controlTask
	pendingControl                 map[domain.RunID]controlTask
	draining                       bool
	active                         map[domain.RunID]taskEntry
	observations                   map[domain.RunID]executionObservation
	secrets                        []string
	changed                        chan struct{}
}
type slotReservation struct {
	manager *taskManager
	once    sync.Once
	control bool
}

func newTaskManager(capacity int, secrets ...string) *taskManager {
	if capacity < 1 {
		capacity = 1
	}
	private := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		if secret != "" {
			private = append(private, secret)
		}
	}
	return &taskManager{capacity: capacity, active: map[domain.RunID]taskEntry{}, pendingControl: map[domain.RunID]controlTask{}, observations: map[domain.RunID]executionObservation{}, secrets: private, changed: make(chan struct{})}
}
func (m *taskManager) signalLocked() { close(m.changed); m.changed = make(chan struct{}) }
func (m *taskManager) reserve() (*slotReservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return nil, ErrDraining
	}
	if m.generationActive+m.reserved >= m.capacity {
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
	r.once.Do(func() {
		m := r.manager
		m.mu.Lock()
		r.releaseLocked()
		m.signalLocked()
		m.mu.Unlock()
	})
}
func (r *slotReservation) releaseLocked() {
	if r.control {
		r.manager.controlReserved--
	} else {
		r.manager.reserved--
	}
}
func (r *slotReservation) start(id domain.RunID, fn func(context.Context) error) error {
	return r.startTask(id, fn, false)
}

// startControl ensures a cleanup runs after any generation executor for the run
// exits. Concurrent requests share the already queued or running cleanup.
func (r *slotReservation) startControl(id domain.RunID, fn func(context.Context) error) error {
	if r == nil || !r.control {
		return errors.New("control slot was not reserved")
	}
	return r.startTask(id, fn, true)
}

func (r *slotReservation) startTask(id domain.RunID, fn func(context.Context) error, followGeneration bool) error {
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
	r.releaseLocked()
	active, alreadyActive := m.active[id]
	if alreadyActive {
		if !followGeneration {
			m.signalLocked()
			return ErrRunActive
		}
		if _, pending := m.pendingControl[id]; active.control || pending {
			m.signalLocked()
			return nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	entry := taskEntry{cancel: cancel, done: make(chan struct{}), control: r.control}
	if alreadyActive {
		m.pendingControl[id] = controlTask{id: id, ctx: ctx, entry: entry, fn: fn}
		m.signalLocked()
		return nil
	}
	m.active[id] = entry
	m.observations[id] = executionObservation{Active: true}
	if r.control {
		m.controlQueue = append(m.controlQueue, controlTask{id: id, ctx: ctx, entry: entry, fn: fn})
		m.startNextControlLocked()
	} else {
		m.generationActive++
		go m.execute(id, ctx, entry, fn)
	}
	m.signalLocked()
	return nil
}

// A single cleanup executor bounds resource cleanup independently of generation.
// Queued commands remain active for run exclusion, observations, and shutdown.
func (m *taskManager) startNextControlLocked() {
	if m.controlActive != 0 || len(m.controlQueue) == 0 {
		return
	}
	task := m.controlQueue[0]
	m.controlQueue[0] = controlTask{}
	m.controlQueue = m.controlQueue[1:]
	m.controlActive++
	go m.execute(task.id, task.ctx, task.entry, task.fn)
}

func (m *taskManager) execute(id domain.RunID, ctx context.Context, entry taskEntry, fn func(context.Context) error) {
	var taskErr error
	panicked := false
	panicType := ""
	var panicStack []byte
	defer func() {
		if recovered := recover(); recovered != nil {
			panicked = true
			panicType = fmt.Sprintf("%T", recovered)
			panicStack = debug.Stack()
			taskErr = errors.New("executor panic")
		}
		diagnostic := ""
		lastError := ""
		if taskErr != nil {
			diagnostic = newDiagnosticID()
			lastError = "执行器已停止（诊断编号 " + diagnostic + "）；请查看本机服务日志，确认后可显式恢复任务"
			log.Printf("cpgen executor stopped run=%s diagnostic=%s panic=%t panic_type=%s error=%s", id, diagnostic, panicked, panicType, redactLocalError(taskErr.Error(), m.secrets))
			if panicked {
				log.Printf("cpgen executor panic diagnostic=%s stack=%q", diagnostic, panicStack)
			}
		}
		entry.cancel()
		m.mu.Lock()
		m.observations[id] = executionObservation{LastError: lastError}
		delete(m.active, id)
		if entry.control {
			m.controlActive--
			m.startNextControlLocked()
		} else {
			m.generationActive--
			if pending, ok := m.pendingControl[id]; ok {
				delete(m.pendingControl, id)
				m.active[id] = pending.entry
				m.observations[id] = executionObservation{Active: true}
				m.controlQueue = append(m.controlQueue, pending)
				m.startNextControlLocked()
			}
		}
		close(entry.done)
		m.signalLocked()
		m.mu.Unlock()
	}()
	taskErr = fn(ctx)
}

var (
	bearerCredential  = regexp.MustCompile(`(?i)((?:["']?authorization["']?\s*:\s*["']?bearer\s+))[^"'\s,;]+["']?`)
	labeledCredential = regexp.MustCompile(`(?i)((?:["']?(?:api[ _-]?key|access[ _-]?token|refresh[ _-]?token|password|passwd|secret|credential)["']?\s*[:=]\s*))("[^"]*"|'[^']*'|[^\s,;]+)`)
	urlCredential     = regexp.MustCompile(`(?i)(https?://)[^/@\s]+@`)
	queryCredential   = regexp.MustCompile(`(?i)([?&](?:api[_-]?key|access[_-]?token|refresh[_-]?token|token|key|password|secret)=)[^&#\s]+`)
)

func redactLocalError(message string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			message = strings.ReplaceAll(message, secret, "[REDACTED]")
		}
	}
	message = bearerCredential.ReplaceAllString(message, "${1}[REDACTED]")
	message = labeledCredential.ReplaceAllString(message, "${1}[REDACTED]")
	message = urlCredential.ReplaceAllString(message, "${1}[REDACTED]@")
	message = queryCredential.ReplaceAllString(message, "${1}[REDACTED]")
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 1200 {
		message = message[:1200] + "…"
	}
	if message == "" {
		return "executor returned an empty error"
	}
	return message
}

func newDiagnosticID() string {
	var id [8]byte
	if _, err := rand.Read(id[:]); err == nil {
		return hex.EncodeToString(id[:])
	}
	return hex.EncodeToString([]byte(time.Now().UTC().Format("150405.000000000")))
}

// Cleanup commands are tracked for shutdown but do not consume generation admission.
func (m *taskManager) reserveControl() (*slotReservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.draining {
		return nil, ErrDraining
	}
	m.controlReserved++
	m.signalLocked()
	return &slotReservation{manager: m, control: true}, nil
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
		if len(m.active) == 0 && m.reserved == 0 && m.controlReserved == 0 {
			m.mu.Unlock()
			return
		}
		changed := m.changed
		m.mu.Unlock()
		<-changed
	}
}
