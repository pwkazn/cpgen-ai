package port

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"regexp"
	"strings"

	"cpgen/internal/domain"
)

const containerPlanSchema = "cpgen.container-plan/v1"

type SandboxProfile string

const (
	ProfileCompileV2        SandboxProfile = "compile-v2"
	ProfileExecuteMVPV2     SandboxProfile = "execute-mvp-v2"
	ProfileExecuteReleaseV2 SandboxProfile = "execute-release-v2"
)

func (v SandboxProfile) Valid() bool {
	switch v {
	case ProfileCompileV2, ProfileExecuteMVPV2, ProfileExecuteReleaseV2:
		return true
	default:
		return false
	}
}

func (v *SandboxProfile) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "SandboxProfile", func(raw string) bool { return SandboxProfile(raw).Valid() }, (*string)(v))
}

type ResourceKind string

const (
	ResourceContainer ResourceKind = "CONTAINER"
	ResourceVolume    ResourceKind = "VOLUME"
	ResourceCgroup    ResourceKind = "CGROUP"
)

func (v ResourceKind) Valid() bool {
	return v == ResourceContainer || v == ResourceVolume || v == ResourceCgroup
}

func (v *ResourceKind) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "ResourceKind", func(raw string) bool { return ResourceKind(raw).Valid() }, (*string)(v))
}

type ResourceRole string

const (
	ResourceImport        ResourceRole = "IMPORT"
	ResourceKeeper        ResourceRole = "KEEPER"
	ResourceTarget        ResourceRole = "TARGET"
	ResourceExport        ResourceRole = "EXPORT"
	ResourceInput         ResourceRole = "INPUT"
	ResourceOutput        ResourceRole = "OUTPUT"
	ResourceReleaseParent ResourceRole = "RELEASE_PARENT"
)

func (v ResourceRole) Valid() bool {
	switch v {
	case ResourceImport, ResourceKeeper, ResourceTarget, ResourceExport,
		ResourceInput, ResourceOutput, ResourceReleaseParent:
		return true
	default:
		return false
	}
}

func (v *ResourceRole) UnmarshalJSON(data []byte) error {
	return unmarshalEnum(data, "ResourceRole", func(raw string) bool { return ResourceRole(raw).Valid() }, (*string)(v))
}

type PlannedResource struct {
	Ordinal              int                 `json:"ordinal"`
	Kind                 ResourceKind        `json:"kind"`
	Role                 ResourceRole        `json:"role"`
	DeterministicName    string              `json:"deterministic_name"`
	ExpectedLabelsDigest domain.Digest       `json:"expected_labels_digest"`
	CreateCallOrdinal    *int                `json:"create_call_ordinal,omitempty"`
	CgroupRelativePath   *domain.SafeRelPath `json:"cgroup_relative_path,omitempty"`
	CreationNonce        *string             `json:"creation_nonce,omitempty"`
}

type ContainerPlan struct {
	PlanDigest           domain.Digest     `json:"plan_digest"`
	EngineIdentityDigest domain.Digest     `json:"engine_identity_digest"`
	Resources            []PlannedResource `json:"resources"`
	TransferBytesMax     int64             `json:"transfer_bytes_max"`
}

func NewContainerPlan(engineIdentity domain.Digest, resources []PlannedResource, transferBytesMax int64) (ContainerPlan, error) {
	plan := ContainerPlan{
		EngineIdentityDigest: engineIdentity,
		Resources:            clonePlannedResources(resources),
		TransferBytesMax:     transferBytesMax,
	}
	if err := plan.validateShape(); err != nil {
		return ContainerPlan{}, err
	}
	plan.PlanDigest = plan.canonicalDigest()
	return plan, nil
}

func (p ContainerPlan) Clone() ContainerPlan {
	p.Resources = clonePlannedResources(p.Resources)
	return p
}

func (p ContainerPlan) Validate() error {
	if err := p.validateShape(); err != nil {
		return err
	}
	if err := p.PlanDigest.Validate(); err != nil {
		return fmt.Errorf("plan digest: %w", err)
	}
	if got := p.canonicalDigest(); got != p.PlanDigest {
		return fmt.Errorf("plan digest mismatch: got %q, computed %q", p.PlanDigest, got)
	}
	return nil
}

var deterministicResourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func (p ContainerPlan) validateShape() error {
	if err := p.EngineIdentityDigest.Validate(); err != nil {
		return fmt.Errorf("engine identity digest: %w", err)
	}
	if p.TransferBytesMax < 0 {
		return fmt.Errorf("transfer byte limit must be non-negative")
	}
	if len(p.Resources) == 0 {
		if p.TransferBytesMax != 0 {
			return fmt.Errorf("empty Engine-ping plan must have a zero transfer limit")
		}
		return nil
	}

	names := make(map[string]struct{}, len(p.Resources))
	containerRoles := make([]ContainerRole, 0, len(p.Resources))
	nextCreateCall := 0
	for index, resource := range p.Resources {
		if resource.Ordinal != index {
			return fmt.Errorf("resource ordinal %d at index %d is not contiguous", resource.Ordinal, index)
		}
		if !resource.Kind.Valid() {
			return fmt.Errorf("resource %d has invalid kind %q", index, resource.Kind)
		}
		if !resource.Role.Valid() {
			return fmt.Errorf("resource %d has invalid role %q", index, resource.Role)
		}
		if !deterministicResourceName.MatchString(resource.DeterministicName) {
			return fmt.Errorf("resource %d has invalid deterministic name %q", index, resource.DeterministicName)
		}
		if _, exists := names[resource.DeterministicName]; exists {
			return fmt.Errorf("duplicate deterministic resource name %q", resource.DeterministicName)
		}
		names[resource.DeterministicName] = struct{}{}
		if err := resource.ExpectedLabelsDigest.Validate(); err != nil {
			return fmt.Errorf("resource %d labels digest: %w", index, err)
		}

		switch resource.Kind {
		case ResourceContainer:
			role, err := resource.Role.containerRole()
			if err != nil {
				return fmt.Errorf("resource %d: %w", index, err)
			}
			if resource.CreateCallOrdinal == nil || *resource.CreateCallOrdinal != nextCreateCall {
				return fmt.Errorf("resource %d must map to create call ordinal %d", index, nextCreateCall)
			}
			if resource.CgroupRelativePath != nil || resource.CreationNonce != nil {
				return fmt.Errorf("container resource %d cannot carry cgroup identity fields", index)
			}
			containerRoles = append(containerRoles, role)
			nextCreateCall++
		case ResourceVolume:
			if resource.Role != ResourceInput && resource.Role != ResourceOutput {
				return fmt.Errorf("volume resource %d has incompatible role %q", index, resource.Role)
			}
			if resource.CreateCallOrdinal != nil || resource.CgroupRelativePath != nil || resource.CreationNonce != nil {
				return fmt.Errorf("volume resource %d carries container or cgroup identity fields", index)
			}
		case ResourceCgroup:
			if resource.Role != ResourceReleaseParent {
				return fmt.Errorf("cgroup resource %d has incompatible role %q", index, resource.Role)
			}
			if resource.CreateCallOrdinal != nil || resource.CgroupRelativePath == nil || resource.CreationNonce == nil {
				return fmt.Errorf("cgroup resource %d has incomplete identity fields", index)
			}
			if err := resource.CgroupRelativePath.Validate(); err != nil {
				return fmt.Errorf("cgroup resource %d path: %w", index, err)
			}
			if *resource.CreationNonce == "" || strings.IndexFunc(*resource.CreationNonce, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
				return fmt.Errorf("cgroup resource %d has invalid creation nonce", index)
			}
		}
	}
	return validateContainerRoleOrder(containerRoles)
}

func validateContainerRoleOrder(roles []ContainerRole) error {
	index := 0
	for index < len(roles) && roles[index] == ContainerImport {
		index++
	}
	hasKeeper := index < len(roles) && roles[index] == ContainerKeeper
	if hasKeeper {
		index++
	}
	if index >= len(roles) || roles[index] != ContainerTarget {
		return fmt.Errorf("container plan must contain exactly one TARGET after IMPORT and optional KEEPER")
	}
	index++
	hasExport := index < len(roles) && roles[index] == ContainerExport
	if hasExport {
		index++
	}
	if index != len(roles) {
		return fmt.Errorf("container roles must follow IMPORT* -> KEEPER? -> TARGET -> EXPORT?")
	}
	if hasKeeper != hasExport {
		return fmt.Errorf("KEEPER and EXPORT must either both be present or both be absent")
	}
	return nil
}

