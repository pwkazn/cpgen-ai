//go:build cpgen_slice0_probe

package probe

import (
	"context"
	"fmt"
	"sync"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

type ClaimState string

const (
	ClaimAuthorized  ClaimState = "AUTHORIZED"
	ClaimDispatching ClaimState = "DISPATCHING"
)

type ClaimSnapshot struct {
	Ordinal int
	Role    port.ContainerRole
	CallID  domain.AttemptCallID
	State   ClaimState
}

type LedgerSnapshot struct {
	EnginePing *ClaimSnapshot
	Containers []ClaimSnapshot
}

type Ledger struct {
	mu          sync.Mutex
	identity    port.ProbeAuthorizationIdentity
	planDigest  domain.Digest
	enginePing  *ClaimSnapshot
	containers  []ClaimSnapshot
	nextOrdinal int
}

func NewContainerLedger(identity port.ProbeAuthorizationIdentity, plan port.ContainerPlan, callIDs []domain.AttemptCallID) (*Ledger, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if identity.PlanDigest != plan.PlanDigest {
		return nil, fmt.Errorf("probe identity plan digest mismatch")
	}
	if len(plan.Resources) == 0 {
		return nil, fmt.Errorf("container ledger requires a non-empty plan")
	}

	containers := make([]ClaimSnapshot, 0, len(callIDs))
	for _, resource := range plan.Resources {
		if resource.Kind != port.ResourceContainer {
			continue
		}
		role, err := containerRole(resource.Role)
		if err != nil {
			return nil, err
		}
		containers = append(containers, ClaimSnapshot{Ordinal: *resource.CreateCallOrdinal, Role: role, State: ClaimAuthorized})
	}
	if len(callIDs) != len(containers) {
		return nil, fmt.Errorf("got %d call IDs for %d planned containers", len(callIDs), len(containers))
	}
	seen := make(map[domain.AttemptCallID]struct{}, len(callIDs))
	for index, callID := range callIDs {
		if err := callID.Validate(); err != nil {
			return nil, fmt.Errorf("container call %d: %w", index, err)
		}
		if _, exists := seen[callID]; exists {
			return nil, fmt.Errorf("duplicate container call ID %q", callID)
		}
		seen[callID] = struct{}{}
		containers[index].CallID = callID
	}
	return &Ledger{identity: identity, planDigest: plan.PlanDigest, containers: containers}, nil
}

func NewEnginePingLedger(identity port.ProbeAuthorizationIdentity, plan port.ContainerPlan, callID domain.AttemptCallID) (*Ledger, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if identity.PlanDigest != plan.PlanDigest {
		return nil, fmt.Errorf("probe identity plan digest mismatch")
	}
	if len(plan.Resources) != 0 {
		return nil, fmt.Errorf("Engine-ping ledger requires an empty plan")
	}
	if err := callID.Validate(); err != nil {
		return nil, err
	}
	return &Ledger{
		identity:   identity,
		planDigest: plan.PlanDigest,
		enginePing: &ClaimSnapshot{Ordinal: -1, CallID: callID, State: ClaimAuthorized},
	}, nil
}

func (l *Ledger) ClaimEnginePing(ctx context.Context, identity port.ProbeAuthorizationIdentity) (domain.AttemptCallID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkIdentity(identity); err != nil {
		return "", err
	}
	if l.enginePing == nil || len(l.containers) != 0 {
		return "", fmt.Errorf("ledger does not authorize an Engine ping")
	}
	if l.enginePing.State != ClaimAuthorized {
		return "", fmt.Errorf("Engine ping claim is %s, not AUTHORIZED", l.enginePing.State)
	}
	l.enginePing.State = ClaimDispatching
	return l.enginePing.CallID, nil
}

func (l *Ledger) ClaimContainer(ctx context.Context, identity port.ProbeAuthorizationIdentity, ordinal int, role port.ContainerRole) (domain.AttemptCallID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.checkIdentity(identity); err != nil {
		return "", err
	}
	if l.enginePing != nil {
		return "", fmt.Errorf("Engine-ping ledger cannot authorize a container")
	}
	if l.nextOrdinal >= len(l.containers) {
		return "", fmt.Errorf("all container claims are already consumed")
	}
	claim := &l.containers[l.nextOrdinal]
	if ordinal != claim.Ordinal || role != claim.Role {
		return "", fmt.Errorf("next claim is ordinal %d role %q, got ordinal %d role %q", claim.Ordinal, claim.Role, ordinal, role)
	}
	if claim.State != ClaimAuthorized {
		return "", fmt.Errorf("container claim %d is %s, not AUTHORIZED", ordinal, claim.State)
	}
	claim.State = ClaimDispatching
	l.nextOrdinal++
	return claim.CallID, nil
}

func (l *Ledger) Snapshot() LedgerSnapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	snapshot := LedgerSnapshot{Containers: append([]ClaimSnapshot(nil), l.containers...)}
	if l.enginePing != nil {
		ping := *l.enginePing
		snapshot.EnginePing = &ping
	}
	return snapshot
}

func (l *Ledger) checkIdentity(got port.ProbeAuthorizationIdentity) error {
	if got != l.identity {
		return fmt.Errorf("probe authorization identity mismatch")
	}
	return nil
}

func containerRole(role port.ResourceRole) (port.ContainerRole, error) {
	switch role {
	case port.ResourceImport:
		return port.ContainerImport, nil
	case port.ResourceKeeper:
		return port.ContainerKeeper, nil
	case port.ResourceTarget:
		return port.ContainerTarget, nil
	case port.ResourceExport:
		return port.ContainerExport, nil
	default:
		return "", fmt.Errorf("resource role %q is not a container role", role)
	}
}
