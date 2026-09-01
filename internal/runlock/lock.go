// Package runlock provides permanent private advisory lock files for local
// process coordination. File handles, rather than persisted ownership records,
// are the sole source of execution ownership.
package runlock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"cpgen/internal/domain"
)

type Mode uint8

const (
	Shared Mode = iota + 1
	Exclusive
)

type Options struct{ PollInterval time.Duration }

var ErrBusy = errors.New("process lock is busy")

type Scope uint8

const (
	ScopeRun Scope = iota + 1
	ScopeArtifacts
)

type Manager struct {
	root         *os.Root
	pollInterval time.Duration
	closed       atomic.Bool
}

type Guard struct {
	scope Scope
	runID domain.RunID
	file  *os.File
	held  atomic.Bool
}

func NewManager(root string, options Options) (*Manager, error) {
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("lock root must be absolute: %q", root)
	}
	if options.PollInterval <= 0 {
		return nil, errors.New("lock poll interval must be positive")
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("stat lock root: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("lock root is not a directory: %q", root)
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open lock root: %w", err)
	}
	return &Manager{root: opened, pollInterval: options.PollInterval}, nil
}

func (m *Manager) AcquireRun(ctx context.Context, runID domain.RunID, mode Mode) (*Guard, error) {
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	return m.acquire(ctx, ScopeRun, runID, mode)
}

func (m *Manager) AcquireArtifacts(ctx context.Context, mode Mode) (*Guard, error) {
	return m.acquire(ctx, ScopeArtifacts, "", mode)
}

func (m *Manager) TryAcquireRun(runID domain.RunID, mode Mode) (*Guard, error) {
	if err := runID.Validate(); err != nil {
		return nil, err
	}
	return m.tryAcquire(ScopeRun, runID, mode)
}

func (m *Manager) TryAcquireArtifacts(mode Mode) (*Guard, error) {
	return m.tryAcquire(ScopeArtifacts, "", mode)
}

func (m *Manager) acquire(ctx context.Context, scope Scope, runID domain.RunID, mode Mode) (*Guard, error) {
	if ctx == nil {
		return nil, errors.New("lock context is nil")
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		guard, err := m.tryAcquire(scope, runID, mode)
		if !errors.Is(err, ErrBusy) {
			return guard, err
		}
		timer := time.NewTimer(m.pollInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (m *Manager) tryAcquire(scope Scope, runID domain.RunID, mode Mode) (*Guard, error) {
	if m == nil || m.root == nil || m.closed.Load() {
		return nil, errors.New("lock manager is closed")
	}
	if mode != Shared && mode != Exclusive {
		return nil, fmt.Errorf("invalid lock mode %d", mode)
	}
	name := "artifacts.lock"
	if scope == ScopeRun {
		name = string(runID) + ".lock"
	} else if scope != ScopeArtifacts {
		return nil, fmt.Errorf("invalid lock scope %d", scope)
	}
	file, err := m.root.OpenFile(name, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	if err := validateLockFile(m.root, name, file); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := lockFile(file, mode); err != nil {
		_ = file.Close()
		return nil, err
	}
	guard := &Guard{scope: scope, runID: runID, file: file}
	guard.held.Store(true)
	return guard, nil
}

func validateLockFile(root *os.Root, name string, file *os.File) error {
	info, err := root.Lstat(name)
	if err != nil {
		return fmt.Errorf("lstat lock file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("lock file is not regular: %s", name)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("lock file is a symlink: %s", name)
	}
	if err := validateSingleLink(file); err != nil {
		return err
	}
	return nil
}

func (m *Manager) Close() error {
	if m == nil || !m.closed.CompareAndSwap(false, true) {
		return nil
	}
	if m.root == nil {
		return nil
	}
	return m.root.Close()
}

func (g *Guard) Scope() Scope {
	if g == nil {
		return 0
	}
	return g.scope
}
func (g *Guard) RunID() (domain.RunID, bool) {
	if g == nil || g.scope != ScopeRun {
		return "", false
	}
	return g.runID, true
}
func (g *Guard) Held() bool { return g != nil && g.held.Load() }
func (g *Guard) Close() error {
	if g == nil || g.file == nil {
		return errors.New("zero lock guard")
	}
	if !g.held.CompareAndSwap(true, false) {
		return nil
	}
	unlockErr := unlockFile(g.file)
	closeErr := g.file.Close()
	return errors.Join(unlockErr, closeErr)
}
