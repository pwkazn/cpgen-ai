package domain

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SandboxExecutionState is the durable state of one sealed Docker resource
// plan. It is deliberately separate from the workflow RunState.
type SandboxExecutionState string

const (
	SandboxExecutionPlanned             SandboxExecutionState = "PLANNED"
	SandboxExecutionArmed               SandboxExecutionState = "ARMED"
	SandboxExecutionRunning             SandboxExecutionState = "RUNNING"
	SandboxExecutionCleanupPending      SandboxExecutionState = "CLEANUP_PENDING"
	SandboxExecutionCleaned             SandboxExecutionState = "CLEANED"
	SandboxExecutionInterrupted         SandboxExecutionState = "INTERRUPTED"
	SandboxExecutionStatePlanned                              = SandboxExecutionPlanned
	SandboxExecutionStateArmed                                = SandboxExecutionArmed
	SandboxExecutionStateRunning                              = SandboxExecutionRunning
	SandboxExecutionStateCleanupPending                       = SandboxExecutionCleanupPending
	SandboxExecutionStateCleaned                              = SandboxExecutionCleaned
	SandboxExecutionStateInterrupted                          = SandboxExecutionInterrupted
)

func (v SandboxExecutionState) Valid() bool {
	switch v {
	case SandboxExecutionPlanned, SandboxExecutionArmed, SandboxExecutionRunning,
		SandboxExecutionCleanupPending, SandboxExecutionCleaned, SandboxExecutionInterrupted:
		return true
	default:
		return false
	}
}

// SandboxResourcePhase is monotone. UNKNOWN and CLEANUP_PENDING are durable
// evidence that the external boundary was not settled, not retry licenses.
type SandboxResourcePhase string

const (
	SandboxResourcePlanned             SandboxResourcePhase = "PLANNED"
	SandboxResourceCreating            SandboxResourcePhase = "CREATING"
	SandboxResourceDispatching         SandboxResourcePhase = "DISPATCHING"
	SandboxResourceSent                SandboxResourcePhase = "SENT"
	SandboxResourceUnknown             SandboxResourcePhase = "UNKNOWN"
	SandboxResourceCompleted           SandboxResourcePhase = "COMPLETED"
	SandboxResourceStarted             SandboxResourcePhase = "STARTED"
	SandboxResourceCleanupPending      SandboxResourcePhase = "CLEANUP_PENDING"
	SandboxResourceStopped             SandboxResourcePhase = "STOPPED"
	SandboxResourceCleaned             SandboxResourcePhase = "CLEANED"
	SandboxResourceInterrupted         SandboxResourcePhase = "INTERRUPTED"
	SandboxResourcePhasePlanned                             = SandboxResourcePlanned
	SandboxResourcePhaseCreating                            = SandboxResourceCreating
	SandboxResourcePhaseDispatching                         = SandboxResourceDispatching
	SandboxResourcePhaseSent                                = SandboxResourceSent
	SandboxResourcePhaseUnknown                             = SandboxResourceUnknown
	SandboxResourcePhaseCompleted                           = SandboxResourceCompleted
	SandboxResourcePhaseStarted                             = SandboxResourceStarted
	SandboxResourcePhaseCleanupPending                      = SandboxResourceCleanupPending
	SandboxResourcePhaseStopped                             = SandboxResourceStopped
	SandboxResourcePhaseCleaned                             = SandboxResourceCleaned
	SandboxResourcePhaseInterrupted                         = SandboxResourceInterrupted
)

func (v SandboxResourcePhase) Valid() bool {
	switch v {
	case SandboxResourcePlanned, SandboxResourceCreating, SandboxResourceDispatching,
		SandboxResourceSent, SandboxResourceUnknown, SandboxResourceCompleted,
		SandboxResourceStarted, SandboxResourceCleanupPending, SandboxResourceStopped,
		SandboxResourceCleaned, SandboxResourceInterrupted:
		return true
	default:
		return false
	}
}

func sandboxPhaseRank(v SandboxResourcePhase) int {
	switch v {
	case SandboxResourcePlanned:
		return 0
	case SandboxResourceCreating:
		return 1
	case SandboxResourceDispatching:
		return 2
	case SandboxResourceSent:
		return 3
	case SandboxResourceUnknown:
		return 4
	case SandboxResourceCompleted:
		return 5
	case SandboxResourceStarted:
		return 6
	case SandboxResourceCleanupPending:
		return 7
	case SandboxResourceStopped:
		return 8
	case SandboxResourceCleaned:
		return 9
	case SandboxResourceInterrupted:
		return 10
	default:
		return -1
	}
}

