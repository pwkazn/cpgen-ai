package port

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"cpgen/internal/domain"
)

// SandboxAuthorizationIdentity binds a single compile/run to its current
// stage attempt. ScopeDigest covers the complete request and frozen policy.
type SandboxAuthorizationIdentity struct {
	RunID              domain.RunID
	StageName          domain.StageName
	AttemptID          domain.AttemptID
	SandboxExecutionID domain.SandboxExecutionID
	LogicalOperationID string
	Kind               domain.CallKind
	ScopeDigest        domain.Digest
	ExpectedRunVersion int64
}

func (i SandboxAuthorizationIdentity) Validate() error {
	for _, err := range []error{i.RunID.Validate(), i.StageName.Validate(), i.AttemptID.Validate(), i.SandboxExecutionID.Validate(), i.ScopeDigest.Validate()} {
		if err != nil {
			return err
		}
	}
	if i.ExpectedRunVersion <= 0 || i.LogicalOperationID == "" || len(i.LogicalOperationID) > 200 || (i.Kind != domain.CallSandboxCompile && i.Kind != domain.CallSandboxRun) {
		return errors.New("sandbox authorization requires a bounded compile/run identity")
	}
	return nil
}

// Each resource has one logical call with one physical key. Resource creates
// are distinct operations, not transport retries of the previous resource.
type SandboxResourceCall struct {
	ResourceOrdinal int
	CallRecordID    domain.CallRecordID
	AttemptCallID   domain.AttemptCallID
}

func SandboxResourceLogicalOperation(identity SandboxAuthorizationIdentity, ordinal int) string {
	return fmt.Sprintf("%s:resource:%d", identity.LogicalOperationID, ordinal)
}

func SandboxResourceRequestDigest(identity SandboxAuthorizationIdentity, plan ContainerPlan, ordinal int) (domain.Digest, error) {
	if err := identity.Validate(); err != nil {
		return "", err
	}
	if err := plan.Validate(); err != nil {
		return "", err
	}
	if ordinal < 0 || ordinal >= len(plan.Resources) || plan.Resources[ordinal].Kind == ResourceCgroup {
		return "", errors.New("sandbox call must identify a planned container or volume")
	}
	// Heartbeat versions are dispatch preconditions, not operation identity.
	identity.ExpectedRunVersion = 0
	raw, err := json.Marshal(struct {
		Schema     string
		Identity   SandboxAuthorizationIdentity
		PlanDigest domain.Digest
		Resource   PlannedResource
	}{"cpgen.sandbox-resource-call/v1", identity, plan.PlanDigest, plan.Resources[ordinal]})
	if err != nil {
		return "", err
	}
	return domain.SumBytes(raw), nil
}

type preparedSandboxAuthorization struct {
	mu       sync.Mutex
	identity SandboxAuthorizationIdentity
	plan     ContainerPlan
	calls    map[int]SandboxResourceCall
	claimed  map[int]bool
	ledger   CallLedger
	now      func() time.Time
	aborted  bool
}

// NewPreparedSandboxAuthorization admits only calls already reserved in the
// ledger. Runner still performs BeginDispatch immediately before Docker I/O.
// Reconstructing this facade never authorizes a DISPATCHING/SENT/terminal key.
func NewPreparedSandboxAuthorization(ctx context.Context, identity SandboxAuthorizationIdentity, plan ContainerPlan, calls []SandboxResourceCall, ledger CallLedger, now func() time.Time) (SandboxDispatchAuthorization, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if len(plan.Resources) == 0 || ledger == nil || now == nil {
		return nil, errors.New("prepared sandbox dependencies and resource plan are required")
	}
	a := &preparedSandboxAuthorization{identity: identity, plan: plan.Clone(), calls: make(map[int]SandboxResourceCall), claimed: make(map[int]bool), ledger: ledger, now: now}
	seenRecords := make(map[domain.CallRecordID]bool)
	seenPhysical := make(map[domain.AttemptCallID]bool)
	for _, call := range calls {
		if call.CallRecordID.Validate() != nil || call.AttemptCallID.Validate() != nil || call.ResourceOrdinal < 0 || call.ResourceOrdinal >= len(plan.Resources) || plan.Resources[call.ResourceOrdinal].Kind == ResourceCgroup {
			return nil, errors.New("invalid planned sandbox call binding")
		}
		if _, exists := a.calls[call.ResourceOrdinal]; exists || seenRecords[call.CallRecordID] || seenPhysical[call.AttemptCallID] {
			return nil, errors.New("sandbox resource calls must be unique")
		}
		a.calls[call.ResourceOrdinal], seenRecords[call.CallRecordID], seenPhysical[call.AttemptCallID] = call, true, true
	}
	for _, resource := range a.plan.Resources {
		if resource.Kind == ResourceCgroup {
			continue
		}
		if _, err := a.readPrepared(ctx, resource.Ordinal); err != nil {
			return nil, err
		}
	}
	return a, nil
}

