package port

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
	"time"

	"cpgen/internal/domain"
)

type Language string

const (
	LanguageCPP20 Language = "CPP20"
	LanguageGo    Language = "GO"
)

func (v Language) Valid() bool { return v == LanguageCPP20 || v == LanguageGo }
func (v *Language) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "Language", func(raw string) bool { return Language(raw).Valid() }, (*string)(v))
}

type ProgramRole string

const (
	RoleSolution  ProgramRole = "SOLUTION"
	RoleBrute     ProgramRole = "BRUTE"
	RoleGenerator ProgramRole = "GENERATOR"
	RoleValidator ProgramRole = "VALIDATOR"
	RoleChecker   ProgramRole = "CHECKER"
)

func (v ProgramRole) Valid() bool {
	switch v {
	case RoleSolution, RoleBrute, RoleGenerator, RoleValidator, RoleChecker:
		return true
	default:
		return false
	}
}
func (v *ProgramRole) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "ProgramRole", func(raw string) bool { return ProgramRole(raw).Valid() }, (*string)(v))
}

type ToolchainID string

func (id ToolchainID) Validate() error {
	if id == "" {
		return fmt.Errorf("toolchain id is required")
	}
	return nil
}

type SourceFile struct {
	Path domain.SafeRelPath `json:"path"`
	Blob domain.BlobRef     `json:"blob"`
}

type SourceBundleManifest struct {
	SchemaVersion domain.SchemaVersion `json:"schema_version"`
	Files         []SourceFile         `json:"files"`
	EntryPoint    domain.SafeRelPath   `json:"entry_point"`
	Digest        domain.Digest        `json:"digest"`
}

func (m SourceBundleManifest) Validate() error {
	if err := m.SchemaVersion.Validate(); err != nil {
		return err
	}
	if len(m.Files) == 0 {
		return fmt.Errorf("source bundle must contain at least one file")
	}
	if err := m.EntryPoint.Validate(); err != nil {
		return fmt.Errorf("entry point: %w", err)
	}
	if err := m.Digest.Validate(); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(m.Files))
	foundEntry := false
	for index, file := range m.Files {
		if err := file.Path.Validate(); err != nil {
			return fmt.Errorf("source file %d: %w", index, err)
		}
		if err := file.Blob.Validate(); err != nil {
			return fmt.Errorf("source file %d: %w", index, err)
		}
		key := string(file.Path)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate source path %q", file.Path)
		}
		seen[key] = struct{}{}
		foundEntry = foundEntry || file.Path == m.EntryPoint
	}
	if !foundEntry {
		return fmt.Errorf("entry point is not present in source files")
	}
	computed, err := ComputeSourceBundleDigest(m)
	if err != nil {
		return err
	}
	if computed != m.Digest {
		return fmt.Errorf("source bundle digest mismatch: got %q, computed %q", m.Digest, computed)
	}
	return nil
}

func ComputeSourceBundleDigest(m SourceBundleManifest) (domain.Digest, error) {
	if err := m.SchemaVersion.Validate(); err != nil {
		return "", err
	}
	if err := m.EntryPoint.Validate(); err != nil {
		return "", fmt.Errorf("entry point: %w", err)
	}
	if len(m.Files) == 0 {
		return "", fmt.Errorf("source bundle must contain at least one file")
	}
	files := slices.Clone(m.Files)
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	seen := make(map[domain.SafeRelPath]struct{}, len(files))
	foundEntry := false
	for index, file := range files {
		if err := file.Path.Validate(); err != nil {
			return "", fmt.Errorf("source file %d: %w", index, err)
		}
		if err := file.Blob.Validate(); err != nil {
			return "", fmt.Errorf("source file %d: %w", index, err)
		}
		if _, exists := seen[file.Path]; exists {
			return "", fmt.Errorf("duplicate source path %q", file.Path)
		}
		seen[file.Path] = struct{}{}
		foundEntry = foundEntry || file.Path == m.EntryPoint
	}
	if !foundEntry {
		return "", fmt.Errorf("entry point is not present in source files")
	}

	var encoded bytes.Buffer
	writeString := func(value string) {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(value)))
		encoded.Write(size[:])
		encoded.WriteString(value)
	}
	writeString(string(m.SchemaVersion))
	writeString(string(m.EntryPoint))
	var count [4]byte
	binary.BigEndian.PutUint32(count[:], uint32(len(files)))
	encoded.Write(count[:])
	for _, file := range files {
		writeString(string(file.Path))
		writeString(string(file.Blob.Digest))
	}
	return domain.SumBytes(encoded.Bytes()), nil
}

type CompileLimits struct {
	Time        time.Duration `json:"time"`
	MemoryBytes int64         `json:"memory_bytes"`
	PIDs        int64         `json:"pids"`
	OutputBytes int64         `json:"output_bytes"`
}