// SandboxAuthorizationIdentity is the sealed identity accepted by Docker.
// Keep this value independent of process ownership and takeover state.
type SandboxAuthorizationIdentity struct {
	RunID                RunID              `json:"run_id"`
	AttemptID            AttemptID          `json:"attempt_id"`
	SandboxExecutionID   SandboxExecutionID `json:"sandbox_execution_id"`
	LogicalOperationID   string             `json:"logical_operation_id"`
	ScopeDigest          Digest             `json:"scope_digest"`
	PlanDigest           Digest             `json:"plan_digest"`
	EngineIdentityDigest Digest             `json:"engine_identity_digest"`
}

func (i SandboxAuthorizationIdentity) Validate() error {
	if err := i.RunID.Validate(); err != nil {
		return err
	}
	if err := i.AttemptID.Validate(); err != nil {
		return err
	}
	if err := i.SandboxExecutionID.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(i.LogicalOperationID) == "" || len(i.LogicalOperationID) > 256 {
		return errors.New("logical operation id is required")
	}
	for name, value := range map[string]Digest{"scope": i.ScopeDigest, "plan": i.PlanDigest, "engine identity": i.EngineIdentityDigest} {
		if err := value.Validate(); err != nil {
			return fmt.Errorf("%s digest: %w", name, err)
		}
	}
	return nil
}

// Digest returns the canonical authorization identity digest. Physical call
// identity is intentionally not part of this sealed operation identity.
func (i SandboxAuthorizationIdentity) Digest() Digest {
	var encoded bytes.Buffer
	write := func(value string) {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(value)))
		encoded.Write(n[:])
		encoded.WriteString(value)
	}
	write("cpgen.sandbox-authorization/v1")
	write(string(i.RunID))
	write(string(i.AttemptID))
	write(string(i.SandboxExecutionID))
	write(i.LogicalOperationID)
	write(string(i.ScopeDigest))
	write(string(i.PlanDigest))
	write(string(i.EngineIdentityDigest))
	return SumBytes(encoded.Bytes())
}

type SandboxExecution struct {
	ID                           SandboxExecutionID    `json:"id"`
	RunID                        RunID                 `json:"run_id"`
	AttemptID                    AttemptID             `json:"attempt_id"`
	StageName                    StageName             `json:"stage_name"`
	LogicalOperationID           string                `json:"logical_operation_id"`
	ScopeDigest                  Digest                `json:"scope_digest"`
	PlanDigest                   Digest                `json:"plan_digest"`
	EngineIdentityDigest         Digest                `json:"engine_identity_digest"`
	WatchdogControlRef           string                `json:"watchdog_control_ref,omitempty"`
	WatchdogTokenDigest          Digest                `json:"watchdog_token_digest"`
	State                        SandboxExecutionState `json:"state"`
	LifecycleVersion             int64                 `json:"lifecycle_version"`
	CleanupVersion               int64                 `json:"cleanup_version"`
	SafetyDeadlineUTC            time.Time             `json:"safety_deadline_utc"`
	CleanupDeadlineUTC           time.Time             `json:"cleanup_deadline_utc"`
	CreatedAt                    time.Time             `json:"created_at"`
	UpdatedAt                    time.Time             `json:"updated_at"`
	ReconciliationEvidenceDigest Digest                `json:"reconciliation_evidence_digest,omitempty"`
	Resources                    []SandboxResource     `json:"resources,omitempty"`
}

func (e SandboxExecution) Identity() SandboxAuthorizationIdentity {
	return SandboxAuthorizationIdentity{RunID: e.RunID, AttemptID: e.AttemptID, SandboxExecutionID: e.ID,
		LogicalOperationID: e.LogicalOperationID, ScopeDigest: e.ScopeDigest, PlanDigest: e.PlanDigest,
		EngineIdentityDigest: e.EngineIdentityDigest}
}

