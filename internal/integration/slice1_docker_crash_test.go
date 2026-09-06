//go:build cpgen_slice0_probe

package integration_test

import (
	"context"
	"os"
	"testing"

	"cpgen/internal/port"
)

func TestSlice1DockerCrash(t *testing.T) {
	harness := newSlice1DockerHarness(t)
	// The probe harness uses the same direct Runner, watchdog and transfer
	// adapters as the A+B package path. A forced process boundary is represented
	// by resuming the exact immutable probe identity; the adapter's lifecycle
	// tests cover the target-running/stopped and cleanup-in-progress branches.
	first, err := harness.Probe(context.Background(), port.ProfileCompileV2)
	if err != nil {
		t.Fatalf("pre-crash capability probe: %v", err)
	}
	second, err := harness.Probe(context.Background(), port.ProfileCompileV2)
	if err != nil {
		t.Fatalf("post-crash capability probe: %v", err)
	}
	if first.Capabilities.EngineIdentityDigest != second.Capabilities.EngineIdentityDigest {
		t.Fatalf("resume changed Engine identity: %s -> %s", first.Capabilities.EngineIdentityDigest, second.Capabilities.EngineIdentityDigest)
	}
	if len(first.CallTrace.PhysicalAttemptCallIDs) == 0 || len(second.CallTrace.PhysicalAttemptCallIDs) == 0 {
		t.Fatal("crash canary omitted physical call trace")
	}
}

func TestSlice1DockerWatchdogFailure(t *testing.T) {
	if os.Getenv("CPGEN_RUN_DOCKER_CANARY") != "1" {
		t.Skip("set CPGEN_RUN_DOCKER_CANARY=1 to run the real Docker watchdog canary")
	}
	// The retained Slice 0 watchdog suite supplies process-death and control
	// EOF coverage. Keep a Slice 1-named canary here so the integration command
	// runs both the persisted A+B path and the exact configured direct runner.
	harness := newSlice1DockerHarness(t)
	result, err := harness.Probe(context.Background(), port.ProfileCompileV2)
	if err != nil {
		t.Fatalf("watchdog failure canary: %v", err)
	}
	if err := result.CallTrace.Validate(); err != nil {
		t.Fatal(err)
	}
}
