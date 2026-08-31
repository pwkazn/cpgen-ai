package docker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"cpgen/internal/domain"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/events"
	moby "github.com/moby/moby/client"
)

const compileDiagnosticMaxBytes int64 = 1 << 20

// Evidence is the normalized, host-produced process evidence used for the
// documented outcome priority. It never contains target-controlled claims.
type Evidence struct {
	CreateFailure    bool
	StartFailure     bool
	StartUnknown     bool
	Started          bool
	HardDeadline     bool
	Stopped          bool
	OOMKilled        bool
	EvidenceComplete bool
	OutputExceeded   bool
	Signal           string
	ExitCode         *int
}

func classifyProcess(evidence Evidence) (domain.ProcessOutcome, error) {
	switch {
	case evidence.CreateFailure || evidence.StartFailure || evidence.StartUnknown:
		return domain.ProcessInfraError, nil
	case evidence.Started && evidence.HardDeadline && evidence.Stopped:
		return domain.ProcessTLE, nil
	case evidence.OOMKilled:
		return domain.ProcessMLE, nil
	case !evidence.Started || !evidence.Stopped || !evidence.EvidenceComplete:
		return domain.ProcessInfraError, nil
	case evidence.OutputExceeded:
		return domain.ProcessOLE, nil
	case evidence.Signal != "":
		return domain.ProcessSignaled, nil
	case evidence.ExitCode != nil:
		return domain.ProcessExited, nil
	default:
		return "", fmt.Errorf("complete process evidence has neither signal nor exit code")
	}
}

func resolveProcess(evidence Evidence, cause *domain.ExecutionCause) (domain.ProcessOutcome, error) {
	if cause != nil {
		interrupted := domain.ExecutionInterrupted{Cause: *cause}
		if err := interrupted.Validate(); err != nil {
			return "", err
		}
		return "", interrupted
	}
	return classifyProcess(evidence)
}

type outputLimiter struct {
	mu       sync.Mutex
	writer   io.Writer
	limit    int64
	observed int64
	exceeded bool
	trigger  func()
}

func newOutputLimiter(writer io.Writer, limit int64, trigger func()) (*outputLimiter, error) {
	if writer == nil || limit <= 0 || trigger == nil {
		return nil, fmt.Errorf("output limiter requires a writer, positive limit, and trigger")
	}
	return &outputLimiter{writer: writer, limit: limit, trigger: trigger}, nil
}

func (l *outputLimiter) Write(data []byte) (int, error) {
	original := len(data)
	l.mu.Lock()
	remaining := l.limit - l.observed
	if remaining < 0 {
		remaining = 0
	}
	writable := int64(len(data))
	if writable > remaining {
		writable = remaining
	}
	if writable > 0 {
		count, err := l.writer.Write(data[:int(writable)])
		if err != nil {
			l.mu.Unlock()
			return count, err
		}
		if count != int(writable) {
			l.mu.Unlock()
			return count, io.ErrShortWrite
		}
	}
	if l.observed < l.limit+1 {
		addition := int64(len(data))
		if addition > l.limit+1-l.observed {
			addition = l.limit + 1 - l.observed
		}
		l.observed += addition
	}
	trigger := !l.exceeded && l.observed > l.limit
	if trigger {
		l.exceeded = true
	}
	l.mu.Unlock()
	if trigger {
		l.trigger()
	}
	return original, nil
}

func (l *outputLimiter) ObservedBytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.observed
}

func (l *outputLimiter) Exceeded() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.exceeded
}

type processExecution struct {
	Evidence Evidence
	Record   ExecutionRecord
	Outcome  domain.ProcessOutcome
}

type waitOutcome struct {
	status int64
	err    error
}

