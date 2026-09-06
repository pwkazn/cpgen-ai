//go:build cpgen_slice0_probe

package port

import (
	"context"
	"fmt"
	"sync"

	"cpgen/internal/domain"
)

type probeSandboxDispatchAuthorization struct {
	mu                sync.Mutex
	identity          ProbeAuthorizationIdentity
	plan              ContainerPlan
	claims            ProbeClaimStore
	nextResourceIndex int
	pingClaimed       bool
	aborted           bool
}

func NewSlice0ProbeAuthorization(identity ProbeAuthorizationIdentity, plan ContainerPlan, claims ProbeClaimStore) (SandboxDispatchAuthorization, error) {
	if err := identity.Validate(); err != nil {
		return nil, fmt.Errorf("probe authorization identity: %w", err)
	}
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("probe authorization plan: %w", err)
	}
	if identity.PlanDigest != plan.PlanDigest {
		return nil, fmt.Errorf("probe authorization plan digest mismatch")
	}
	if claims == nil {
		return nil, fmt.Errorf("probe claim store is required")
	}
	return &probeSandboxDispatchAuthorization{identity: identity, plan: plan.Clone(), claims: claims}, nil
}

func (a *probeSandboxDispatchAuthorization) LogicalOperationID() string {
	return a.identity.LogicalOperationID
}
func (a *probeSandboxDispatchAuthorization) RunID() domain.RunID         { return a.identity.RunID }
func (a *probeSandboxDispatchAuthorization) AttemptID() domain.AttemptID { return a.identity.AttemptID }
func (a *probeSandboxDispatchAuthorization) SandboxExecutionID() domain.SandboxExecutionID {
	return a.identity.SandboxExecutionID
}
func (a *probeSandboxDispatchAuthorization) ScopeDigest() domain.Digest {
	return a.identity.ScopeDigest
}
func (a *probeSandboxDispatchAuthorization) PlanDigest() domain.Digest { return a.plan.PlanDigest }
func (a *probeSandboxDispatchAuthorization) ContainerPlan() ContainerPlan {
	return a.plan.Clone()
}
func (a *probeSandboxDispatchAuthorization) sealSandboxDispatchAuthorization() {}

func (a *probeSandboxDispatchAuthorization) ClaimEnginePing(ctx context.Context) (DispatchAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.aborted {
		return nil, fmt.Errorf("probe authorization was aborted")
	}
	if len(a.plan.Resources) != 0 {
		return nil, fmt.Errorf("Engine ping claim is not allowed by a container plan")
	}
	if a.pingClaimed {
		return nil, fmt.Errorf("Engine ping claim was already consumed")
	}
	callID, err := a.claims.ClaimEnginePing(ctx, a.identity)
	if err != nil {
		return nil, err
	}
	a.pingClaimed = true
	return probeDispatchGrant{callID: callID, identity: a.identity}, nil
}

func (a *probeSandboxDispatchAuthorization) ClaimNextContainer(ctx context.Context, role ContainerRole) (ContainerDispatchGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.aborted {
		return ContainerDispatchGrant{}, fmt.Errorf("probe authorization was aborted")
	}
	if len(a.plan.Resources) == 0 {
		return ContainerDispatchGrant{}, fmt.Errorf("container claim is not allowed by an Engine-ping plan")
	}
	for a.nextResourceIndex < len(a.plan.Resources) && a.plan.Resources[a.nextResourceIndex].Kind != ResourceContainer {
		a.nextResourceIndex++
	}
	if a.nextResourceIndex >= len(a.plan.Resources) {
		return ContainerDispatchGrant{}, fmt.Errorf("container plan has no unclaimed ContainerCreate")
	}
	resource := a.plan.Resources[a.nextResourceIndex]
	wantRole, err := resource.Role.containerRole()
	if err != nil {
		return ContainerDispatchGrant{}, err
	}
	if !role.Valid() || role != wantRole {
		return ContainerDispatchGrant{}, fmt.Errorf("next container role is %q, got %q", wantRole, role)
	}
	callID, err := a.claims.ClaimContainer(ctx, a.identity, *resource.CreateCallOrdinal, role)
	if err != nil {
		return ContainerDispatchGrant{}, err
	}
	a.nextResourceIndex++
	return ContainerDispatchGrant{
		Role:         role,
		Auth:         probeDispatchGrant{callID: callID, identity: a.identity},
		CallRecordID: callRecordForAttempt(callID), ExpectedRunVersion: 1,
	}, nil
}

func (a *probeSandboxDispatchAuthorization) AbortRemaining(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.aborted {
		return nil
	}
	if err := a.claims.AbortRemaining(ctx, a.identity); err != nil {
		return err
	}
	a.aborted = true
	return nil
}

type probeDispatchGrant struct {
	callID   domain.AttemptCallID
	identity ProbeAuthorizationIdentity
}

func (g probeDispatchGrant) CallID() domain.AttemptCallID { return g.callID }
func (g probeDispatchGrant) RunID() domain.RunID          { return g.identity.RunID }
func (g probeDispatchGrant) AttemptID() domain.AttemptID  { return g.identity.AttemptID }
func (g probeDispatchGrant) SandboxExecutionID() domain.SandboxExecutionID {
	return g.identity.SandboxExecutionID
}
func (g probeDispatchGrant) ScopeDigest() domain.Digest { return g.identity.ScopeDigest }
func (g probeDispatchGrant) sealDispatchAuthorization() {}

func callRecordForAttempt(id domain.AttemptCallID) domain.CallRecordID {
	raw := string(id)
	if len(raw) > len("call_") && raw[:len("call_")] == "call_" {
		raw = raw[len("call_"):]
	}
	return domain.CallRecordID("callrec_" + raw)
}
