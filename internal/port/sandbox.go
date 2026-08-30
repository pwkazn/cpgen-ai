package port

import (
	"context"
	"fmt"
	"slices"
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
	return nil
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

type RoleArgs struct{}

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
	for name, artifact := range map[string]*domain.PendingArtifact{"program": r.Program, "stdout": r.Stdout, "stderr": r.Stderr} {
		if artifact != nil {
			if err := artifact.Validate(); err != nil {
				return fmt.Errorf("%s artifact: %w", name, err)
			}
		}
	}
	return nil
}

type RunResult struct {
	CallTrace domain.CallTrace        `json:"call_trace"`
	Outcome   domain.ProcessOutcome   `json:"outcome"`
	ExitCode  *int                    `json:"exit_code,omitempty"`
	Signal    *string                 `json:"signal,omitempty"`
	Metrics   ProcessMetrics          `json:"metrics"`
	Stdout    *domain.PendingArtifact `json:"stdout,omitempty"`
	Stderr    *domain.PendingArtifact `json:"stderr,omitempty"`
	Details   map[string]string       `json:"details,omitempty"`
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
	for name, artifact := range map[string]*domain.PendingArtifact{"stdout": r.Stdout, "stderr": r.Stderr} {
		if artifact != nil {
			if err := artifact.Validate(); err != nil {
				return fmt.Errorf("%s artifact: %w", name, err)
			}
		}
	}
	return nil
}

type DispatchAuthorization interface {
	CallID() domain.AttemptCallID
	RunID() domain.RunID
	AttemptID() domain.AttemptID
	OwnerID() domain.OwnerID
	LeaseEpoch() int64
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
}

type SandboxDispatchAuthorization interface {
	LogicalOperationID() string
	RunID() domain.RunID
	AttemptID() domain.AttemptID
	OwnerID() domain.OwnerID
	LeaseEpoch() int64
	ScopeDigest() domain.Digest
	PlanDigest() domain.Digest
	ContainerPlan() ContainerPlan
	ClaimEnginePing(ctx context.Context) (DispatchAuthorization, error)
	ClaimNextContainer(ctx context.Context, role ContainerRole) (ContainerDispatchGrant, error)
	sealSandboxDispatchAuthorization()
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
