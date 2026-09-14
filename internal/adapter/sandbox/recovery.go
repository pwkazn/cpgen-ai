package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	docker "cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/adapter/storage/blob"
	"cpgen/internal/adapter/storage/sqlite"
	artifact "cpgen/internal/artifact"
	"cpgen/internal/clock"
	"cpgen/internal/domain"
	durable "cpgen/internal/execution"
	"cpgen/internal/port"
)

// ErrCleanupPending means the exact external sandbox resources are still
// unresolved. It is deliberately separate from a generic host failure so the
// CLI can report exit code 10 while preserving the RUNNING projection.
var ErrCleanupPending = errors.New("sandbox cleanup is pending")

var ErrSandboxEvidenceIncomplete = errors.New("interrupted sandbox has no complete execution result")

// recoverSandboxResult reconstructs a result only after the exact execution
// has durable CLEANED proof. It never grants creation or repeats a process.
// Missing final receipt bytes do not invalidate already retained process and
// program artifacts; missing execution evidence cannot be guessed from cleanup.
func recoverSandboxResult(ctx context.Context, session *Session, identity port.SandboxAuthorizationIdentity, plan port.ContainerPlan, sink *ArtifactSink, request any) (any, bool, error) {
	c := session.config
	_, err := c.Store.GetSandboxExecution(ctx, identity.SandboxExecutionID)
	if errors.Is(err, sqlite.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if err := VerifyCleaned(ctx, c.Store, identity, plan); err != nil {
		return nil, false, fmt.Errorf("%w: %v", ErrCleanupPending, err)
	}
	prefix := "sandbox/" + string(identity.SandboxExecutionID) + "/"
	phase, inputDigest := "", domain.Digest("")
	stdoutLimit, stderrLimit := int64(1<<20), int64(1<<20)
	switch r := request.(type) {
	case port.CompileRequest:
		phase, inputDigest = "compile", r.SourceBundle.Digest
	case port.RunRequest:
		phase, inputDigest, stdoutLimit, stderrLimit = "run", r.Program.Digest, r.Limits.StdoutBytes, r.Limits.StderrBytes
	default:
		return nil, false, errors.New("unsupported sandbox recovery request")
	}
	read := func(path string, role domain.ArtifactRole, media string, max int64) (*domain.PendingArtifact, error) {
		pending, found, err := sink.ReadDeclared(ctx, domain.SafeRelPath(prefix+path))
		if err != nil {
			return nil, err
		}
		if !found {
			return nil, fmt.Errorf("%w: missing %s", ErrSandboxEvidenceIncomplete, path)
		}
		if pending.Role != role || pending.MediaType != media || pending.Blob.Size > max || pending.Provenance.SchemaVersion != domain.DomainSchemaVersion || pending.Provenance.Producer != docker.ExecutionProtocolDockerDirectV2 || pending.Provenance.InputDigest == nil || *pending.Provenance.InputDigest != inputDigest {
			return nil, errors.New("sandbox recovery artifact differs from its exact request")
		}
		return &pending, nil
	}
	stdout, err := read(phase+"/stdout", domain.ArtifactStdout, "application/octet-stream", stdoutLimit)
	if err != nil {
		return nil, false, err
	}
	stderr, err := read(phase+"/stderr", domain.ArtifactStderr, "application/octet-stream", stderrLimit)
	if err != nil {
		return nil, false, err
	}
	execution, err := read(phase+"/execution.json", domain.ArtifactEvidence, "application/vnd.cpgen.execution-record+json", 64<<10)
	if err != nil {
		return nil, false, err
	}
	raw, err := artifact.ReadVerified(ctx, c.Blobs, execution.Blob, 64<<10)
	if err != nil {
		return nil, false, err
	}
	var record docker.ExecutionRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, false, err
	}
	if record.Validate() != nil || !record.EvidenceComplete || record.PlanDigest != plan.PlanDigest || record.EngineIdentityDigest != c.EngineIdentity {
		return nil, false, ErrSandboxEvidenceIncomplete
	}
	succeeded := record.Outcome == domain.ProcessExited && record.ExitCode != nil && *record.ExitCode == 0
	ledger, err := durable.NewRunLedger(c.Store, identity.RunID, identity.StageName, identity.AttemptID)
	if err != nil {
		return nil, false, err
	}
	trace := domain.CallTrace{LogicalOperationID: identity.LogicalOperationID, DispatchKind: domain.DispatchDispatched, ResultAttemptCallID: &record.TargetCallID}
	var bindings []port.SandboxResourceCall
	targetFound := false
	for _, resource := range plan.Resources {
		if resource.Kind == port.ResourceCgroup {
			continue
		}
		id := domain.CallRecordID(durable.MutationID("callrec", identity.SandboxExecutionID, resource.Ordinal))
		physicalID := domain.AttemptCallID(durable.MutationID("call", id))
		p, err := ledger.LoadCall(ctx, id)
		if err != nil {
			return nil, false, err
		}
		digest, err := port.SandboxResourceRequestDigest(identity, plan, resource.Ordinal)
		if err != nil {
			return nil, false, err
		}
		if p.Call.RunID != identity.RunID || p.Call.StageName != identity.StageName || p.Call.AttemptID != identity.AttemptID || p.Call.Kind != identity.Kind || p.Call.Provider != "docker" || p.Call.RequestDigest != digest || p.Call.PolicyDigest != identity.ScopeDigest || p.Call.LogicalOperationID != port.SandboxResourceLogicalOperation(identity, resource.Ordinal) || len(p.PhysicalCalls) != 1 || p.PhysicalCalls[0].ID != physicalID {
			return nil, false, errors.New("sandbox recovery call binding differs")
		}
		physical := p.PhysicalCalls[0]
		bindings = append(bindings, port.SandboxResourceCall{ResourceOrdinal: resource.Ordinal, CallRecordID: id, AttemptCallID: physicalID})
		if !succeeded && resource.Role == port.ResourceExport && (physical.State == domain.PhysicalPrepared || physical.State == domain.PhysicalAbortedNoDispatch) {
			continue
		}
		if physical.State != domain.PhysicalCompleted || physical.Outcome == nil || *physical.Outcome != domain.PhysicalOutcomeSuccess || physical.Failure != nil {
			return nil, false, ErrSandboxEvidenceIncomplete
		}
		trace.PhysicalAttemptCallIDs = append(trace.PhysicalAttemptCallIDs, physicalID)
		if resource.Role == port.ResourceTarget {
			if record.TargetCallID != physicalID {
				return nil, false, errors.New("sandbox recovery target call differs")
			}
			targetFound = true
		}
	}
	if !targetFound || trace.Validate() != nil {
		return nil, false, ErrSandboxEvidenceIncomplete
	}
	var result any
	switch r := request.(type) {
	case port.CompileRequest:
		compiled := port.CompileResult{CallTrace: trace, Outcome: domain.CompileInfraError, Stdout: stdout, Stderr: stderr, Execution: execution, Details: map[string]string{"process_outcome": string(record.Outcome)}}
		if record.Outcome == domain.ProcessExited && record.ExitCode != nil {
			compiled.Outcome = domain.CompileCE
			if succeeded {
				compiled.Program, err = read("program/main", domain.ArtifactProgram, "application/vnd.cpgen.executable", r.Limits.OutputBytes)
				if err != nil {
					return nil, false, err
				}
				compiled.Outcome = domain.CompileOK
			}
		}
		if err := compiled.Validate(); err != nil {
			return nil, false, err
		}
		result = compiled
	case port.RunRequest:
		run := port.RunResult{CallTrace: trace, Outcome: record.Outcome, Signal: record.Signal, Metrics: port.ProcessMetrics{WallTime: record.WallTime, CPUTime: record.CPUTime, PeakRSSBytes: record.PeakRSSBytes, OOMKilled: record.OOMKilled}, Stdout: stdout, Stderr: stderr, Execution: execution, Outputs: []domain.PendingArtifact{}}
		if record.Outcome == domain.ProcessExited {
			run.ExitCode = record.ExitCode
		}
		if succeeded {
			for _, output := range r.Outputs {
				artifact, err := read(string(output.Path), domain.ArtifactOutput, "application/octet-stream", output.MaxBytes)
				if err != nil {
					return nil, false, err
				}
				run.Outputs = append(run.Outputs, *artifact)
			}
		}
		if err := run.Validate(); err != nil {
			return nil, false, err
		}
		result = run
	}
	if err := finishSandboxResourceCalls(ctx, ledger, c.Clock, identity, bindings); err != nil {
		return nil, false, err
	}
	return result, true, nil
}

