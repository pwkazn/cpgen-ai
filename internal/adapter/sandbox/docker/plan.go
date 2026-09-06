package docker

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

// PlanIdentity is the immutable operation identity used to derive resource
// names and ownership labels before any Docker resource is created.
type PlanIdentity struct {
	RunID                domain.RunID
	AttemptID            domain.AttemptID
	SandboxExecutionID   domain.SandboxExecutionID
	LogicalOperationID   string
	OperationNonce       string
	EngineIdentityDigest domain.Digest
}

var operationNoncePattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func (i PlanIdentity) Validate() error {
	if err := i.RunID.Validate(); err != nil {
		return err
	}
	if err := i.AttemptID.Validate(); err != nil {
		return err
	}
	if err := i.SandboxExecutionID.Validate(); err != nil {
		return err
	}
	if i.LogicalOperationID == "" || len(i.LogicalOperationID) > 256 || !utf8.ValidString(i.LogicalOperationID) || strings.IndexFunc(i.LogicalOperationID, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return fmt.Errorf("invalid logical operation ID %q", i.LogicalOperationID)
	}
	if !operationNoncePattern.MatchString(i.OperationNonce) {
		return fmt.Errorf("operation nonce must be 32 lowercase hexadecimal characters")
	}
	if err := i.EngineIdentityDigest.Validate(); err != nil {
		return fmt.Errorf("engine identity digest: %w", err)
	}
	return nil
}

func BuildCompilePlan(request port.CompileRequest, lock toolchain.Lock, identity PlanIdentity) (port.ContainerPlan, error) {
	if err := request.Validate(); err != nil {
		return port.ContainerPlan{}, fmt.Errorf("compile request: %w", err)
	}
	if err := lock.Validate(); err != nil {
		return port.ContainerPlan{}, fmt.Errorf("toolchain lock: %w", err)
	}
	if err := identity.Validate(); err != nil {
		return port.ContainerPlan{}, err
	}
	compiler, err := lockedToolchain(lock, request.Toolchain)
	if err != nil {
		return port.ContainerPlan{}, err
	}
	if compiler.Language != request.Language {
		return port.ContainerPlan{}, fmt.Errorf("toolchain %q does not compile language %q", request.Toolchain, request.Language)
	}
	if request.ExpectedOutput != compiler.OutputPath {
		return port.ContainerPlan{}, fmt.Errorf("compile output must be the locked path %q", compiler.OutputPath)
	}

	var transferBytes int64
	for _, source := range request.SourceBundle.Files {
		if err := addTransferBytes(&transferBytes, source.Blob.Size); err != nil {
			return port.ContainerPlan{}, err
		}
	}
	if err := addTransferBytes(&transferBytes, request.Limits.OutputBytes); err != nil {
		return port.ContainerPlan{}, err
	}

	resources := newResourcePlan(identity)
	resources.addVolume(port.ResourceInput)
	resources.addVolume(port.ResourceOutput)
	resources.addContainer(port.ResourceImport)
	resources.addContainer(port.ResourceKeeper)
	resources.addContainer(port.ResourceTarget)
	resources.addContainer(port.ResourceExport)
	return port.NewContainerPlan(identity.EngineIdentityDigest, resources.items, transferBytes)
}

func BuildRunPlan(request port.RunRequest, lock toolchain.Lock, identity PlanIdentity) (port.ContainerPlan, error) {
	if err := request.Validate(); err != nil {
		return port.ContainerPlan{}, fmt.Errorf("run request: %w", err)
	}
	if err := lock.Validate(); err != nil {
		return port.ContainerPlan{}, fmt.Errorf("toolchain lock: %w", err)
	}
	if err := identity.Validate(); err != nil {
		return port.ContainerPlan{}, err
	}

	var transferBytes int64
	if err := addTransferBytes(&transferBytes, request.Program.Size); err != nil {
		return port.ContainerPlan{}, err
	}
	if request.Stdin != nil {
		if err := addTransferBytes(&transferBytes, request.Stdin.Size); err != nil {
			return port.ContainerPlan{}, err
		}
	}
	for _, input := range request.Files {
		if err := addTransferBytes(&transferBytes, input.Blob.Size); err != nil {
			return port.ContainerPlan{}, err
		}
	}
	for _, output := range request.Outputs {
		if err := addTransferBytes(&transferBytes, output.MaxBytes); err != nil {
			return port.ContainerPlan{}, err
		}
	}

	resources := newResourcePlan(identity)
	resources.addVolume(port.ResourceInput) // /program
	inputVolume := len(request.Files) != 0
	if inputVolume {
		resources.addVolume(port.ResourceInput) // /input
	}
	if len(request.Outputs) != 0 {
		resources.addVolume(port.ResourceOutput)
	}
	resources.addContainer(port.ResourceImport) // program import
	if inputVolume {
		resources.addContainer(port.ResourceImport)
	}
	if len(request.Outputs) != 0 {
		resources.addContainer(port.ResourceKeeper)
	}
	resources.addContainer(port.ResourceTarget)
	if len(request.Outputs) != 0 {
		resources.addContainer(port.ResourceExport)
	}
	return port.NewContainerPlan(identity.EngineIdentityDigest, resources.items, transferBytes)
}

type resourcePlanBuilder struct {
	identity      PlanIdentity
	items         []port.PlannedResource
	containerCall int
	physicalCall  int
}