func (r ResourceRole) containerRole() (ContainerRole, error) {
	switch r {
	case ResourceImport:
		return ContainerImport, nil
	case ResourceKeeper:
		return ContainerKeeper, nil
	case ResourceTarget:
		return ContainerTarget, nil
	case ResourceExport:
		return ContainerExport, nil
	default:
		return "", fmt.Errorf("role %q is not valid for a container", r)
	}
}

func clonePlannedResources(resources []PlannedResource) []PlannedResource {
	if resources == nil {
		return nil
	}
	clone := make([]PlannedResource, len(resources))
	for index, resource := range resources {
		clone[index] = resource
		if resource.CreateCallOrdinal != nil {
			value := *resource.CreateCallOrdinal
			clone[index].CreateCallOrdinal = &value
		}
		if resource.CgroupRelativePath != nil {
			value := *resource.CgroupRelativePath
			clone[index].CgroupRelativePath = &value
		}
		if resource.CreationNonce != nil {
			value := *resource.CreationNonce
			clone[index].CreationNonce = &value
		}
	}
	return clone
}

func (p ContainerPlan) canonicalDigest() domain.Digest {
	var encoded bytes.Buffer
	writeString := func(value string) {
		var size [4]byte
		binary.BigEndian.PutUint32(size[:], uint32(len(value)))
		encoded.Write(size[:])
		encoded.WriteString(value)
	}
	writeInt64 := func(value int64) {
		var raw [8]byte
		binary.BigEndian.PutUint64(raw[:], uint64(value))
		encoded.Write(raw[:])
	}
	writeOptionalString := func(value *string) {
		if value == nil {
			encoded.WriteByte(0)
			return
		}
		encoded.WriteByte(1)
		writeString(*value)
	}

	writeString(containerPlanSchema)
	writeString(string(p.EngineIdentityDigest))
	writeInt64(p.TransferBytesMax)
	var resourceCount [4]byte
	binary.BigEndian.PutUint32(resourceCount[:], uint32(len(p.Resources)))
	encoded.Write(resourceCount[:])
	for _, resource := range p.Resources {
		writeInt64(int64(resource.Ordinal))
		writeString(string(resource.Kind))
		writeString(string(resource.Role))
		writeString(resource.DeterministicName)
		writeString(string(resource.ExpectedLabelsDigest))
		if resource.CreateCallOrdinal == nil {
			encoded.WriteByte(0)
		} else {
			encoded.WriteByte(1)
			writeInt64(int64(*resource.CreateCallOrdinal))
		}
		if resource.CgroupRelativePath == nil {
			writeOptionalString(nil)
		} else {
			value := string(*resource.CgroupRelativePath)
			writeOptionalString(&value)
		}
		writeOptionalString(resource.CreationNonce)
	}
	return domain.SumBytes(encoded.Bytes())
}

const (
	CapabilityDirectPID1          = "direct_pid1"
	CapabilityNonRoot             = "non_root"
	CapabilityCapDropAll          = "cap_drop_all"
	CapabilityNoNewPrivileges     = "no_new_privileges"
	CapabilityReadOnlyRootFS      = "read_only_rootfs"
	CapabilityNetworkNone         = "network_none"
	CapabilityMemoryLimit         = "memory_limit"
	CapabilityMemorySwapDisabled  = "memory_swap_disabled"
	CapabilityPIDsLimit           = "pids_limit"
	CapabilityAttach              = "attach"
	CapabilityLogConfigNone       = "log_config_none"
	CapabilityVolumeTransfer      = "volume_transfer"
	CapabilityDetachedWatchdog    = "detached_watchdog"
	CapabilityReleaseCgroupParent = "release_cgroup_parent"
)

type CapabilitySnapshot struct {
	Profile              SandboxProfile  `json:"profile"`
	EngineIdentityDigest domain.Digest   `json:"engine_identity_digest"`
	EndpointDigest       domain.Digest   `json:"endpoint_digest"`
	ServerOS             string          `json:"server_os"`
	APIVersion           string          `json:"api_version"`
	CgroupVersion        int             `json:"cgroup_version"`
	Flags                map[string]bool `json:"flags"`
	BuilderImageDigest   domain.Digest   `json:"builder_image_digest"`
	RuntimeImageDigest   domain.Digest   `json:"runtime_image_digest"`
	TransferImageDigest  domain.Digest   `json:"transfer_image_digest"`
	ExecutionProtocol    string          `json:"execution_protocol"`
}