func (op *operation) executeTarget(ctx context.Context, target *ownedContainer, targetCall domain.AttemptCallID, hardLimit time.Duration, stdin *[]byte, stdout, stderr *outputLimiter) (processExecution, error) {
	if hardLimit <= 0 || stdout == nil || stderr == nil {
		return processExecution{}, fmt.Errorf("target execution requires positive time and output limits")
	}
	if err := ctx.Err(); err != nil {
		return processExecution{}, context.Cause(ctx)
	}
	if cause := executionCause(ctx); cause != nil {
		return processExecution{}, domain.ExecutionInterrupted{Cause: *cause}
	}
	attached, err := op.runner.engine.ContainerAttach(ctx, target.id, moby.ContainerAttachOptions{
		Stream: true, Stdin: stdin != nil, Stdout: true, Stderr: true,
	})
	if err != nil {
		return processExecution{}, fmt.Errorf("attach target before Start: %w", err)
	}
	defer attached.Close()

	eventsCtx, cancelEvents := context.WithCancel(context.Background())
	eventResult := op.runner.engine.Events(eventsCtx, moby.EventsListOptions{Filters: moby.Filters{}.
		Add("type", "container").Add("container", target.id).Add("event", string(events.ActionOOM), string(events.ActionDie))})
	monitor := newTargetEventMonitor(target.id)
	eventsDone := make(chan struct{})
	go func() {
		monitor.consume(eventsCtx, eventResult)
		close(eventsDone)
	}()
	defer func() {
		cancelEvents()
		<-eventsDone
	}()

	overflow := make(chan struct{}, 1)
	stdout.setTrigger(func() {
		select {
		case overflow <- struct{}{}:
		default:
		}
	})
	stderr.setTrigger(func() {
		select {
		case overflow <- struct{}{}:
		default:
		}
	})
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := stdcopy.StdCopy(stdout, stderr, attached.Reader)
		copyDone <- copyErr
	}()
	if err := op.watchdog.TargetPhase(ctx, target.resource, target.id, hardLimit+op.runner.limits.CleanupTimeout); err != nil {
		attached.Close()
		<-copyDone
		return processExecution{}, err
	}
	op.targetPhase = true
	if err := ctx.Err(); err != nil {
		attached.Close()
		<-copyDone
		return processExecution{}, context.Cause(ctx)
	}

	startBoundary := time.Now()
	hardTimer := time.NewTimer(hardLimit)
	defer hardTimer.Stop()
	startCtx, cancelStart := context.WithCancel(context.Background())
	defer cancelStart()
	startDone := make(chan error, 1)
	go func() {
		_, startErr := op.runner.engine.ContainerStart(startCtx, target.id, moby.ContainerStartOptions{})
		startDone <- startErr
	}()

	evidence := Evidence{}
	var triggerCause *domain.ExecutionCause
	contextTriggered := false
	select {
	case startErr := <-startDone:
		if startErr != nil {
			evidence.StartFailure = true
			return processExecution{}, fmt.Errorf("start target: %w", startErr)
		}
		evidence.Started = true
		target.running = true
	case <-hardTimer.C:
		evidence.HardDeadline = true
		evidence.StartUnknown = true
		cancelStart()
	case <-ctx.Done():
		triggerCause = executionCause(ctx)
		contextTriggered = true
		cancelStart()
	}

	stdinDone := make(chan error, 1)
	if stdin != nil && evidence.Started {
		go func() {
			written, writeErr := io.Copy(attached.Conn, bytes.NewReader(*stdin))
			if writeErr == nil && written != int64(len(*stdin)) {
				writeErr = io.ErrShortWrite
			}
			closeErr := attached.CloseWrite()
			stdinDone <- errors.Join(writeErr, closeErr)
		}()
	} else {
		stdinDone <- nil
	}

	waitDone := make(chan waitOutcome, 1)
	waitCtx, cancelWait := context.WithCancel(context.Background())
	defer cancelWait()
	go func() {
		status, waitErr := waitContainer(waitCtx, op.runner.engine, target.id)
		waitDone <- waitOutcome{status: status, err: waitErr}
	}()

	waited := waitOutcome{}
	waitObserved := false
	needsStop := evidence.HardDeadline || contextTriggered
	if evidence.Started {
		select {
		case waited = <-waitDone:
			waitObserved = waited.err == nil
		case <-hardTimer.C:
			evidence.HardDeadline = true
			needsStop = true
		case <-overflow:
			evidence.OutputExceeded = true
			needsStop = true
		case <-ctx.Done():
			triggerCause = executionCause(ctx)
			contextTriggered = true
			needsStop = true
		}
	}

	proof := StopProof{WaitObserved: waitObserved}
	if needsStop {
		cleanupCtx, cancelCleanup := context.WithTimeout(context.Background(), op.runner.limits.CleanupTimeout)
		proof, err = portableStop(cleanupCtx, op.runner.engine, target.id, func(inspected moby.ContainerInspectResult) error {
			return verifyContainerOwnership(inspected, target)
		})
		cancelCleanup()
		if err != nil {
			return processExecution{}, err
		}
		if !waitObserved {
			select {
			case waited = <-waitDone:
				waitObserved = waited.err == nil
			default:
			}
		}
	}
	target.running = false

	inspectCtx, cancelInspect := context.WithTimeout(context.Background(), op.runner.limits.CleanupTimeout)
	inspected, inspectErr := op.runner.engine.ContainerInspect(inspectCtx, target.id, moby.ContainerInspectOptions{})
	cancelInspect()
	inspectComplete := false
	if inspectErr == nil {
		if err := verifyContainerOwnership(inspected, target); err != nil {
			return processExecution{}, err
		}
		state := inspected.Container.State
		if state != nil && !state.Running && state.Pid == 0 {
			proof.InspectStopped = true
			inspectComplete = true
			evidence.OOMKilled = state.OOMKilled
			exit := state.ExitCode
			evidence.ExitCode = &exit
			if waitObserved && int64(exit) != waited.status {
				inspectComplete = false
			}
			if evidence.StartUnknown && state.StartedAt != "" {
				evidence.StartUnknown = false
				evidence.Started = true
			}
		}
	}
	cancelEvents()
	<-eventsDone
	evidence.OOMKilled = evidence.OOMKilled || monitor.oomKilled()
	evidence.OutputExceeded = evidence.OutputExceeded || stdout.Exceeded() || stderr.Exceeded()
	evidence.Stopped = proof.Validate() == nil
	evidence.EvidenceComplete = inspectComplete && monitor.err() == nil && (waitObserved || proof.WaitObserved)
	if evidence.StartUnknown && evidence.Started {
		evidence.StartUnknown = false
	}
	if evidence.Stopped {
		ackCtx, cancelAck := context.WithTimeout(context.Background(), op.runner.limits.CleanupTimeout)
		ackErr := op.watchdog.Stopped(ackCtx, target.resource, target.id)
		cancelAck()
		if ackErr != nil {
			return processExecution{}, fmt.Errorf("watchdog STOPPED acknowledgement: %w", ackErr)
		}
		op.targetStopped = true
	}

	copyErr := waitForAttachDrain(copyDone, attached, op.runner.limits.CleanupTimeout)
	stdinErr := <-stdinDone
	if copyErr != nil && !isClosedStreamError(copyErr) {
		evidence.EvidenceComplete = false
	}
	if stdinErr != nil && !isClosedStreamError(stdinErr) {
		evidence.EvidenceComplete = false
	}

	underlying, classifyErr := classifyProcess(evidence)
	if classifyErr != nil {
		return processExecution{}, classifyErr
	}
	if cause := executionCause(ctx); cause != nil {
		triggerCause = cause
	}
	signal := (*string)(nil)
	if evidence.Signal != "" {
		value := evidence.Signal
		signal = &value
	}
	record := ExecutionRecord{
		SchemaVersion: ExecutionRecordSchemaVersion, Protocol: ExecutionProtocolDockerDirectV2,
		Started: evidence.Started, Stopped: evidence.Stopped, EvidenceComplete: evidence.EvidenceComplete,
		Outcome: underlying, ExitCode: evidence.ExitCode, Signal: signal, WallTime: time.Since(startBoundary),
		MeasurementProfile: "mvp-v2", StdoutBytes: stdout.ObservedBytes(), StderrBytes: stderr.ObservedBytes(),
		StdoutTruncated: stdout.Exceeded(), StderrTruncated: stderr.Exceeded(), OOMKilled: evidence.OOMKilled,
		HardDeadline: evidence.HardDeadline, TriggerCause: triggerCause,
		EngineIdentityDigest: op.identity.EngineIdentityDigest, PlanDigest: op.plan.PlanDigest, TargetCallID: targetCall,
	}
	if err := record.Validate(); err != nil {
		return processExecution{}, err
	}
	execution := processExecution{Evidence: evidence, Record: record, Outcome: underlying}
	if triggerCause != nil {
		return execution, domain.ExecutionInterrupted{Cause: *triggerCause}
	}
	if cause := context.Cause(ctx); cause != nil {
		return execution, cause
	}
	return execution, nil
}