func (e SandboxExecution) Validate() error {
	if err := e.ID.Validate(); err != nil {
		return err
	}
	if err := e.Identity().Validate(); err != nil {
		return err
	}
	if err := e.StageName.Validate(); err != nil {
		return err
	}
	if err := e.WatchdogTokenDigest.Validate(); err != nil {
		return fmt.Errorf("watchdog token digest: %w", err)
	}
	if strings.TrimSpace(e.WatchdogControlRef) == "" {
		return errors.New("watchdog control reference is required")
	}
	if e.ReconciliationEvidenceDigest != "" {
		if err := e.ReconciliationEvidenceDigest.Validate(); err != nil {
			return fmt.Errorf("reconciliation evidence digest: %w", err)
		}
	}
	if !e.State.Valid() || e.LifecycleVersion <= 0 || e.CleanupVersion < 0 {
		return errors.New("sandbox execution state or version is invalid")
	}
	if err := validateUTCTime("safety deadline", e.SafetyDeadlineUTC); err != nil {
		return err
	}
	if err := validateUTCTime("cleanup deadline", e.CleanupDeadlineUTC); err != nil {
		return err
	}
	if err := validateUTCTime("created at", e.CreatedAt); err != nil {
		return err
	}
	if err := validateUTCTime("updated at", e.UpdatedAt); err != nil {
		return err
	}
	if e.CleanupDeadlineUTC.Before(e.SafetyDeadlineUTC) || e.UpdatedAt.Before(e.CreatedAt) {
		return errors.New("sandbox execution deadlines or timestamps are invalid")
	}
	return nil
}

type SandboxResource struct {
	ID                    SandboxResourceID    `json:"id"`
	ExecutionID           SandboxExecutionID   `json:"execution_id"`
	PlanOrdinal           int                  `json:"plan_ordinal"`
	Kind                  string               `json:"kind"`
	Role                  string               `json:"role"`
	PhysicalCallID        *AttemptCallID       `json:"physical_call_id,omitempty"`
	DeterministicName     string               `json:"deterministic_name"`
	ExpectedLabelsDigest  Digest               `json:"expected_labels_digest"`
	LabelsDigest          Digest               `json:"labels_digest"`
	EngineResourceID      string               `json:"engine_resource_id,omitempty"`
	EngineIdentityDigest  Digest               `json:"engine_identity_digest"`
	CgroupIdentityDigest  *Digest              `json:"cgroup_identity_digest,omitempty"`
	CreationNonce         string               `json:"creation_nonce,omitempty"`
	StopProofDigest       Digest               `json:"stop_proof_digest,omitempty"`
	StopProofKind         string               `json:"stop_proof_kind,omitempty"`
	CleanupEvidenceDigest Digest               `json:"cleanup_evidence_digest,omitempty"`
	Phase                 SandboxResourcePhase `json:"phase"`
	Version               int64                `json:"version"`
	CreatedAt             time.Time            `json:"created_at"`
	UpdatedAt             time.Time            `json:"updated_at"`
}

func (r SandboxResource) Validate() error {
	if err := r.ID.Validate(); err != nil {
		return err
	}
	if err := r.ExecutionID.Validate(); err != nil {
		return err
	}
	if r.PlanOrdinal < 0 || strings.TrimSpace(r.Kind) == "" || strings.TrimSpace(r.Role) == "" || strings.TrimSpace(r.DeterministicName) == "" {
		return errors.New("sandbox resource identity is invalid")
	}
	if err := r.ExpectedLabelsDigest.Validate(); err != nil {
		return fmt.Errorf("expected labels digest: %w", err)
	}
	if r.LabelsDigest != "" {
		if err := r.LabelsDigest.Validate(); err != nil {
			return fmt.Errorf("labels digest: %w", err)
		}
	}
	if err := r.EngineIdentityDigest.Validate(); err != nil {
		return fmt.Errorf("engine identity digest: %w", err)
	}
	if r.CgroupIdentityDigest != nil {
		if err := r.CgroupIdentityDigest.Validate(); err != nil {
			return err
		}
	}
	if r.PhysicalCallID != nil {
		if err := r.PhysicalCallID.Validate(); err != nil {
			return err
		}
	}
	if r.StopProofDigest != "" {
		if err := r.StopProofDigest.Validate(); err != nil {
			return fmt.Errorf("stop proof digest: %w", err)
		}
		if strings.TrimSpace(r.StopProofKind) == "" {
			return errors.New("stop proof kind is required with stop proof digest")
		}
	}
	if r.CleanupEvidenceDigest != "" {
		if err := r.CleanupEvidenceDigest.Validate(); err != nil {
			return fmt.Errorf("cleanup evidence digest: %w", err)
		}
	}
	if !r.Phase.Valid() || r.Version <= 0 {
		return errors.New("sandbox resource phase or version is invalid")
	}
	if err := validateUTCTime("created at", r.CreatedAt); err != nil {
		return err
	}
	if err := validateUTCTime("updated at", r.UpdatedAt); err != nil {
		return err
	}
	if r.UpdatedAt.Before(r.CreatedAt) {
		return errors.New("sandbox resource updated at precedes created at")
	}
	if r.Phase == SandboxResourceCompleted || r.Phase == SandboxResourceStarted || r.Phase == SandboxResourceStopped || r.Phase == SandboxResourceCleaned {
		if strings.TrimSpace(r.EngineResourceID) == "" {
			return errors.New("settled resource requires engine identity")
		}
	}
	if r.Phase == SandboxResourceStopped || r.Phase == SandboxResourceCleaned || r.Phase == SandboxResourceInterrupted {
		if r.StopProofDigest == "" || strings.TrimSpace(r.StopProofKind) == "" {
			return errors.New("cleanup-settled resource requires persisted stop proof")
		}
	}
	if r.Phase == SandboxResourceCleaned && r.CleanupEvidenceDigest == "" {
		return errors.New("CLEANED resource requires persisted cleanup evidence")
	}
	return nil
}

