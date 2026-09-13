package application_test

import (
	"context"
	"testing"
	"time"

	"cpgen/internal/application"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
)

type activeRuntime struct{ commands []domain.ActiveTimeCommand }

func (r *activeRuntime) AccountActiveTime(_ context.Context, command domain.ActiveTimeCommand) (domain.ActiveTimeResult, error) {
	r.commands = append(r.commands, command)
	return domain.ActiveTimeResult{RunID: command.RunID, RunVersion: command.ExpectedRunVersion + 1, Deadline: command.At.Add(time.Second), ActiveElapsed: time.Second, Remaining: time.Second, Active: command.Action != domain.ActiveTimeStop}, nil
}

func TestActiveTimeUsesAuthoritativeCommands(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	runtime := &activeRuntime{}
	account, err := application.NewActiveTime(runtime, clock.NewFake(now), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	runID := domain.RunID("run_0123456789abcdef0123456789abcdef")
	if _, err := account.Start(context.Background(), runID, 4); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Heartbeat(context.Background(), runID, 5); err != nil {
		t.Fatal(err)
	}
	if _, err := account.Stop(context.Background(), runID, 6); err != nil {
		t.Fatal(err)
	}
	if len(runtime.commands) != 3 || runtime.commands[0].Action != domain.ActiveTimeStart || runtime.commands[1].Action != domain.ActiveTimeHeartbeat || runtime.commands[2].Action != domain.ActiveTimeStop {
		t.Fatalf("commands = %+v", runtime.commands)
	}
}

func TestActiveTimeRecoveryChargesOneInterval(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	runtime := &activeRuntime{}
	account, err := application.NewActiveTime(runtime, clock.NewFake(now), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := account.Recover(context.Background(), domain.RunID("run_0123456789abcdef0123456789abcdef"), 8); err != nil {
		t.Fatal(err)
	}
	if runtime.commands[0].Action != domain.ActiveTimeRecover || runtime.commands[0].HeartbeatInterval != 2*time.Second {
		t.Fatalf("recovery command = %+v", runtime.commands[0])
	}
}