func (l CompileLimits) Validate() error {
	if l.Time <= 0 || l.MemoryBytes <= 0 || l.PIDs <= 0 || l.OutputBytes <= 0 {
		return fmt.Errorf("all compile limits must be positive")
	}
	return nil
}

type CompileRequest struct {
	Language       Language             `json:"language"`
	Role           ProgramRole          `json:"role"`
	SourceBundle   SourceBundleManifest `json:"source_bundle"`
	Toolchain      ToolchainID          `json:"toolchain"`
	Limits         CompileLimits        `json:"limits"`
	ExpectedOutput domain.SafeRelPath   `json:"expected_output"`
}

func (r CompileRequest) Validate() error {
	if !r.Language.Valid() || !r.Role.Valid() {
		return fmt.Errorf("invalid compile language or role")
	}
	if err := r.SourceBundle.Validate(); err != nil {
		return err
	}
	if err := r.Toolchain.Validate(); err != nil {
		return err
	}
	if err := r.Limits.Validate(); err != nil {
		return err
	}
	return r.ExpectedOutput.Validate()
}

type RoleArgs struct {
	GeneratorCase *GeneratorCaseArgs `json:"generator_case,omitempty"`
}

// GeneratorCaseArgs extends the seed-only legacy protocol with two closed
// application-owned arguments. It never admits arbitrary argv or shell text.
type GeneratorCaseArgs struct {
	Ordinal int                 `json:"ordinal"`
	Kind    domain.DataCaseKind `json:"kind"`
}

type InputMount struct {
	Path domain.SafeRelPath `json:"path"`
	Blob domain.BlobRef     `json:"blob"`
}

type OutputDeclaration struct {
	Path     domain.SafeRelPath `json:"path"`
	MaxBytes int64              `json:"max_bytes"`
}

type RunLimits struct {
	Time        time.Duration `json:"time"`
	MemoryBytes int64         `json:"memory_bytes"`
	PIDs        int64         `json:"pids"`
	StdoutBytes int64         `json:"stdout_bytes"`
	StderrBytes int64         `json:"stderr_bytes"`
}

func (l RunLimits) Validate() error {
	if l.Time <= 0 || l.MemoryBytes <= 0 || l.PIDs <= 0 || l.StdoutBytes <= 0 || l.StderrBytes <= 0 {
		return fmt.Errorf("all run limits must be positive")
	}
	return nil
}

type RunRequest struct {
	Role    ProgramRole         `json:"role"`
	Program domain.BlobRef      `json:"program"`
	Args    RoleArgs            `json:"args"`
	Stdin   *domain.BlobRef     `json:"stdin,omitempty"`
	Files   []InputMount        `json:"files"`
	Outputs []OutputDeclaration `json:"outputs"`
	Limits  RunLimits           `json:"limits"`
	Seed    *uint64             `json:"seed,omitempty"`
}

func (r RunRequest) Validate() error {
	if !r.Role.Valid() {
		return fmt.Errorf("invalid program role %q", r.Role)
	}
	if err := r.Program.Validate(); err != nil {
		return err
	}
	if err := r.Limits.Validate(); err != nil {
		return err
	}
	if r.Stdin != nil {
		if err := r.Stdin.Validate(); err != nil {
			return fmt.Errorf("stdin: %w", err)
		}
	}
	paths := make(map[domain.SafeRelPath]struct{}, len(r.Files)+len(r.Outputs))
	for _, input := range r.Files {
		if err := input.Path.Validate(); err != nil {
			return err
		}
		if err := input.Blob.Validate(); err != nil {
			return err
		}
		if _, exists := paths[input.Path]; exists {
			return fmt.Errorf("duplicate run path %q", input.Path)
		}
		paths[input.Path] = struct{}{}
	}
	for _, output := range r.Outputs {
		if err := output.Path.Validate(); err != nil {
			return err
		}
		if output.MaxBytes <= 0 {
			return fmt.Errorf("output max bytes must be positive")
		}
		if _, exists := paths[output.Path]; exists {
			return fmt.Errorf("duplicate run path %q", output.Path)
		}
		paths[output.Path] = struct{}{}
	}
	if r.Role == RoleGenerator && r.Seed == nil {
		return fmt.Errorf("generator seed is required")
	}
	if r.Role != RoleGenerator && r.Seed != nil {
		return fmt.Errorf("seed is only valid for generator runs")
	}
	if args := r.Args.GeneratorCase; args != nil {
		if r.Role != RoleGenerator || args.Ordinal < 1 || args.Ordinal > 12 || (args.Kind != domain.DataCaseSmall && args.Kind != domain.DataCaseBoundary && args.Kind != domain.DataCaseStress) {
			return fmt.Errorf("generator case arguments require a bounded ordinal and supported kind")
		}
	}
	return nil
}