type SandboxWatchdogControl struct {
	ExecutionID       SandboxExecutionID `json:"execution_id"`
	ControlID         string             `json:"control_id"`
	ProcessRecordRef  string             `json:"process_record_ref"`
	ControlFileDigest Digest             `json:"control_file_digest"`
	TokenDigest       Digest             `json:"token_digest"`
	ArmedAt           time.Time          `json:"armed_at"`
}

func (c SandboxWatchdogControl) Validate() error {
	if err := c.ExecutionID.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(c.ControlID) == "" || strings.TrimSpace(c.ProcessRecordRef) == "" {
		return errors.New("watchdog control identity is required")
	}
	if err := c.ControlFileDigest.Validate(); err != nil {
		return err
	}
	if err := c.TokenDigest.Validate(); err != nil {
		return err
	}
	return validateUTCTime("watchdog armed at", c.ArmedAt)
}

type SandboxPreCreateACK struct {
	ExecutionID       SandboxExecutionID `json:"execution_id"`
	ResourceID        SandboxResourceID  `json:"resource_id"`
	ResourceVersion   int64              `json:"resource_version"`
	LabelsDigest      Digest             `json:"labels_digest"`
	WatchdogRecordRef string             `json:"watchdog_record_ref"`
	IdempotencyKey    string             `json:"idempotency_key"`
	At                time.Time          `json:"at"`
}

func (a SandboxPreCreateACK) Validate() error {
	if err := a.ExecutionID.Validate(); err != nil {
		return err
	}
	if err := a.ResourceID.Validate(); err != nil {
		return err
	}
	if a.ResourceVersion <= 0 {
		return errors.New("resource ACK version must be positive")
	}
	if err := a.LabelsDigest.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.WatchdogRecordRef) == "" {
		return errors.New("watchdog record reference is required")
	}
	if err := validateIdempotencyKey(a.IdempotencyKey); err != nil {
		return err
	}
	return validateUTCTime("ACK time", a.At)
}

type SandboxCleanupBlocker struct {
	ExecutionID SandboxExecutionID `json:"execution_id"`
	ResourceID  SandboxResourceID  `json:"resource_id"`
	Reason      string             `json:"reason"`
	Manual      bool               `json:"manual"`
}

// RecordResourceStopProofCommand is the only command allowed to persist a
// stopped resource. The proof digest is generated from an independent
// stop/kill/wait/inspect observation outside the database transaction.
type RecordResourceStopProofCommand struct {
	ExecutionID          SandboxExecutionID `json:"execution_id"`
	ResourceID           SandboxResourceID  `json:"resource_id"`
	ExpectedVersion      int64              `json:"expected_version"`
	EngineResourceID     string             `json:"engine_resource_id"`
	EngineIdentityDigest Digest             `json:"engine_identity_digest"`
	LabelsDigest         Digest             `json:"labels_digest,omitempty"`
	ProofDigest          Digest             `json:"proof_digest"`
	ProofKind            string             `json:"proof_kind"`
	At                   time.Time          `json:"at"`
}

