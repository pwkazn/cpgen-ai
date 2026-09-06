//go:build cpgen_slice0_probe

package docker_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

// recordingLifecycle provides the durable boundaries required by the current
// Runner path while keeping the old Slice 0 tests entirely in-memory.
type recordingLifecycle struct {
	mu        sync.Mutex
	execution domain.SandboxExecution
	resources map[domain.SandboxResourceID]domain.SandboxResource
}

func newRecordingLifecycle() *recordingLifecycle {
	return &recordingLifecycle{resources: make(map[domain.SandboxResourceID]domain.SandboxResource)}
}

func (s *recordingLifecycle) GetSandboxExecution(_ context.Context, id domain.SandboxExecutionID) (domain.SandboxExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execution.ID == "" || s.execution.ID != id {
		return domain.SandboxExecution{}, errors.New("sandbox execution does not exist")
	}
	return s.snapshot(), nil
}

func (s *recordingLifecycle) PrepareExecution(_ context.Context, req domain.PrepareExecutionRequest) (domain.SandboxExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.execution.ID != "" {
		return s.snapshot(), nil
	}
	s.execution = domain.SandboxExecution{ID: req.ExecutionID, RunID: req.RunID, AttemptID: req.AttemptID, StageName: req.StageName, LogicalOperationID: req.LogicalOperationID, ScopeDigest: req.ScopeDigest, PlanDigest: req.PlanDigest, EngineIdentityDigest: req.EngineIdentityDigest, WatchdogControlRef: req.WatchdogControlRef, WatchdogTokenDigest: req.WatchdogTokenDigest, State: domain.SandboxExecutionPlanned, LifecycleVersion: 1, CreatedAt: req.At, UpdatedAt: req.At}
	s.resources = make(map[domain.SandboxResourceID]domain.SandboxResource, len(req.Resources))
	for _, resource := range req.Resources {
		s.resources[resource.ID] = resource
	}
	return s.snapshot(), nil
}

func (s *recordingLifecycle) RecordWatchdogArmed(context.Context, domain.WatchdogArmed) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execution.State = domain.SandboxExecutionArmed
	s.execution.LifecycleVersion++
	return nil
}

func (s *recordingLifecycle) BeginResourceCreate(_ context.Context, req domain.BeginResourceCreate) (domain.PreCreateRequest, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resource := s.resources[req.ResourceID]
	resource.Version++
	resource.Phase = domain.SandboxResourceCreating
	resource.UpdatedAt = req.At
	s.resources[req.ResourceID] = resource
	return domain.PreCreateRequest{ExecutionID: req.ExecutionID, ResourceID: req.ResourceID, Resource: resource, Version: resource.Version}, nil
}

func (s *recordingLifecycle) RecordPreCreateACK(context.Context, domain.PreCreateACK) error {
	return nil
}

func (s *recordingLifecycle) AdvanceResource(_ context.Context, req domain.AdvanceResourceRequest) (domain.SandboxResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resource := s.resources[req.ResourceID]
	resource.Version++
	resource.Phase = req.Phase
	if req.EngineResourceID != "" {
		resource.EngineResourceID = req.EngineResourceID
	}
	if req.LabelsDigest != "" {
		resource.LabelsDigest = req.LabelsDigest
	}
	resource.UpdatedAt = req.At
	s.resources[req.ResourceID] = resource
	return resource, nil
}

func (s *recordingLifecycle) MarkCleanupPending(_ context.Context, req domain.MarkCleanupPendingCommand) (domain.SandboxExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execution.State = domain.SandboxExecutionCleanupPending
	s.execution.LifecycleVersion++
	return s.snapshot(), nil
}
func (s *recordingLifecycle) FinishCleanup(_ context.Context, req domain.FinishCleanupCommand) (domain.SandboxExecution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.execution.State = domain.SandboxExecutionCleaned
	s.execution.LifecycleVersion++
	s.execution.ReconciliationEvidenceDigest = req.ReconciliationDigest
	return s.snapshot(), nil
}