func (a *preparedSandboxAuthorization) readPrepared(ctx context.Context, ordinal int) (domain.PhysicalCall, error) {
	call, ok := a.calls[ordinal]
	if !ok {
		return domain.PhysicalCall{}, errors.New("sandbox resource has no prepared call")
	}
	p, err := a.ledger.LoadCall(ctx, call.CallRecordID)
	if err != nil {
		return domain.PhysicalCall{}, err
	}
	if err := p.Validate(); err != nil {
		return domain.PhysicalCall{}, err
	}
	digest, err := SandboxResourceRequestDigest(a.identity, a.plan, ordinal)
	if err != nil {
		return domain.PhysicalCall{}, err
	}
	i, c := a.identity, p.Call
	if c.RunID != i.RunID || c.StageName != i.StageName || c.AttemptID != i.AttemptID || c.Kind != i.Kind || c.Provider != "docker" || c.LogicalOperationID != SandboxResourceLogicalOperation(i, ordinal) || c.RequestDigest != digest || c.PolicyDigest != i.ScopeDigest || c.State != domain.CallRecordPrepared || c.RetryPolicy.MaxAttempts != 1 || len(p.PhysicalCalls) != 1 {
		return domain.PhysicalCall{}, errors.New("prepared sandbox call differs from exact resource scope")
	}
	physical := p.PhysicalCalls[0]
	kind := domain.PhysicalDockerContainerCreate
	if a.plan.Resources[ordinal].Kind == ResourceVolume {
		kind = domain.PhysicalDockerVolumeCreate
	}
	if physical.ID != call.AttemptCallID || physical.CallRecordID != c.ID || physical.RunID != i.RunID || physical.StageName != i.StageName || physical.AttemptID != i.AttemptID || physical.Kind != kind || physical.Provider != "docker" || physical.RequestDigest != digest || physical.Ordinal != 1 || physical.RetryOrdinal != 1 || physical.State != domain.PhysicalPrepared {
		return domain.PhysicalCall{}, errors.New("sandbox resource is not a fresh prepared physical key")
	}
	if kind == domain.PhysicalDockerVolumeCreate {
		if len(p.Reservations) != 0 {
			return domain.PhysicalCall{}, errors.New("volume call carries unrelated reservations")
		}
	} else {
		if len(p.Reservations) != 1 {
			return domain.PhysicalCall{}, errors.New("container call requires its count reservation")
		}
		r := p.Reservations[0]
		if r.RunID != i.RunID || r.StageName != i.StageName || r.AttemptID != i.AttemptID || r.CallRecordID != c.ID || r.AttemptCallID != physical.ID || r.Dimension != domain.BudgetDockerContainerCreates || r.UpperBound != 1 || r.State != domain.ReservationReserved {
			return domain.PhysicalCall{}, errors.New("container reservation differs from the physical key")
		}
	}
	return physical, nil
}

func (a *preparedSandboxAuthorization) LogicalOperationID() string {
	return a.identity.LogicalOperationID
}
func (a *preparedSandboxAuthorization) RunID() domain.RunID         { return a.identity.RunID }
func (a *preparedSandboxAuthorization) StageName() domain.StageName { return a.identity.StageName }
func (a *preparedSandboxAuthorization) AttemptID() domain.AttemptID { return a.identity.AttemptID }
func (a *preparedSandboxAuthorization) SandboxExecutionID() domain.SandboxExecutionID {
	return a.identity.SandboxExecutionID
}
func (a *preparedSandboxAuthorization) ScopeDigest() domain.Digest      { return a.identity.ScopeDigest }
func (a *preparedSandboxAuthorization) PlanDigest() domain.Digest       { return a.plan.PlanDigest }
func (a *preparedSandboxAuthorization) ContainerPlan() ContainerPlan    { return a.plan.Clone() }
func (*preparedSandboxAuthorization) sealSandboxDispatchAuthorization() {}
func (*preparedSandboxAuthorization) ClaimEnginePing(context.Context) (DispatchAuthorization, error) {
	return nil, errors.New("compile/run authorization cannot claim an Engine ping")
}