func (c RecordResourceStopProofCommand) Validate() error {
	if err := validateSandboxResourceCommand(c.ExecutionID, c.ResourceID, c.ExpectedVersion, "stop_"+string(c.ResourceID), c.At); err != nil {
		return err
	}
	if strings.TrimSpace(c.EngineResourceID) == "" || strings.TrimSpace(c.ProofKind) == "" {
		return errors.New("stop proof requires exact engine resource and proof kind")
	}
	if err := c.EngineIdentityDigest.Validate(); err != nil {
		return err
	}
	if c.LabelsDigest != "" {
		if err := c.LabelsDigest.Validate(); err != nil {
			return err
		}
	}
	return c.ProofDigest.Validate()
}

// RecordResourceCleaned persists removal evidence after a resource has been
// proven stopped. It is deliberately separate from AdvanceResource.
type RecordResourceCleanedCommand struct {
	ExecutionID          SandboxExecutionID `json:"execution_id"`
	ResourceID           SandboxResourceID  `json:"resource_id"`
	ExpectedVersion      int64              `json:"expected_version"`
	EngineResourceID     string             `json:"engine_resource_id"`
	EngineIdentityDigest Digest             `json:"engine_identity_digest"`
	EvidenceDigest       Digest             `json:"evidence_digest"`
	At                   time.Time          `json:"at"`
}

func (c RecordResourceCleanedCommand) Validate() error {
	if err := validateSandboxResourceCommand(c.ExecutionID, c.ResourceID, c.ExpectedVersion, "clean_"+string(c.ResourceID), c.At); err != nil {
		return err
	}
	if strings.TrimSpace(c.EngineResourceID) == "" {
		return errors.New("cleaned resource requires exact engine resource")
	}
	if err := c.EngineIdentityDigest.Validate(); err != nil {
		return err
	}
	return c.EvidenceDigest.Validate()
}

// RecordResourceInterrupted is used for a resource before any external create
// was authorized. It covers PLANNED and CREATING rows when claim/dispatch
// failed, and records a deterministic reason digest instead of consulting the
// Engine. It cannot be reached through the arbitrary phase command.
type RecordResourceInterruptedCommand struct {
	ExecutionID     SandboxExecutionID `json:"execution_id"`
	ResourceID      SandboxResourceID  `json:"resource_id"`
	ExpectedVersion int64              `json:"expected_version"`
	ReasonDigest    Digest             `json:"reason_digest"`
	At              time.Time          `json:"at"`
}

func (c RecordResourceInterruptedCommand) Validate() error {
	if err := validateSandboxResourceCommand(c.ExecutionID, c.ResourceID, c.ExpectedVersion, "interrupt_"+string(c.ResourceID), c.At); err != nil {
		return err
	}
	return c.ReasonDigest.Validate()
}

type SandboxReconcileReport struct {
	RunID         RunID                   `json:"run_id"`
	Executions    []SandboxExecution      `json:"executions,omitempty"`
	Resources     []SandboxResource       `json:"resources,omitempty"`
	Cleaned       int                     `json:"cleaned"`
	Pending       int                     `json:"pending"`
	ManualCleanup []SandboxCleanupBlocker `json:"manual_cleanup,omitempty"`
	Completed     bool                    `json:"completed"`
}

type PrepareExecutionRequest struct {
	ExecutionID          SandboxExecutionID `json:"execution_id"`
	RunID                RunID              `json:"run_id"`
	AttemptID            AttemptID          `json:"attempt_id"`
	StageName            StageName          `json:"stage_name"`
	LogicalOperationID   string             `json:"logical_operation_id"`
	ScopeDigest          Digest             `json:"scope_digest"`
	PlanDigest           Digest             `json:"plan_digest"`
	EngineIdentityDigest Digest             `json:"engine_identity_digest"`
	Resources            []SandboxResource  `json:"resources"`
	WatchdogControlRef   string             `json:"watchdog_control_ref"`
	WatchdogTokenDigest  Digest             `json:"watchdog_token_digest"`
	SafetyDeadlineUTC    time.Time          `json:"safety_deadline_utc"`
	CleanupDeadlineUTC   time.Time          `json:"cleanup_deadline_utc"`
	IdempotencyKey       string             `json:"idempotency_key"`
	At                   time.Time          `json:"at"`
}