func (s *recordingLifecycle) RecordResourceStopProof(_ context.Context, req domain.RecordResourceStopProofCommand) (domain.SandboxResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resource := s.resources[req.ResourceID]
	resource.Version++
	resource.Phase = domain.SandboxResourceStopped
	resource.EngineResourceID = req.EngineResourceID
	resource.StopProofDigest = req.ProofDigest
	resource.StopProofKind = req.ProofKind
	resource.UpdatedAt = req.At
	s.resources[req.ResourceID] = resource
	return resource, nil
}
func (s *recordingLifecycle) RecordResourceCleaned(_ context.Context, req domain.RecordResourceCleanedCommand) (domain.SandboxResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resource := s.resources[req.ResourceID]
	resource.Version++
	resource.Phase = domain.SandboxResourceCleaned
	resource.CleanupEvidenceDigest = req.EvidenceDigest
	resource.UpdatedAt = req.At
	s.resources[req.ResourceID] = resource
	return resource, nil
}
func (s *recordingLifecycle) RecordResourceInterrupted(_ context.Context, req domain.RecordResourceInterruptedCommand) (domain.SandboxResource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	resource := s.resources[req.ResourceID]
	resource.Version++
	resource.Phase = domain.SandboxResourceInterrupted
	resource.StopProofDigest = req.ReasonDigest
	resource.StopProofKind = "NO_CREATE"
	resource.UpdatedAt = req.At
	s.resources[req.ResourceID] = resource
	return resource, nil
}

func (s *recordingLifecycle) snapshot() domain.SandboxExecution {
	result := s.execution
	result.Resources = make([]domain.SandboxResource, 0, len(s.resources))
	for _, resource := range s.resources {
		result.Resources = append(result.Resources, resource)
	}
	return result
}

var _ port.SandboxLifecycleRecorder = (*recordingLifecycle)(nil)
var _ port.SandboxLifecycleReader = (*recordingLifecycle)(nil)
var _ port.SandboxCleanupRecorder = (*recordingLifecycle)(nil)

type recordingCallLedger struct{}

func (recordingCallLedger) OpenCall(context.Context, domain.OpenCallRequest) (domain.CallRecord, error) {
	return domain.CallRecord{}, nil
}
func (recordingCallLedger) LoadCall(context.Context, domain.CallRecordID) (domain.PreparedCalls, error) {
	return domain.PreparedCalls{}, nil
}
func (recordingCallLedger) PrepareCalls(context.Context, domain.PrepareCallsRequest) (domain.PreparedCalls, error) {
	return domain.PreparedCalls{}, nil
}
func (recordingCallLedger) BeginDispatch(_ context.Context, req domain.BeginDispatchRequest) (domain.DispatchGrant, error) {
	return domain.DispatchGrant{RunID: req.RunID, ExpectedRunVersion: req.ExpectedRunVersion, StageName: req.StageName, AttemptID: req.AttemptID, CallRecordID: req.CallRecordID, AttemptCallID: req.AttemptCallID, Ordinal: 1, RetryGroup: "docker", RetryOrdinal: 1, Kind: domain.PhysicalDockerContainerCreate, Provider: "docker", RequestDigest: domain.SumBytes([]byte("request")), IdempotencyKey: req.IdempotencyKey, DispatchStartedAt: req.At, GrantDigest: domain.SumBytes([]byte("grant"))}, nil
}
func (recordingCallLedger) ResumeDispatch(context.Context, int64, domain.AttemptCallID) (domain.DispatchGrant, error) {
	return domain.DispatchGrant{}, nil
}
func (recordingCallLedger) MarkSent(context.Context, domain.DispatchGrant, time.Time) error {
	return nil
}
func (recordingCallLedger) CompletePhysical(context.Context, domain.CompletePhysicalRequest) error {
	return nil
}
func (recordingCallLedger) FinishCall(context.Context, domain.FinishCallRequest) (domain.CallTrace, error) {
	return domain.CallTrace{}, nil
}

var _ port.CallLedger = recordingCallLedger{}