// ReconcileStageCalls settles retained calls only after the caller has verified
// run admission and exact Docker cleanup. It never dispatches new work.
func ReconcileStageCalls(ctx context.Context, callStore durable.Store, blobs *blob.Store, source clock.Clock, identity port.SandboxAuthorizationIdentity) error {
	runID := identity.RunID
	store, ok := callStore.(interface {
		ReadAttemptSandboxCalls(context.Context, domain.RunID, domain.StageName, domain.AttemptID) ([]domain.CallRecord, error)
	})
	if !ok {
		return errors.New("solution cleanup requires scoped call history")
	}
	calls, err := store.ReadAttemptSandboxCalls(ctx, runID, identity.StageName, identity.AttemptID)
	if err != nil {
		return err
	}
	ledger, err := durable.NewRunLedger(callStore, runID, identity.StageName, identity.AttemptID)
	if err != nil {
		return err
	}
	for _, call := range calls {
		if call.State == domain.CallRecordTerminal {
			continue
		}
		if call.Provider != "blob" && call.Provider != "docker" {
			return errors.New("sandbox cleanup found an unsupported provider")
		}
		failure := &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
		if call.State == domain.CallRecordOpen {
			_, err := ledger.FinishCall(ctx, domain.FinishCallRequest{RunID: runID, ExpectedRunVersion: identity.ExpectedRunVersion, StageName: identity.StageName, AttemptID: identity.AttemptID, CallRecordID: call.ID, DispatchKind: domain.DispatchNone, Failure: failure, IdempotencyKey: durable.MutationID("finish", call.ID), At: source.Now().UTC()})
			if err != nil {
				return err
			}
			continue
		}
		p, err := ledger.LoadCall(ctx, call.ID)
		if err != nil {
			return err
		}
		if len(p.PhysicalCalls) != 1 {
			return errors.New("sandbox cleanup call has multiple physical identities")
		}
		physical := p.PhysicalCalls[0]
		if call.Provider == "blob" && physical.State != domain.PhysicalCompleted && physical.State != domain.PhysicalAbortedNoDispatch {
			declID := domain.ArtifactDeclarationID(durable.MutationID("decl", call.ID))
			decl, token, err := ledger.ReadArtifactWriter(ctx, declID)
			if err != nil && !errors.Is(err, sqlite.ErrNotFound) {
				return err
			}
			if err == nil {
				if token.State == domain.ArtifactWriterSealed {
					session, err := artifact.NewPreparedArtifactSession(ledger, blobs, p)
					if err != nil {
						return err
					}
					writer, err := session.Prepare(ctx, decl.ID)
					if err != nil {
						return err
					}
					if _, err := writer.Finalize(ctx); err != nil {
						return err
					}
					token.State = domain.ArtifactWriterFinalized
				}
				if token.State == domain.ArtifactWriterFinalized {
					pending, err := ledger.ReadPendingArtifact(ctx, decl.ID)
					if err != nil {
						return err
					}
					sink := &ArtifactSink{ledger: ledger, blobs: blobs, clock: source, identity: identity}
					if err := sink.complete(ctx, decl, pending); err != nil {
						return err
					}
					continue
				}
				if token.State != domain.ArtifactWriterReleased {
					if err := ledger.ReleaseArtifact(ctx, token.ID); err != nil {
						return err
					}
				}
			}
		}
		if physical.State == domain.PhysicalDispatching || physical.State == domain.PhysicalSent {
			state, outcome := domain.PhysicalUnknown, domain.PhysicalOutcomeUnknown
			failure = &domain.PortFailure{Code: domain.FailureBoundaryUnknown, Class: domain.FailureUnknown}
			if call.Provider == "blob" {
				state, outcome = domain.PhysicalAbortedNoDispatch, domain.PhysicalOutcomeNoSend
				failure = &domain.PortFailure{Code: domain.FailurePolicyRejected, Class: domain.FailureRejected}
			}
			if err := ledger.CompletePhysical(ctx, domain.CompletePhysicalRequest{RunID: runID, ExpectedRunVersion: identity.ExpectedRunVersion, StageName: identity.StageName, AttemptID: identity.AttemptID, CallRecordID: call.ID, AttemptCallID: physical.ID, State: state, Outcome: outcome, Failure: failure, IdempotencyKey: durable.MutationID("terminal_cleanup", physical.ID), At: source.Now().UTC()}); err != nil {
				return err
			}
		}
		binding := port.SandboxResourceCall{CallRecordID: call.ID, AttemptCallID: physical.ID}
		if err := finishSandboxResourceCalls(ctx, ledger, source, identity, []port.SandboxResourceCall{binding}); err != nil {
			return err
		}
	}
	return nil
}
