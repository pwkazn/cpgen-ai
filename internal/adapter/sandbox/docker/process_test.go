package docker

import (
	"bytes"
	"errors"
	"testing"

	"cpgen/internal/domain"
)

func TestProcessOutcomePriority(t *testing.T) {
	zero := 0
	tests := []struct {
		name     string
		evidence Evidence
		want     domain.ProcessOutcome
	}{
		{"create failure", Evidence{CreateFailure: true}, domain.ProcessInfraError},
		{"hard deadline", Evidence{Started: true, HardDeadline: true, Stopped: true}, domain.ProcessTLE},
		{"oom beats ole", Evidence{Started: true, OOMKilled: true, OutputExceeded: true}, domain.ProcessMLE},
		{"missing runtime evidence", Evidence{Started: true, EvidenceComplete: false}, domain.ProcessInfraError},
		{"ole", Evidence{Started: true, Stopped: true, EvidenceComplete: true, OutputExceeded: true}, domain.ProcessOLE},
		{"signal", Evidence{Started: true, Stopped: true, EvidenceComplete: true, Signal: "SIGSEGV"}, domain.ProcessSignaled},
		{"exit", Evidence{Started: true, Stopped: true, EvidenceComplete: true, ExitCode: &zero}, domain.ProcessExited},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := classifyProcess(test.evidence)
			if err != nil || got != test.want {
				t.Fatalf("classifyProcess() = %q, %v; want %q", got, err, test.want)
			}
		})
	}
}

func TestExecutionCauseRaceAlwaysReturnsInterrupted(t *testing.T) {
	zero := 0
	evidenceCases := []Evidence{
		{Started: true, HardDeadline: true, Stopped: true},
		{Started: true, OOMKilled: true, Stopped: true},
		{Started: true, EvidenceComplete: true, Stopped: true, ExitCode: &zero},
	}
	causes := []domain.ExecutionCause{
		domain.CauseUserCancel, domain.CauseRevisionInvalidated, domain.CauseStepDeadline,
		domain.CauseRunBudgetDeadline,
	}
	for _, cause := range causes {
		for _, evidence := range evidenceCases {
			if _, err := resolveProcess(evidence, &cause); err == nil {
				t.Fatalf("cause %q returned a process outcome", cause)
			} else {
				var interrupted domain.ExecutionInterrupted
				if !errors.As(err, &interrupted) || interrupted.Cause != cause {
					t.Fatalf("cause %q returned %T %v", cause, err, err)
				}
			}
		}
	}
}

func TestOutputLimiterCountsLimitPlusOneAndContinuesDiscarding(t *testing.T) {
	var output bytes.Buffer
	triggers := 0
	limiter, err := newOutputLimiter(&output, 4, func() { triggers++ })
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{[]byte("abc"), []byte("def"), []byte("more")} {
		if count, err := limiter.Write(data); err != nil || count != len(data) {
			t.Fatalf("Write() = %d, %v", count, err)
		}
	}
	if output.String() != "abcd" || limiter.ObservedBytes() != 5 || !limiter.Exceeded() || triggers != 1 {
		t.Fatalf("output=%q observed=%d exceeded=%v triggers=%d", output.String(), limiter.ObservedBytes(), limiter.Exceeded(), triggers)
	}
}