func (l *outputLimiter) setTrigger(trigger func()) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.trigger = trigger
}

func waitForAttachDrain(done <-chan error, attached moby.ContainerAttachResult, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		attached.Close()
		select {
		case err := <-done:
			return err
		case <-time.After(time.Second):
			return fmt.Errorf("target attach stream did not close after target stop")
		}
	}
}

func executionCause(ctx context.Context) *domain.ExecutionCause {
	if ctx == nil {
		return nil
	}
	var interrupted domain.ExecutionInterrupted
	if errors.As(context.Cause(ctx), &interrupted) && interrupted.Cause.Valid() {
		cause := interrupted.Cause
		return &cause
	}
	return nil
}

type targetEventMonitor struct {
	mu       sync.Mutex
	targetID string
	oom      bool
	failure  error
}

func newTargetEventMonitor(targetID string) *targetEventMonitor {
	return &targetEventMonitor{targetID: targetID}
}

func (m *targetEventMonitor) consume(ctx context.Context, result moby.EventsResult) {
	messages, failures := result.Messages, result.Err
	for messages != nil || failures != nil {
		select {
		case <-ctx.Done():
			for messages != nil {
				select {
				case message, ok := <-messages:
					if !ok {
						messages = nil
						continue
					}
					m.record(message)
				default:
					return
				}
			}
			return
		case message, ok := <-messages:
			if !ok {
				messages = nil
				continue
			}
			m.record(message)
		case err, ok := <-failures:
			if !ok {
				failures = nil
				continue
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				m.mu.Lock()
				m.failure = err
				m.mu.Unlock()
			}
		}
	}
	if ctx.Err() == nil {
		m.mu.Lock()
		m.failure = fmt.Errorf("target Engine event stream ended before execution reconciliation")
		m.mu.Unlock()
	}
}

func (m *targetEventMonitor) record(message events.Message) {
	if message.Actor.ID == m.targetID && message.Action == events.ActionOOM {
		m.mu.Lock()
		m.oom = true
		m.mu.Unlock()
	}
}

func (m *targetEventMonitor) oomKilled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.oom
}

func (m *targetEventMonitor) err() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failure
}
