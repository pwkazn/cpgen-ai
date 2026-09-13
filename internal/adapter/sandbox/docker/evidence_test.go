package docker

import (
	"testing"
	"time"

	"cpgen/internal/domain"
)

func TestExecutionRecordValidatesIdentityAndEvidenceConsistency(t *testing.T) {
	exit := 0
	record := ExecutionRecord{
		SchemaVersion: "cpgen.execution-record/v1", Protocol: ExecutionProtocolDockerDirectV2,
		Started: true, Stopped: true, EvidenceComplete: true, Outcome: domain.ProcessExited, ExitCode: &exit,
		WallTime: time.Millisecond, MeasurementProfile: "mvp-v2", StdoutBytes: 4, StderrBytes: 2,
		EngineIdentityDigest: domain.SumBytes([]byte("engine")), PlanDigest: domain.SumBytes([]byte("plan")),
		TargetCallID: "call_00000000000000000000000000000001",
	}
	if err := record.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ExecutionRecord){
		"wrong protocol": func(item *ExecutionRecord) { item.Protocol = "shell" },
		"unstopped":      func(item *ExecutionRecord) { item.Stopped = false },
		"negative bytes": func(item *ExecutionRecord) { item.StdoutBytes = -1 },
		"bad identity":   func(item *ExecutionRecord) { item.PlanDigest = "mutable" },
		"exit mismatch":  func(item *ExecutionRecord) { item.Outcome = domain.ProcessSignaled },
	} {
		t.Run(name, func(t *testing.T) {
			bad := record
			mutate(&bad)
			if err := bad.Validate(); err == nil {
				t.Fatalf("invalid record was accepted: %#v", bad)
			}
		})
	}
}