func RequiredCapabilityFlags(profile SandboxProfile) map[string]bool {
	flags := map[string]bool{
		CapabilityDirectPID1:         true,
		CapabilityNonRoot:            true,
		CapabilityCapDropAll:         true,
		CapabilityNoNewPrivileges:    true,
		CapabilityReadOnlyRootFS:     true,
		CapabilityNetworkNone:        true,
		CapabilityMemoryLimit:        true,
		CapabilityMemorySwapDisabled: true,
		CapabilityPIDsLimit:          true,
		CapabilityAttach:             true,
		CapabilityLogConfigNone:      true,
		CapabilityVolumeTransfer:     true,
		CapabilityDetachedWatchdog:   true,
	}
	if profile == ProfileExecuteReleaseV2 {
		flags[CapabilityReleaseCgroupParent] = true
	}
	return flags
}

func (s CapabilitySnapshot) Validate(profile SandboxProfile) error {
	if !profile.Valid() || s.Profile != profile {
		return fmt.Errorf("capability profile %q does not match expected %q", s.Profile, profile)
	}
	for name, digest := range map[string]domain.Digest{
		"engine identity": s.EngineIdentityDigest,
		"endpoint":        s.EndpointDigest,
		"builder image":   s.BuilderImageDigest,
		"runtime image":   s.RuntimeImageDigest,
		"transfer image":  s.TransferImageDigest,
	} {
		if err := digest.Validate(); err != nil {
			return fmt.Errorf("%s digest: %w", name, err)
		}
	}
	if s.ServerOS != "linux" {
		return fmt.Errorf("Docker server OS must be linux, got %q", s.ServerOS)
	}
	if s.APIVersion == "" {
		return fmt.Errorf("Docker API version is required")
	}
	if s.CgroupVersion != 2 {
		return fmt.Errorf("cgroup v2 is required")
	}
	if s.ExecutionProtocol != "docker-direct-v2" {
		return fmt.Errorf("unsupported execution protocol %q", s.ExecutionProtocol)
	}
	for flag := range RequiredCapabilityFlags(profile) {
		if !s.Flags[flag] {
			return fmt.Errorf("mandatory capability %q is missing", flag)
		}
	}
	return nil
}

type ProbeAuthorizationIdentity struct {
	LogicalOperationID   string
	RunID                domain.RunID
	AttemptID            domain.AttemptID
	SandboxExecutionID   domain.SandboxExecutionID
	EngineIdentityDigest domain.Digest
	OwnerID              domain.OwnerID
	LeaseEpoch           int64
	ScopeDigest          domain.Digest
	PlanDigest           domain.Digest
}

func (i ProbeAuthorizationIdentity) Validate() error {
	if i.LogicalOperationID == "" {
		return fmt.Errorf("logical operation id is required")
	}
	if err := i.RunID.Validate(); err != nil {
		return err
	}
	if err := i.AttemptID.Validate(); err != nil {
		return err
	}
	if i.SandboxExecutionID != "" {
		if err := i.SandboxExecutionID.Validate(); err != nil {
			return err
		}
		if err := i.EngineIdentityDigest.Validate(); err != nil {
			return fmt.Errorf("engine identity digest: %w", err)
		}
	} else {
		if err := i.OwnerID.Validate(); err != nil {
			return err
		}
		if i.LeaseEpoch <= 0 {
			return fmt.Errorf("lease epoch must be positive")
		}
	}
	if err := i.ScopeDigest.Validate(); err != nil {
		return fmt.Errorf("scope digest: %w", err)
	}
	if err := i.PlanDigest.Validate(); err != nil {
		return fmt.Errorf("plan digest: %w", err)
	}
	return nil
}

type ProbeClaimStore interface {
	ClaimEnginePing(context.Context, ProbeAuthorizationIdentity) (domain.AttemptCallID, error)
	ClaimContainer(context.Context, ProbeAuthorizationIdentity, int, ContainerRole) (domain.AttemptCallID, error)
	AbortRemaining(context.Context, ProbeAuthorizationIdentity) error
}