type ProcessMetrics struct {
	WallTime     time.Duration  `json:"wall_time"`
	CPUTime      *time.Duration `json:"cpu_time,omitempty"`
	PeakRSSBytes *int64         `json:"peak_rss_bytes,omitempty"`
	OOMKilled    bool           `json:"oom_killed"`
}

type CompileResult struct {
	CallTrace domain.CallTrace        `json:"call_trace"`
	Outcome   domain.CompileOutcome   `json:"outcome"`
	Program   *domain.PendingArtifact `json:"program,omitempty"`
	Stdout    *domain.PendingArtifact `json:"stdout,omitempty"`
	Stderr    *domain.PendingArtifact `json:"stderr,omitempty"`
	Execution *domain.PendingArtifact `json:"execution,omitempty"`
	Details   map[string]string       `json:"details,omitempty"`
}

func (r CompileResult) Validate() error {
	if err := r.CallTrace.Validate(); err != nil {
		return err
	}
	if !r.Outcome.Valid() {
		return fmt.Errorf("invalid compile outcome %q", r.Outcome)
	}
	if r.Outcome == domain.CompileOK && r.Program == nil {
		return fmt.Errorf("successful compile requires a program artifact")
	}
	if r.Outcome != domain.CompileOK && r.Program != nil {
		return fmt.Errorf("failed compile cannot return a program artifact")
	}
	for name, artifact := range map[string]*domain.PendingArtifact{"program": r.Program, "stdout": r.Stdout, "stderr": r.Stderr, "execution": r.Execution} {
		if artifact != nil {
			if err := artifact.Validate(); err != nil {
				return fmt.Errorf("%s artifact: %w", name, err)
			}
		}
	}
	for name, check := range map[string]struct {
		artifact *domain.PendingArtifact
		role     domain.ArtifactRole
	}{
		"program": {r.Program, domain.ArtifactProgram}, "stdout": {r.Stdout, domain.ArtifactStdout},
		"stderr": {r.Stderr, domain.ArtifactStderr}, "execution": {r.Execution, domain.ArtifactEvidence},
	} {
		if check.artifact != nil && check.artifact.Role != check.role {
			return fmt.Errorf("%s artifact has role %q, want %q", name, check.artifact.Role, check.role)
		}
	}
	return nil
}

type RunResult struct {
	CallTrace domain.CallTrace         `json:"call_trace"`
	Outcome   domain.ProcessOutcome    `json:"outcome"`
	ExitCode  *int                     `json:"exit_code,omitempty"`
	Signal    *string                  `json:"signal,omitempty"`
	Metrics   ProcessMetrics           `json:"metrics"`
	Stdout    *domain.PendingArtifact  `json:"stdout,omitempty"`
	Stderr    *domain.PendingArtifact  `json:"stderr,omitempty"`
	Execution *domain.PendingArtifact  `json:"execution,omitempty"`
	Outputs   []domain.PendingArtifact `json:"outputs,omitempty"`
	Details   map[string]string        `json:"details,omitempty"`
}

func (r RunResult) Validate() error {
	if err := r.CallTrace.Validate(); err != nil {
		return err
	}
	if !r.Outcome.Valid() {
		return fmt.Errorf("invalid process outcome %q", r.Outcome)
	}
	if r.Outcome == domain.ProcessExited && r.ExitCode == nil {
		return fmt.Errorf("EXITED process requires an exit code")
	}
	if r.Outcome == domain.ProcessSignaled && (r.Signal == nil || *r.Signal == "") {
		return fmt.Errorf("SIGNALED process requires a signal")
	}
	if r.Metrics.WallTime < 0 || (r.Metrics.CPUTime != nil && *r.Metrics.CPUTime < 0) ||
		(r.Metrics.PeakRSSBytes != nil && *r.Metrics.PeakRSSBytes < 0) {
		return fmt.Errorf("process metrics must be non-negative")
	}
	for name, artifact := range map[string]*domain.PendingArtifact{"stdout": r.Stdout, "stderr": r.Stderr, "execution": r.Execution} {
		if artifact != nil {
			if err := artifact.Validate(); err != nil {
				return fmt.Errorf("%s artifact: %w", name, err)
			}
		}
	}
	for name, check := range map[string]struct {
		artifact *domain.PendingArtifact
		role     domain.ArtifactRole
	}{
		"stdout": {r.Stdout, domain.ArtifactStdout}, "stderr": {r.Stderr, domain.ArtifactStderr},
		"execution": {r.Execution, domain.ArtifactEvidence},
	} {
		if check.artifact != nil && check.artifact.Role != check.role {
			return fmt.Errorf("%s artifact has role %q, want %q", name, check.artifact.Role, check.role)
		}
	}
	outputPaths := make(map[domain.SafeRelPath]struct{}, len(r.Outputs))
	for index, artifact := range r.Outputs {
		if err := artifact.Validate(); err != nil {
			return fmt.Errorf("output artifact %d: %w", index, err)
		}
		if artifact.Role != domain.ArtifactOutput {
			return fmt.Errorf("output artifact %d has role %q", index, artifact.Role)
		}
		if _, exists := outputPaths[artifact.LogicalPath]; exists {
			return fmt.Errorf("duplicate output artifact path %q", artifact.LogicalPath)
		}
		outputPaths[artifact.LogicalPath] = struct{}{}
	}
	return nil
}