func (a *preparedSandboxAuthorization) claim(ctx context.Context, kind ResourceKind, role ResourceRole) (SandboxResourceCall, error) {
	if a.aborted {
		return SandboxResourceCall{}, errors.New("sandbox authorization was aborted")
	}
	for _, r := range a.plan.Resources {
		if r.Kind != kind || a.claimed[r.Ordinal] {
			continue
		}
		if r.Role != role {
			return SandboxResourceCall{}, errors.New("sandbox claim is out of planned role order")
		}
		if _, err := a.readPrepared(ctx, r.Ordinal); err != nil {
			return SandboxResourceCall{}, err
		}
		a.claimed[r.Ordinal] = true
		return a.calls[r.Ordinal], nil
	}
	return SandboxResourceCall{}, errors.New("sandbox plan has no remaining resource of this kind")
}

func (a *preparedSandboxAuthorization) ClaimNextContainer(ctx context.Context, role ContainerRole) (ContainerDispatchGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.claim(ctx, ResourceContainer, ResourceRole(role))
	if err != nil {
		return ContainerDispatchGrant{}, err
	}
	return ContainerDispatchGrant{Role: role, Auth: preparedSandboxGrant{identity: a.identity, callID: c.AttemptCallID}, CallRecordID: c.CallRecordID, ExpectedRunVersion: a.identity.ExpectedRunVersion}, nil
}

func (a *preparedSandboxAuthorization) ClaimNextVolume(ctx context.Context, role ResourceRole) (VolumeDispatchGrant, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, err := a.claim(ctx, ResourceVolume, role)
	if err != nil {
		return VolumeDispatchGrant{}, err
	}
	return VolumeDispatchGrant{Role: role, Auth: preparedSandboxGrant{identity: a.identity, callID: c.AttemptCallID}, CallRecordID: c.CallRecordID, ExpectedRunVersion: a.identity.ExpectedRunVersion, Durable: true}, nil
}

func (a *preparedSandboxAuthorization) AbortRemaining(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.aborted = true
	var errs []error
	for _, r := range a.plan.Resources {
		c, ok := a.calls[r.Ordinal]
		if !ok {
			continue
		}
		p, err := a.ledger.LoadCall(ctx, c.CallRecordID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(p.PhysicalCalls) != 1 || p.PhysicalCalls[0].ID != c.AttemptCallID {
			errs = append(errs, errors.New("sandbox cleanup call binding changed"))
			continue
		}
		if p.PhysicalCalls[0].State != domain.PhysicalPrepared {
			continue
		}
		i := a.identity
		err = a.ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: i.RunID, ExpectedRunVersion: i.ExpectedRunVersion, StageName: i.StageName, AttemptID: i.AttemptID, CallRecordID: c.CallRecordID, AttemptCallID: c.AttemptCallID, State: domain.PhysicalAbortedNoDispatch, Outcome: domain.PhysicalOutcomeNoSend, Failure: &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}, IdempotencyKey: "abort_" + string(c.AttemptCallID), At: a.now().UTC()})
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

type preparedSandboxGrant struct {
	identity SandboxAuthorizationIdentity
	callID   domain.AttemptCallID
}

func (g preparedSandboxGrant) CallID() domain.AttemptCallID { return g.callID }
func (g preparedSandboxGrant) RunID() domain.RunID          { return g.identity.RunID }
func (g preparedSandboxGrant) AttemptID() domain.AttemptID  { return g.identity.AttemptID }
func (g preparedSandboxGrant) SandboxExecutionID() domain.SandboxExecutionID {
	return g.identity.SandboxExecutionID
}
func (g preparedSandboxGrant) ScopeDigest() domain.Digest { return g.identity.ScopeDigest }
func (preparedSandboxGrant) sealDispatchAuthorization()   {}
