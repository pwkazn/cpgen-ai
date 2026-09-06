package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

// ActiveTime is a foreground-only adapter around the authoritative SQLite
// accounting command. It has no ownership or authorization responsibilities.
type ActiveTime struct {
	runtime interface {
		AccountActiveTime(context.Context, domain.ActiveTimeCommand) (domain.ActiveTimeResult, error)
	}
	clock    clock.Clock
	interval time.Duration
}

func NewActiveTime(runtime interface {
	AccountActiveTime(context.Context, domain.ActiveTimeCommand) (domain.ActiveTimeResult, error)
}, source clock.Clock, interval time.Duration) (*ActiveTime, error) {
	if runtime == nil || source == nil || interval <= 0 {
		return nil, errors.New("active-time dependencies are required")
	}
	return &ActiveTime{runtime: runtime, clock: source, interval: interval}, nil
}

func (a *ActiveTime) Start(ctx context.Context, runID domain.RunID, expectedVersion int64) (domain.ActiveTimeResult, error) {
	return a.account(ctx, runID, expectedVersion, domain.ActiveTimeStart, 0, "start")
}
func (a *ActiveTime) Heartbeat(ctx context.Context, runID domain.RunID, expectedVersion int64) (domain.ActiveTimeResult, error) {
	return a.account(ctx, runID, expectedVersion, domain.ActiveTimeHeartbeat, 0, "heartbeat")
}
func (a *ActiveTime) Stop(ctx context.Context, runID domain.RunID, expectedVersion int64) (domain.ActiveTimeResult, error) {
	return a.accountAt(ctx, runID, expectedVersion, domain.ActiveTimeStop, 0, "stop", a.clock.Now().UTC())
}
func (a *ActiveTime) stopAt(ctx context.Context, runID domain.RunID, expectedVersion int64, at time.Time) (domain.ActiveTimeResult, error) {
	return a.accountAt(ctx, runID, expectedVersion, domain.ActiveTimeStop, 0, "stop", at.UTC())
}
func (a *ActiveTime) Recover(ctx context.Context, runID domain.RunID, expectedVersion int64) (domain.ActiveTimeResult, error) {
	return a.account(ctx, runID, expectedVersion, domain.ActiveTimeRecover, a.interval, "recover")
}

func (a *ActiveTime) account(ctx context.Context, runID domain.RunID, expectedVersion int64, action domain.ActiveTimeAction, interval time.Duration, label string) (domain.ActiveTimeResult, error) {
	return a.accountAt(ctx, runID, expectedVersion, action, interval, label, a.clock.Now().UTC())
}

func (a *ActiveTime) accountAt(ctx context.Context, runID domain.RunID, expectedVersion int64, action domain.ActiveTimeAction, interval time.Duration, label string, at time.Time) (domain.ActiveTimeResult, error) {
	if err := runID.Validate(); err != nil {
		return domain.ActiveTimeResult{}, err
	}
	if expectedVersion <= 0 {
		return domain.ActiveTimeResult{}, errors.New("active-time expected version must be positive")
	}
	command := domain.ActiveTimeCommand{RunID: runID, ExpectedRunVersion: expectedVersion, Action: action, HeartbeatInterval: interval, IdempotencyKey: stableActiveTimeID(label, runID, expectedVersion), At: at}
	result, err := a.runtime.AccountActiveTime(ctx, command)
	if err != nil {
		return domain.ActiveTimeResult{}, fmt.Errorf("account active time: %w", err)
	}
	if err := result.Validate(); err != nil {
		return domain.ActiveTimeResult{}, err
	}
	return result, nil
}

func stableActiveTimeID(label string, runID domain.RunID, version int64) string {
	digest := domain.SumBytes([]byte(fmt.Sprintf("cpgen.active-time/v1:%s:%s:%d", label, runID, version)))
	return label + "_" + string(digest[len("sha256:"):len("sha256:")+32])
}
