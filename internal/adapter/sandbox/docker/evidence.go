package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const ExecutionRecordSchemaVersion domain.SchemaVersion = "cpgen.execution-record/v1"

const executionRecordMaxBytes int64 = 64 << 10

// ExecutionRecord is immutable by convention: Runner builds and validates a
// complete value, serializes that value, and never gives the target a writer
// or writable mount for it.
type ExecutionRecord struct {
	SchemaVersion        domain.SchemaVersion   `json:"schema_version"`
	Protocol             string                 `json:"protocol"`
	Started              bool                   `json:"started"`
	Stopped              bool                   `json:"stopped"`
	EvidenceComplete     bool                   `json:"evidence_complete"`
	Outcome              domain.ProcessOutcome  `json:"process_outcome"`
	ExitCode             *int                   `json:"exit_code,omitempty"`
	Signal               *string                `json:"signal,omitempty"`
	WallTime             time.Duration          `json:"wall_time_ns"`
	CPUTime              *time.Duration         `json:"cpu_time_ns,omitempty"`
	PeakRSSBytes         *int64                 `json:"peak_rss_bytes,omitempty"`
	MeasurementProfile   string                 `json:"measurement_profile"`
	StdoutBytes          int64                  `json:"stdout_bytes"`
	StderrBytes          int64                  `json:"stderr_bytes"`
	StdoutTruncated      bool                   `json:"stdout_truncated"`
	StderrTruncated      bool                   `json:"stderr_truncated"`
	OOMKilled            bool                   `json:"oom_killed"`
	HardDeadline         bool                   `json:"hard_deadline"`
	TriggerCause         *domain.ExecutionCause `json:"trigger_cause,omitempty"`
	EngineIdentityDigest domain.Digest          `json:"engine_identity_digest"`
	PlanDigest           domain.Digest          `json:"plan_digest"`
	TargetCallID         domain.AttemptCallID   `json:"target_call_id"`
}

func (r ExecutionRecord) Validate() error {
	if r.SchemaVersion != ExecutionRecordSchemaVersion {
		return fmt.Errorf("execution record schema version must be %q", ExecutionRecordSchemaVersion)
	}
	if r.Protocol != ExecutionProtocolDockerDirectV2 {
		return fmt.Errorf("execution protocol must be %q", ExecutionProtocolDockerDirectV2)
	}
	if !r.Outcome.Valid() {
		return fmt.Errorf("invalid process outcome %q", r.Outcome)
	}
	if !r.Stopped {
		return fmt.Errorf("execution record requires a stopped proof")
	}
	if r.Outcome != domain.ProcessInfraError && !r.Started {
		return fmt.Errorf("non-infrastructure process outcome requires an explicit Start")
	}
	if r.Outcome == domain.ProcessExited && r.ExitCode == nil {
		return fmt.Errorf("EXITED execution record requires an exit code")
	}
	if r.Outcome != domain.ProcessExited && r.ExitCode != nil && r.Outcome == domain.ProcessSignaled {
		return fmt.Errorf("SIGNALED execution record cannot use an exit code as signal evidence")
	}
	if r.Outcome == domain.ProcessSignaled && (r.Signal == nil || *r.Signal == "") {
		return fmt.Errorf("SIGNALED execution record requires signal evidence")
	}
	if r.WallTime < 0 || (r.CPUTime != nil && *r.CPUTime < 0) || (r.PeakRSSBytes != nil && *r.PeakRSSBytes < 0) ||
		r.StdoutBytes < 0 || r.StderrBytes < 0 {
		return fmt.Errorf("execution measurements must be non-negative")
	}
	if r.MeasurementProfile == "" {
		return fmt.Errorf("measurement profile is required")
	}
	if err := r.EngineIdentityDigest.Validate(); err != nil {
		return fmt.Errorf("Engine identity digest: %w", err)
	}
	if err := r.PlanDigest.Validate(); err != nil {
		return fmt.Errorf("plan digest: %w", err)
	}
	if err := r.TargetCallID.Validate(); err != nil {
		return fmt.Errorf("target call ID: %w", err)
	}
	if r.TriggerCause != nil && !r.TriggerCause.Valid() {
		return fmt.Errorf("invalid trigger cause %q", *r.TriggerCause)
	}
	return nil
}

type processArtifacts struct {
	stdout    *preparedArtifact
	stderr    *preparedArtifact
	execution *preparedArtifact
}

