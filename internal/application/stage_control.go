package application

import (
	"context"
	"errors"
	"time"

	"cpgen/internal/adapter/storage/sqlite"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

// stageControl owns active-time accounting and the two bounded pollers. The
// foreground owner holds run/artifact locks and joins the pollers before any
// terminal commit or Close. It does not invoke stages or own storage handles.
type stageControl struct {
	runtime interface {
		GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
		PendingCancel(context.Context, domain.RunID) (*domain.ControlRequest, error)
	}
	clock               clock.Clock
	active              *ActiveTime
	controlPollInterval time.Duration
}
type stageWatch struct {
	ctx                   context.Context
	cancel                context.CancelCauseFunc
	pollDone, accountDone chan struct{}
	exhausted             chan bool
	errors                chan error
}

func (s *stageControl) watch(ctx context.Context, runID domain.RunID) *stageWatch {
	stageCtx, cancel := context.WithCancelCause(ctx)
	w := &stageWatch{ctx: stageCtx, cancel: cancel, pollDone: make(chan struct{}), accountDone: make(chan struct{}), exhausted: make(chan bool, 1), errors: make(chan error, 1)}
	go s.cancelPoller(stageCtx, runID, func() { cancel(domain.ExecutionInterrupted{Cause: domain.CauseUserCancel}) }, w.pollDone)
	go s.accountingPoller(stageCtx, runID, func() { cancel(domain.ExecutionInterrupted{Cause: domain.CauseRunBudgetDeadline}) }, w.accountDone, w.exhausted, w.errors)
	return w
}
func (w *stageWatch) join() { w.cancel(nil); <-w.pollDone; <-w.accountDone }
func (s *stageControl) hasCancel(runID domain.RunID) bool {
	pending, err := s.runtime.PendingCancel(context.Background(), runID)
	return err == nil && pending != nil
}
func (s *stageControl) accountingPoller(ctx context.Context, runID domain.RunID, cancel func(), done chan<- struct{}, exhausted chan<- bool, errorsOut chan<- error) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(s.active.interval):
			snapshot, err := s.runtime.GetRun(context.Background(), runID)
			if err != nil || snapshot.ActiveStartedAt == nil {
				if err != nil {
					select {
					case errorsOut <- err:
					default:
					}
					cancel()
					return
				}
				continue
			}
			result, err := s.active.Heartbeat(context.Background(), runID, snapshot.Version)
			if err != nil {
				// Provider settlement or a second handle's control request may
				// advance the version after this read. Retry accounting on the
				// next bounded tick; this conflict is not a budget cancellation.
				if errors.Is(err, sqlite.ErrVersionConflict) {
					continue
				}
				select {
				case errorsOut <- err:
				default:
				}
				cancel()
				return
			}
			if result.Exhausted {
				select {
				case exhausted <- true:
				default:
				}
				cancel()
				return
			}
		}
	}
}

func (s *stageControl) cancelPoller(ctx context.Context, runID domain.RunID, cancel context.CancelFunc, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.clock.After(s.controlPollInterval):
			if s.hasCancel(runID) {
				cancel()
				return
			}
		}
	}
}