type DispatchAuthorization interface {
	CallID() domain.AttemptCallID
	RunID() domain.RunID
	AttemptID() domain.AttemptID
	ScopeDigest() domain.Digest
	sealDispatchAuthorization()
}

type ContainerRole string

const (
	ContainerImport ContainerRole = "IMPORT"
	ContainerKeeper ContainerRole = "KEEPER"
	ContainerTarget ContainerRole = "TARGET"
	ContainerExport ContainerRole = "EXPORT"
)

func (v ContainerRole) Valid() bool {
	return slices.Contains([]ContainerRole{ContainerImport, ContainerKeeper, ContainerTarget, ContainerExport}, v)
}
func (v *ContainerRole) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "ContainerRole", func(raw string) bool { return ContainerRole(raw).Valid() }, (*string)(v))
}

type ContainerDispatchGrant struct {
	Role ContainerRole
	Auth DispatchAuthorization
	// CallRecordID and ExpectedRunVersion bind the physical grant to the
	// already-prepared Task 4 call ledger. They are not part of the sealed
	// sandbox identity and are never emitted as Docker ownership labels.
	CallRecordID       domain.CallRecordID
	ExpectedRunVersion int64
}

// VolumeDispatchAuthorization is implemented by authorizations that carry
// prepared physical-call claims for volume creation. It is deliberately
// separate from SandboxDispatchAuthorization so legacy Slice 0 callers can
// continue to compile while a caller migrates its volume call ledger.
type VolumeDispatchAuthorization interface {
	ClaimNextVolume(context.Context, ResourceRole) (VolumeDispatchGrant, error)
}

type VolumeDispatchGrant struct {
	Role               ResourceRole
	Auth               DispatchAuthorization
	CallRecordID       domain.CallRecordID
	ExpectedRunVersion int64
	// Durable is true when the call ID came from the durable Task 4 claim
	// store. Legacy in-memory probe fixtures may still derive a compatibility
	// ID; those calls are ledger-checked but are not part of the old trace
	// surface.
	Durable bool
}

type SandboxDispatchAuthorization interface {
	LogicalOperationID() string
	RunID() domain.RunID
	AttemptID() domain.AttemptID
	ScopeDigest() domain.Digest
	PlanDigest() domain.Digest
	ContainerPlan() ContainerPlan
	ClaimEnginePing(ctx context.Context) (DispatchAuthorization, error)
	ClaimNextContainer(ctx context.Context, role ContainerRole) (ContainerDispatchGrant, error)
	AbortRemaining(ctx context.Context) error
	sealSandboxDispatchAuthorization()
}

// SandboxExecutionIdentity is implemented by current sandbox authorizations.
// It is optional on the legacy Slice 0 interface so old probe fixtures can be
// replayed while callers migrate to durable SandboxExecution identities.
type SandboxExecutionIdentity interface {
	SandboxExecutionID() domain.SandboxExecutionID
}

func SandboxExecutionIDOf(value any) (domain.SandboxExecutionID, bool) {
	identity, ok := value.(SandboxExecutionIdentity)
	if !ok {
		return "", false
	}
	return identity.SandboxExecutionID(), true
}

type DockerProbeRequest struct {
	Profile string `json:"profile"`
}

type DockerProbeResult struct {
	Capabilities CapabilitySnapshot `json:"capabilities"`
	CallTrace    domain.CallTrace   `json:"call_trace"`
}

type MeteredSandbox interface {
	Compile(ctx context.Context, request CompileRequest) (domain.MeteredOutcome[CompileResult], error)
	Run(ctx context.Context, request RunRequest) (domain.MeteredOutcome[RunResult], error)
}

type DockerSandbox interface {
	Compile(ctx context.Context, auth SandboxDispatchAuthorization, request CompileRequest) (CompileResult, error)
	Run(ctx context.Context, auth SandboxDispatchAuthorization, request RunRequest) (RunResult, error)
	Probe(ctx context.Context, auth SandboxDispatchAuthorization, request DockerProbeRequest) (DockerProbeResult, error)
}
