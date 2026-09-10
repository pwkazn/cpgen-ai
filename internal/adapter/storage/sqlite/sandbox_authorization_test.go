package sqlite

import (
	"context"
	"fmt"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestPreparedSandboxAuthorizationBindsRealReservedResourceCalls(t *testing.T) {
	ctx := context.Background()
	f, identity, plan, bindings := sandboxAuthorizationFixture(t)
	now := func() time.Time { return f.now }
	auth, err := port.NewPreparedSandboxAuthorization(ctx, identity, plan, bindings, f.store, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.ClaimEnginePing(ctx); err == nil {
		t.Fatal("compile capability allowed unplanned ping")
	}
	if _, err := auth.ClaimNextContainer(ctx, port.ContainerTarget); err == nil {
		t.Fatal("target bypassed import and keeper")
	}
	volumes := auth.(port.VolumeDispatchAuthorization)
	if _, err := volumes.ClaimNextVolume(ctx, port.ResourceOutput); err == nil {
		t.Fatal("output volume bypassed input")
	}
	copyPlan := auth.ContainerPlan()
	copyPlan.Resources[0].Role = port.ResourceOutput
	if auth.ContainerPlan().Resources[0].Role != port.ResourceInput {
		t.Fatal("authorization exposed mutable resource plan")
	}
	changed := identity
	changed.ScopeDigest = domain.SumBytes([]byte("substituted compilation request"))
	if _, err := port.NewPreparedSandboxAuthorization(ctx, changed, plan, bindings, f.store, now); err == nil {
		t.Fatal("foreign request reused prepared calls")
	}
	changed = identity
	changed.ExpectedRunVersion++
	originalDigest, _ := port.SandboxResourceRequestDigest(identity, plan, 0)
	newDigest, _ := port.SandboxResourceRequestDigest(changed, plan, 0)
	if originalDigest != newDigest {
		t.Fatal("heartbeat changed resource request identity")
	}
	grant, err := volumes.ClaimNextVolume(ctx, port.ResourceInput)
	if err != nil {
		t.Fatal(err)
	}
	if !grant.Durable || grant.Auth.CallID() != bindings[0].AttemptCallID || grant.CallRecordID != bindings[0].CallRecordID || grant.ExpectedRunVersion != 2 {
		t.Fatalf("unbound volume grant: %+v", grant)
	}
	if _, err := volumes.ClaimNextVolume(ctx, port.ResourceInput); err == nil {
		t.Fatal("same volume capability consumed twice")
	}
	// The Runner performs the actual BeginDispatch. Once this durable boundary
	// changes, reconstructing the facade cannot issue another create capability.
	dispatch := mustBeginDispatch(t, f, grant.CallRecordID, grant.Auth.CallID(), "sandbox volume")
	if _, err := port.NewPreparedSandboxAuthorization(ctx, identity, plan, bindings, f.store, now); err == nil {
		t.Fatal("DISPATCHING volume admitted by fresh authorization")
	}
	if err := f.store.MarkSent(ctx, dispatch, f.now); err != nil {
		t.Fatal(err)
	}
	if err := auth.AbortRemaining(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.AbortRemaining(ctx); err != nil {
		t.Fatalf("repeat abort: %v", err)
	}
	for index, binding := range bindings {
		p, err := f.store.LoadCall(ctx, binding.CallRecordID)
		if err != nil {
			t.Fatal(err)
		}
		want := domain.PhysicalAbortedNoDispatch
		if index == 0 {
			want = domain.PhysicalSent
		}
		if p.PhysicalCalls[0].State != want {
			t.Fatalf("resource %d state=%s want=%s", index, p.PhysicalCalls[0].State, want)
		}
	}
	if _, err := auth.ClaimNextContainer(ctx, port.ContainerImport); err == nil {
		t.Fatal("aborted authorization issued create capability")
	}
	var reserved, consumed int64
	if err := f.store.db.QueryRowContext(ctx, `SELECT reserved_value, consumed_value FROM budget_accounts WHERE run_id=? AND dimension='DOCKER_CONTAINER_CREATES'`, f.runID).Scan(&reserved, &consumed); err != nil {
		t.Fatal(err)
	}
	if reserved != 0 || consumed != 0 {
		t.Fatalf("undispatched containers were charged: %d/%d", reserved, consumed)
	}
}

func TestPreparedSandboxAuthorizationRechecksStateAtClaim(t *testing.T) {
	ctx := context.Background()
	f, identity, plan, bindings := sandboxAuthorizationFixture(t)
	auth, err := port.NewPreparedSandboxAuthorization(ctx, identity, plan, bindings, f.store, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	mustBeginDispatch(t, f, bindings[2].CallRecordID, bindings[2].AttemptCallID, "changed after construction")
	if _, err := auth.ClaimNextContainer(ctx, port.ContainerImport); err == nil {
		t.Fatal("claim ignored durable DISPATCHING transition")
	}
	bad := append([]port.SandboxResourceCall(nil), bindings...)
	bad[1] = bad[0]
	if _, err := port.NewPreparedSandboxAuthorization(ctx, identity, plan, bad, f.store, func() time.Time { return f.now }); err == nil {
		t.Fatal("duplicate resource binding accepted")
	}
}

func sandboxAuthorizationFixture(t *testing.T) (meteringFixture, port.SandboxAuthorizationIdentity, port.ContainerPlan, []port.SandboxResourceCall) {
	t.Helper()
	ctx := context.Background()
	f := newMeteringFixture(t, "f3", testCreateRunRequest(testRunID, testNow, time.Minute).BudgetLimits)
	identity := port.SandboxAuthorizationIdentity{RunID: f.runID, StageName: f.stage, AttemptID: f.attemptID, SandboxExecutionID: "sandbox_000000000000000000000000000000f3", LogicalOperationID: "solution-reference-compile", Kind: domain.CallSandboxCompile, ScopeDigest: domain.SumBytes([]byte("frozen compiler request")), ExpectedRunVersion: 2}
	resources := []port.PlannedResource{}
	for index, role := range []port.ResourceRole{port.ResourceInput, port.ResourceOutput, port.ResourceImport, port.ResourceKeeper, port.ResourceTarget, port.ResourceExport} {
		kind := port.ResourceVolume
		var ordinal *int
		if index >= 2 {
			kind = port.ResourceContainer
			v := index - 2
			ordinal = &v
		}
		resources = append(resources, port.PlannedResource{Ordinal: index, Kind: kind, Role: role, DeterministicName: fmt.Sprintf("cpgen-auth-%d", index), ExpectedLabelsDigest: domain.SumBytes([]byte(fmt.Sprintf("labels-%d", index))), CreateCallOrdinal: ordinal})
	}
	plan, err := port.NewContainerPlan(domain.SumBytes([]byte("engine")), resources, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	bindings := make([]port.SandboxResourceCall, len(resources))
	for index, resource := range resources {
		digest, err := port.SandboxResourceRequestDigest(identity, plan, index)
		if err != nil {
			t.Fatal(err)
		}
		open := openCallRequest(f, index+1, identity.Kind)
		open.Provider, open.RequestDigest, open.PolicyDigest = "docker", digest, identity.ScopeDigest
		open.LogicalOperationID, open.RetryPolicy.MaxAttempts = port.SandboxResourceLogicalOperation(identity, index), 1
		record, err := f.store.OpenCall(ctx, open)
		if err != nil {
			t.Fatal(err)
		}
		kind := domain.PhysicalDockerContainerCreate
		if resource.Kind == port.ResourceVolume {
			kind = domain.PhysicalDockerVolumeCreate
		}
		request := prepareOneRequest(f, record, index+1, kind, domain.BudgetDockerContainerCreates, 1)
		request.PlanDigest = plan.PlanDigest
		request.Calls[0].Provider, request.Calls[0].RequestDigest = "docker", digest
		prepared, err := f.store.PrepareCalls(ctx, request)
		if err != nil {
			t.Fatal(err)
		}
		bindings[index] = port.SandboxResourceCall{ResourceOrdinal: index, CallRecordID: record.ID, AttemptCallID: prepared.PhysicalCalls[0].ID}
	}
	return f, identity, plan, bindings
}