func newResourcePlan(identity PlanIdentity) *resourcePlanBuilder {
	return &resourcePlanBuilder{identity: identity}
}

func (b *resourcePlanBuilder) addVolume(role port.ResourceRole) {
	physicalCall := b.physicalCall
	b.physicalCall++
	b.add(port.ResourceVolume, role, nil, &physicalCall)
}

func (b *resourcePlanBuilder) addContainer(role port.ResourceRole) {
	callOrdinal := b.containerCall
	b.containerCall++
	physicalCall := b.physicalCall
	b.physicalCall++
	b.add(port.ResourceContainer, role, &callOrdinal, &physicalCall)
}

func (b *resourcePlanBuilder) add(kind port.ResourceKind, role port.ResourceRole, callOrdinal, physicalCallOrdinal *int) {
	ordinal := len(b.items)
	resource := port.PlannedResource{
		Ordinal:             ordinal,
		Kind:                kind,
		Role:                role,
		DeterministicName:   resourceName(b.identity.OperationNonce, ordinal, kind, role),
		CreateCallOrdinal:   callOrdinal,
		PhysicalCallOrdinal: physicalCallOrdinal,
	}
	resource.ExpectedLabelsDigest = digestLabels(baseResourceLabels(b.identity, resource))
	b.items = append(b.items, resource)
}

func resourceName(nonce string, ordinal int, kind port.ResourceKind, role port.ResourceRole) string {
	shortKind := "ctr"
	if kind == port.ResourceVolume {
		shortKind = "vol"
	}
	return fmt.Sprintf("cpgen-s0-%s-%02d-%s-%s", nonce, ordinal, shortKind, strings.ToLower(string(role)))
}

// ResourceLabels completes the exact Engine label set after the plan and the
// physical call reservation exist. Both containers and volumes carry the
// physical call identity when a call is supplied. A nil volume call retains
// the legacy plan-inspection form ("none"); the Runner never uses that form
// for an actual VolumeCreate.
func ResourceLabels(identity PlanIdentity, plan port.ContainerPlan, resource port.PlannedResource, callID *domain.AttemptCallID) (map[string]string, error) {
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if err := plan.Validate(); err != nil {
		return nil, err
	}
	if plan.EngineIdentityDigest != identity.EngineIdentityDigest {
		return nil, fmt.Errorf("plan Engine identity does not match operation identity")
	}
	if resource.Ordinal < 0 || resource.Ordinal >= len(plan.Resources) || !reflect.DeepEqual(resource, plan.Resources[resource.Ordinal]) {
		return nil, fmt.Errorf("resource is not the exact planned resource at ordinal %d", resource.Ordinal)
	}
	labels := baseResourceLabels(identity, resource)
	if digestLabels(labels) != resource.ExpectedLabelsDigest {
		return nil, fmt.Errorf("resource ownership label digest mismatch")
	}
	labels["org.cpgen.plan-digest"] = string(plan.PlanDigest)
	if resource.Kind == port.ResourceContainer {
		if callID == nil {
			return nil, fmt.Errorf("Engine resource requires a call ID")
		}
		if err := callID.Validate(); err != nil {
			return nil, err
		}
		labels["org.cpgen.call"] = string(*callID)
	} else if resource.Kind == port.ResourceVolume && callID != nil {
		if err := callID.Validate(); err != nil {
			return nil, err
		}
		labels["org.cpgen.call"] = string(*callID)
	} else {
		if callID != nil {
			return nil, fmt.Errorf("non-container resource cannot carry a call ID")
		}
		labels["org.cpgen.call"] = "none"
	}
	return labels, nil
}

func baseResourceLabels(identity PlanIdentity, resource port.PlannedResource) map[string]string {
	labels := map[string]string{
		"org.cpgen.attempt":            string(identity.AttemptID),
		"org.cpgen.engine-digest":      string(identity.EngineIdentityDigest),
		"org.cpgen.execution-protocol": ExecutionProtocolDockerDirectV2,
		"org.cpgen.kind":               string(resource.Kind),
		"org.cpgen.logical-operation":  identity.LogicalOperationID,
		"org.cpgen.name":               resource.DeterministicName,
		"org.cpgen.ordinal":            strconv.Itoa(resource.Ordinal),
		"org.cpgen.role":               string(resource.Role),
		"org.cpgen.run":                string(identity.RunID),
		"org.cpgen.slice":              "0",
	}
	labels["org.cpgen.sandbox-execution"] = string(identity.SandboxExecutionID)
	return labels
}

func digestLabels(labels map[string]string) domain.Digest {
	encoded, err := json.Marshal(labels)
	if err != nil {
		panic(err) // map[string]string cannot fail JSON encoding
	}
	return domain.SumBytes(encoded)
}

func lockedToolchain(lock toolchain.Lock, id port.ToolchainID) (toolchain.Toolchain, error) {
	for _, candidate := range lock.Toolchains {
		if candidate.ID == id {
			return candidate, nil
		}
	}
	return toolchain.Toolchain{}, fmt.Errorf("toolchain %q is not pinned", id)
}

func addTransferBytes(total *int64, value int64) error {
	if value < 0 || *total > math.MaxInt64-value {
		return fmt.Errorf("transfer byte budget overflows int64")
	}
	*total += value
	return nil
}