func (r PrepareExecutionRequest) Validate() error {
	if err := r.ExecutionID.Validate(); err != nil {
		return err
	}
	if err := (SandboxAuthorizationIdentity{RunID: r.RunID, AttemptID: r.AttemptID, SandboxExecutionID: r.ExecutionID, LogicalOperationID: r.LogicalOperationID, ScopeDigest: r.ScopeDigest, PlanDigest: r.PlanDigest, EngineIdentityDigest: r.EngineIdentityDigest}).Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(r.WatchdogControlRef) == "" {
		return errors.New("watchdog control reference is required")
	}
	if err := r.WatchdogTokenDigest.Validate(); err != nil {
		return err
	}
	if len(r.Resources) == 0 {
		return errors.New("sandbox execution resource plan is empty")
	}
	seen := make(map[int]struct{}, len(r.Resources))
	for index, resource := range r.Resources {
		if resource.ExecutionID != r.ExecutionID || resource.PlanOrdinal != index {
			return errors.New("sandbox resource plan is not contiguous and execution-bound")
		}
		if _, ok := seen[resource.PlanOrdinal]; ok {
			return errors.New("duplicate sandbox resource ordinal")
		}
		seen[resource.PlanOrdinal] = struct{}{}
		if err := resource.Validate(); err != nil {
			return fmt.Errorf("resource %d: %w", index, err)
		}
		if resource.Phase != SandboxResourcePlanned || resource.Version != 1 {
			return errors.New("prepared resource must start PLANNED at version 1")
		}
	}
	if err := validateUTCTime("safety deadline", r.SafetyDeadlineUTC); err != nil {
		return err
	}
	if err := validateUTCTime("cleanup deadline", r.CleanupDeadlineUTC); err != nil {
		return err
	}
	if r.CleanupDeadlineUTC.Before(r.SafetyDeadlineUTC) {
		return errors.New("cleanup deadline precedes safety deadline")
	}
	if err := validateUTCTime("command time", r.At); err != nil {
		return err
	}
	return validateIdempotencyKey(r.IdempotencyKey)
}

type WatchdogArmed struct {
	ExecutionID       SandboxExecutionID `json:"execution_id"`
	ExpectedVersion   int64              `json:"expected_version"`
	ControlRecordRef  string             `json:"control_record_ref"`
	ControlFileDigest Digest             `json:"control_file_digest"`
	TokenDigest       Digest             `json:"token_digest"`
	IdempotencyKey    string             `json:"idempotency_key"`
	At                time.Time          `json:"at"`
}

func (c WatchdogArmed) Validate() error {
	if err := c.ExecutionID.Validate(); err != nil {
		return err
	}
	if c.ExpectedVersion <= 0 || strings.TrimSpace(c.ControlRecordRef) == "" {
		return errors.New("watchdog arm identity is invalid")
	}
	if err := c.ControlFileDigest.Validate(); err != nil {
		return err
	}
	if err := c.TokenDigest.Validate(); err != nil {
		return err
	}
	if err := validateIdempotencyKey(c.IdempotencyKey); err != nil {
		return err
	}
	return validateUTCTime("command time", c.At)
}

type BeginResourceCreate struct {
	ExecutionID     SandboxExecutionID `json:"execution_id"`
	ResourceID      SandboxResourceID  `json:"resource_id"`
	ExpectedVersion int64              `json:"expected_version"`
	// PhysicalCallID identifies the prepared call which may cross the
	// external resource boundary. It is optional while entering CREATING for
	// compatibility with older callers, but must be persisted before
	// DISPATCHING is recorded.
	PhysicalCallID *AttemptCallID `json:"physical_call_id,omitempty"`
	IdempotencyKey string         `json:"idempotency_key"`
	At             time.Time      `json:"at"`
}

func (r BeginResourceCreate) Validate() error {
	if err := validateSandboxResourceCommand(r.ExecutionID, r.ResourceID, r.ExpectedVersion, r.IdempotencyKey, r.At); err != nil {
		return err
	}
	if r.PhysicalCallID != nil {
		return r.PhysicalCallID.Validate()
	}
	return nil
}

