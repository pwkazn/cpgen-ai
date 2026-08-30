//go:build cpgen_slice0_probe

package probe_test

import (
	"context"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/probe"
)

func TestProbeLedgerRejectsWrongOrderDuplicateAndForeignIdentity(t *testing.T) {
	plan := containerPlan(t)
	identity := probeIdentity(plan.PlanDigest)
	calls := []domain.AttemptCallID{
		"call_00000000000000000000000000000001",
		"call_00000000000000000000000000000002",
	}
	ledger, err := probe.NewContainerLedger(identity, plan, calls)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := port.NewSlice0ProbeAuthorization(identity, plan, ledger)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := auth.ClaimNextContainer(context.Background(), port.ContainerTarget); err == nil {
		t.Fatal("wrong-order claim succeeded")
	}
	importGrant, err := auth.ClaimNextContainer(context.Background(), port.ContainerImport)
	if err != nil {
		t.Fatal(err)
	}
	if importGrant.Role != port.ContainerImport || importGrant.Auth.CallID() != calls[0] {
		t.Fatalf("unexpected import grant: %#v", importGrant)
	}
	if _, err := auth.ClaimNextContainer(context.Background(), port.ContainerImport); err == nil {
		t.Fatal("duplicate claim succeeded")
	}
	targetGrant, err := auth.ClaimNextContainer(context.Background(), port.ContainerTarget)
	if err != nil {
		t.Fatal(err)
	}
	if targetGrant.Auth.CallID() != calls[1] {
		t.Fatalf("target call = %q, want %q", targetGrant.Auth.CallID(), calls[1])
	}
	if _, err := auth.ClaimNextContainer(context.Background(), port.ContainerTarget); err == nil {
		t.Fatal("extra claim succeeded")
	}

	snapshot := ledger.Snapshot()
	for index, claim := range snapshot.Containers {
		if claim.State != probe.ClaimDispatching {
			t.Fatalf("claim %d state = %q, want DISPATCHING", index, claim.State)
		}
	}
	foreign := identity
	foreign.PlanDigest = domain.SumBytes([]byte("foreign-plan"))
	if _, err := ledger.ClaimContainer(context.Background(), foreign, 0, port.ContainerImport); err == nil {
		t.Fatal("foreign plan identity was accepted")
	}
}

func TestProbeAuthorizationReturnsPlanCopies(t *testing.T) {
	plan := containerPlan(t)
	identity := probeIdentity(plan.PlanDigest)
	ledger, err := probe.NewContainerLedger(identity, plan, []domain.AttemptCallID{
		"call_00000000000000000000000000000001",
		"call_00000000000000000000000000000002",
	})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := port.NewSlice0ProbeAuthorization(identity, plan, ledger)
	if err != nil {
		t.Fatal(err)
	}

	got := auth.ContainerPlan()
	got.Resources[0].DeterministicName = "mutated"
	if auth.ContainerPlan().Resources[0].DeterministicName == "mutated" {
		t.Fatal("authorization leaked mutable plan state")
	}
	if auth.PlanDigest() != plan.PlanDigest {
		t.Fatalf("plan digest = %q, want %q", auth.PlanDigest(), plan.PlanDigest)
	}
}

func TestProbeAuthorizationAbortsOnlyUnconsumedClaims(t *testing.T) {
	plan := containerPlan(t)
	identity := probeIdentity(plan.PlanDigest)
	ledger, err := probe.NewContainerLedger(identity, plan, []domain.AttemptCallID{
		"call_00000000000000000000000000000001",
		"call_00000000000000000000000000000002",
	})
	if err != nil {
		t.Fatal(err)
	}
	auth, err := port.NewSlice0ProbeAuthorization(identity, plan, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.ClaimNextContainer(context.Background(), port.ContainerImport); err != nil {
		t.Fatal(err)
	}
	if err := auth.AbortRemaining(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot := ledger.Snapshot()
	if snapshot.Containers[0].State != probe.ClaimDispatching || snapshot.Containers[1].State != probe.ClaimAbortedNoDispatch {
		t.Fatalf("claim states = %#v", snapshot.Containers)
	}
	if _, err := auth.ClaimNextContainer(context.Background(), port.ContainerTarget); err == nil {
		t.Fatal("aborted claim was consumed")
	}
}

func TestEnginePingAndContainerClaimsAreMutuallyExclusive(t *testing.T) {
	pingPlan, err := port.NewContainerPlan(domain.SumBytes([]byte("engine")), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	identity := probeIdentity(pingPlan.PlanDigest)
	callID := domain.AttemptCallID("call_00000000000000000000000000000003")
	ledger, err := probe.NewEnginePingLedger(identity, pingPlan, callID)
	if err != nil {
		t.Fatal(err)
	}
	auth, err := port.NewSlice0ProbeAuthorization(identity, pingPlan, ledger)
	if err != nil {
		t.Fatal(err)
	}

	grant, err := auth.ClaimEnginePing(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if grant.CallID() != callID {
		t.Fatalf("ping call = %q, want %q", grant.CallID(), callID)
	}
	if _, err := auth.ClaimEnginePing(context.Background()); err == nil {
		t.Fatal("duplicate ping claim succeeded")
	}
	if _, err := auth.ClaimNextContainer(context.Background(), port.ContainerTarget); err == nil {
		t.Fatal("container claim on ping authorization succeeded")
	}

	containerPlan := containerPlan(t)
	identity.PlanDigest = containerPlan.PlanDigest
	containerLedger, err := probe.NewContainerLedger(identity, containerPlan, []domain.AttemptCallID{
		"call_00000000000000000000000000000004",
		"call_00000000000000000000000000000005",
	})
	if err != nil {
		t.Fatal(err)
	}
	containerAuth, err := port.NewSlice0ProbeAuthorization(identity, containerPlan, containerLedger)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := containerAuth.ClaimEnginePing(context.Background()); err == nil {
		t.Fatal("ping claim on container authorization succeeded")
	}
}

func probeIdentity(planDigest domain.Digest) port.ProbeAuthorizationIdentity {
	return port.ProbeAuthorizationIdentity{
		LogicalOperationID: "slice0-probe-operation",
		RunID:              "run_00000000000000000000000000000001",
		AttemptID:          "attempt_00000000000000000000000000000001",
		OwnerID:            "owner_00000000000000000000000000000001",
		LeaseEpoch:         7,
		ScopeDigest:        domain.SumBytes([]byte("scope")),
		PlanDigest:         planDigest,
	}
}

func containerPlan(t *testing.T) port.ContainerPlan {
	t.Helper()
	labels := domain.SumBytes([]byte("labels"))
	zero, one := 0, 1
	plan, err := port.NewContainerPlan(domain.SumBytes([]byte("engine")), []port.PlannedResource{
		{Ordinal: 0, Kind: port.ResourceContainer, Role: port.ResourceImport, DeterministicName: "import", ExpectedLabelsDigest: labels, CreateCallOrdinal: &zero},
		{Ordinal: 1, Kind: port.ResourceContainer, Role: port.ResourceTarget, DeterministicName: "target", ExpectedLabelsDigest: labels, CreateCallOrdinal: &one},
	}, 1024)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}
