package fake

import (
	"context"
	"fmt"
	"sync"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type Sandbox struct {
	mu              sync.Mutex
	compileOutcomes []domain.MeteredOutcome[port.CompileResult]
	runOutcomes     []domain.MeteredOutcome[port.RunResult]
	compileRequests []port.CompileRequest
	runRequests     []port.RunRequest
}

func NewSandbox(compile []domain.MeteredOutcome[port.CompileResult], run []domain.MeteredOutcome[port.RunResult]) *Sandbox {
	return &Sandbox{
		compileOutcomes: append([]domain.MeteredOutcome[port.CompileResult](nil), compile...),
		runOutcomes:     append([]domain.MeteredOutcome[port.RunResult](nil), run...),
	}
}

func (f *Sandbox) Compile(ctx context.Context, request port.CompileRequest) (domain.MeteredOutcome[port.CompileResult], error) {
	if err := ctx.Err(); err != nil {
		return domain.MeteredOutcome[port.CompileResult]{}, err
	}
	if err := request.Validate(); err != nil {
		return domain.MeteredOutcome[port.CompileResult]{}, fmt.Errorf("fake compile request: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.compileRequests = append(f.compileRequests, request)
	if len(f.compileOutcomes) == 0 {
		return domain.MeteredOutcome[port.CompileResult]{}, fmt.Errorf("fake sandbox has no queued compile outcome")
	}
	outcome := f.compileOutcomes[0]
	f.compileOutcomes = f.compileOutcomes[1:]
	if err := outcome.Validate(); err != nil {
		return domain.MeteredOutcome[port.CompileResult]{}, fmt.Errorf("invalid fake compile outcome: %w", err)
	}
	if outcome.Value != nil {
		if err := outcome.Value.Validate(); err != nil {
			return domain.MeteredOutcome[port.CompileResult]{}, fmt.Errorf("invalid fake compile result: %w", err)
		}
		if !outcome.Value.CallTrace.Equal(outcome.CallTrace) {
			return domain.MeteredOutcome[port.CompileResult]{}, fmt.Errorf("fake compile value and wrapper call traces differ")
		}
	}
	return outcome, nil
}

func (f *Sandbox) Run(ctx context.Context, request port.RunRequest) (domain.MeteredOutcome[port.RunResult], error) {
	if err := ctx.Err(); err != nil {
		return domain.MeteredOutcome[port.RunResult]{}, err
	}
	if err := request.Validate(); err != nil {
		return domain.MeteredOutcome[port.RunResult]{}, fmt.Errorf("fake run request: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runRequests = append(f.runRequests, request)
	if len(f.runOutcomes) == 0 {
		return domain.MeteredOutcome[port.RunResult]{}, fmt.Errorf("fake sandbox has no queued run outcome")
	}
	outcome := f.runOutcomes[0]
	f.runOutcomes = f.runOutcomes[1:]
	if err := outcome.Validate(); err != nil {
		return domain.MeteredOutcome[port.RunResult]{}, fmt.Errorf("invalid fake run outcome: %w", err)
	}
	if outcome.Value != nil {
		if err := outcome.Value.Validate(); err != nil {
			return domain.MeteredOutcome[port.RunResult]{}, fmt.Errorf("invalid fake run result: %w", err)
		}
		if !outcome.Value.CallTrace.Equal(outcome.CallTrace) {
			return domain.MeteredOutcome[port.RunResult]{}, fmt.Errorf("fake run value and wrapper call traces differ")
		}
	}
	return outcome, nil
}

func (f *Sandbox) CompileRequests() []port.CompileRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]port.CompileRequest(nil), f.compileRequests...)
}

func (f *Sandbox) RunRequests() []port.RunRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]port.RunRequest(nil), f.runRequests...)
}

var _ port.MeteredSandbox = (*Sandbox)(nil)