func (r *Runner) prepareProcessArtifacts(ctx context.Context, op *operation, prefix domain.SafeRelPath, inputDigest domain.Digest, stdoutLimit, stderrLimit int64) (*processArtifacts, error) {
	if err := prefix.Validate(); err != nil {
		return nil, err
	}
	if err := inputDigest.Validate(); err != nil {
		return nil, err
	}
	if stdoutLimit <= 0 || stderrLimit <= 0 {
		return nil, fmt.Errorf("process stream limits must be positive")
	}
	provenance := domain.ProvenanceCandidate{
		SchemaVersion: domain.DomainSchemaVersion, Producer: ExecutionProtocolDockerDirectV2, InputDigest: &inputDigest,
	}
	prepare := func(declaration port.ArtifactDeclaration) (*preparedArtifact, error) {
		if err := declaration.Validate(); err != nil {
			return nil, err
		}
		writer, err := r.artifacts.Prepare(ctx, declaration)
		if err != nil {
			return nil, err
		}
		artifact := &preparedArtifact{declaration: declaration, writer: writer}
		op.writers = append(op.writers, artifact)
		return artifact, nil
	}
	stdout, err := prepare(port.ArtifactDeclaration{
		MediaType: "application/octet-stream", Role: domain.ArtifactStdout,
		LogicalPath: domain.SafeRelPath(string(prefix) + "/stdout"), MaxBytes: stdoutLimit, Provenance: provenance,
	})
	if err != nil {
		return nil, err
	}
	stderr, err := prepare(port.ArtifactDeclaration{
		MediaType: "application/octet-stream", Role: domain.ArtifactStderr,
		LogicalPath: domain.SafeRelPath(string(prefix) + "/stderr"), MaxBytes: stderrLimit, Provenance: provenance,
	})
	if err != nil {
		return nil, err
	}
	execution, err := prepare(port.ArtifactDeclaration{
		MediaType: "application/vnd.cpgen.execution-record+json", Role: domain.ArtifactEvidence,
		LogicalPath: domain.SafeRelPath(string(prefix) + "/execution.json"), MaxBytes: executionRecordMaxBytes, Provenance: provenance,
	})
	if err != nil {
		return nil, err
	}
	return &processArtifacts{stdout: stdout, stderr: stderr, execution: execution}, nil
}

func (a *processArtifacts) limiters() (*outputLimiter, *outputLimiter, error) {
	stdout, err := newOutputLimiter(a.stdout.writer, a.stdout.declaration.MaxBytes, func() {})
	if err != nil {
		return nil, nil, err
	}
	stderr, err := newOutputLimiter(a.stderr.writer, a.stderr.declaration.MaxBytes, func() {})
	if err != nil {
		return nil, nil, err
	}
	return stdout, stderr, nil
}

func (a *processArtifacts) finalize(ctx context.Context, record ExecutionRecord) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if _, err := finalizePreparedArtifact(ctx, a.stdout, record.TargetCallID); err != nil {
		return err
	}
	if _, err := finalizePreparedArtifact(ctx, a.stderr, record.TargetCallID); err != nil {
		return err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode execution record: %w", err)
	}
	if int64(len(encoded)) > a.execution.declaration.MaxBytes {
		return fmt.Errorf("execution record exceeds %d bytes", a.execution.declaration.MaxBytes)
	}
	if _, err := a.execution.writer.Write(encoded); err != nil {
		return fmt.Errorf("write execution record: %w", err)
	}
	_, err = finalizePreparedArtifact(ctx, a.execution, record.TargetCallID)
	return err
}

func finalizePreparedArtifact(ctx context.Context, artifact *preparedArtifact, callID domain.AttemptCallID) (*domain.PendingArtifact, error) {
	pending, err := artifact.writer.Finalize(ctx)
	if err != nil {
		return nil, err
	}
	artifact.finalized = true
	pending.CallID = callID
	if pending.MediaType != artifact.declaration.MediaType || pending.Role != artifact.declaration.Role ||
		pending.LogicalPath != artifact.declaration.LogicalPath || pending.Provenance != artifact.declaration.Provenance {
		return nil, fmt.Errorf("finalized artifact %q does not match its declaration", artifact.declaration.LogicalPath)
	}
	if err := pending.Validate(); err != nil {
		return nil, err
	}
	artifact.pending = pending
	return &artifact.pending, nil
}

func finalizeProcessArtifacts(artifacts *processArtifacts, record ExecutionRecord, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return artifacts.finalize(ctx, record)
}