type PreCreateRequest struct {
	ExecutionID SandboxExecutionID `json:"execution_id"`
	ResourceID  SandboxResourceID  `json:"resource_id"`
	Resource    SandboxResource    `json:"resource"`
	Version     int64              `json:"version"`
}

func (r PreCreateRequest) Validate() error {
	if err := r.ExecutionID.Validate(); err != nil {
		return err
	}
	if err := r.ResourceID.Validate(); err != nil {
		return err
	}
	if r.Version <= 0 || r.Resource.ID != r.ResourceID || r.Resource.ExecutionID != r.ExecutionID {
		return errors.New("pre-create request identity is invalid")
	}
	return r.Resource.Validate()
}

type PreCreateACK = SandboxPreCreateACK

type AdvanceResourceRequest struct {
	ExecutionID          SandboxExecutionID   `json:"execution_id"`
	ResourceID           SandboxResourceID    `json:"resource_id"`
	ExpectedVersion      int64                `json:"expected_version"`
	Phase                SandboxResourcePhase `json:"phase"`
	PhysicalCallID       *AttemptCallID       `json:"physical_call_id,omitempty"`
	EngineResourceID     string               `json:"engine_resource_id,omitempty"`
	EngineIdentityDigest Digest               `json:"engine_identity_digest,omitempty"`
	LabelsDigest         Digest               `json:"labels_digest,omitempty"`
	IdempotencyKey       string               `json:"idempotency_key"`
	At                   time.Time            `json:"at"`
}

func (r AdvanceResourceRequest) Validate() error {
	if err := validateSandboxResourceCommand(r.ExecutionID, r.ResourceID, r.ExpectedVersion, r.IdempotencyKey, r.At); err != nil {
		return err
	}
	if !r.Phase.Valid() || r.Phase == SandboxResourcePlanned || r.Phase == SandboxResourceStopped || r.Phase == SandboxResourceCleaned || r.Phase == SandboxResourceInterrupted {
		return errors.New("resource advance phase is invalid")
	}
	if r.PhysicalCallID != nil {
		if err := r.PhysicalCallID.Validate(); err != nil {
			return err
		}
	}
	if r.EngineIdentityDigest != "" {
		if err := r.EngineIdentityDigest.Validate(); err != nil {
			return err
		}
	}
	if r.LabelsDigest != "" {
		if err := r.LabelsDigest.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type MarkCleanupPendingCommand struct {
	ExecutionID     SandboxExecutionID `json:"execution_id"`
	ExpectedVersion int64              `json:"expected_version"`
	Reason          string             `json:"reason"`
	IdempotencyKey  string             `json:"idempotency_key"`
	At              time.Time          `json:"at"`
}

type FinishCleanupCommand struct {
	ExecutionID          SandboxExecutionID `json:"execution_id"`
	ExpectedVersion      int64              `json:"expected_version"`
	ReconciliationDigest Digest             `json:"reconciliation_digest"`
	IdempotencyKey       string             `json:"idempotency_key"`
	At                   time.Time          `json:"at"`
}

func (r MarkCleanupPendingCommand) Validate() error {
	if err := validateSandboxExecutionCommand(r.ExecutionID, r.ExpectedVersion, r.IdempotencyKey, r.At); err != nil {
		return err
	}
	if strings.TrimSpace(r.Reason) == "" {
		return errors.New("cleanup reason is required")
	}
	return nil
}
func (r FinishCleanupCommand) Validate() error {
	if err := validateSandboxExecutionCommand(r.ExecutionID, r.ExpectedVersion, r.IdempotencyKey, r.At); err != nil {
		return err
	}
	return r.ReconciliationDigest.Validate()
}

func validateSandboxExecutionCommand(id SandboxExecutionID, version int64, key string, at time.Time) error {
	if err := id.Validate(); err != nil {
		return err
	}
	if version <= 0 {
		return errors.New("expected sandbox execution version must be positive")
	}
	if err := validateUTCTime("command time", at); err != nil {
		return err
	}
	return validateIdempotencyKey(key)
}
func validateSandboxResourceCommand(execution SandboxExecutionID, resource SandboxResourceID, version int64, key string, at time.Time) error {
	if err := validateSandboxExecutionCommand(execution, version, key, at); err != nil {
		return err
	}
	if err := resource.Validate(); err != nil {
		return err
	}
	return nil
}
