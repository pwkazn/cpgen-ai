package port_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

func TestNewContainerPlanUsesCanonicalDigestAndReturnsDeepClone(t *testing.T) {
	targetCall := 0
	labels := domain.SumBytes([]byte("labels"))
	engine := domain.SumBytes([]byte("engine"))
	resources := []port.PlannedResource{
		{
			Ordinal:              0,
			Kind:                 port.ResourceVolume,
			Role:                 port.ResourceInput,
			DeterministicName:    "cpgen-v-input",
			ExpectedLabelsDigest: labels,
		},
		{
			Ordinal:              1,
			Kind:                 port.ResourceContainer,
			Role:                 port.ResourceTarget,
			DeterministicName:    "cpgen-c-target",
			ExpectedLabelsDigest: labels,
			CreateCallOrdinal:    &targetCall,
		},
	}

	plan, err := port.NewContainerPlan(engine, resources, 1024)
	if err != nil {
		t.Fatal(err)
	}
	wantDigest := canonicalPlanDigestForTest(engine, resources, 1024)
	if plan.PlanDigest != wantDigest {
		t.Fatalf("plan digest = %q, want %q", plan.PlanDigest, wantDigest)
	}

	resources[0].DeterministicName = "caller-mutated"
	if plan.Resources[0].DeterministicName != "cpgen-v-input" {
		t.Fatal("constructor retained the caller's resources slice")
	}
	clone := plan.Clone()
	clone.Resources[0].DeterministicName = "clone-mutated"
	*clone.Resources[1].CreateCallOrdinal = 99
	if plan.Resources[0].DeterministicName != "cpgen-v-input" || *plan.Resources[1].CreateCallOrdinal != 0 {
		t.Fatal("Clone returned aliased plan data")
	}
	if err := plan.Validate(); err != nil {
		t.Fatalf("valid plan rejected: %v", err)
	}
}

func TestContainerPlanRejectsNonCanonicalOrExpandedResources(t *testing.T) {
	labels := domain.SumBytes([]byte("labels"))
	engine := domain.SumBytes([]byte("engine"))
	zero := 0
	one := 1
	path := domain.SafeRelPath("cpgen/release")
	nonce := "nonce"

	tests := []struct {
		name      string
		resources []port.PlannedResource
	}{
		{
			name: "non-contiguous ordinal",
			resources: []port.PlannedResource{{
				Ordinal: 1, Kind: port.ResourceContainer, Role: port.ResourceTarget,
				DeterministicName: "target", ExpectedLabelsDigest: labels, CreateCallOrdinal: &zero,
			}},
		},
		{
			name: "duplicate deterministic name",
			resources: []port.PlannedResource{
				{Ordinal: 0, Kind: port.ResourceVolume, Role: port.ResourceInput, DeterministicName: "same", ExpectedLabelsDigest: labels},
				{Ordinal: 1, Kind: port.ResourceContainer, Role: port.ResourceTarget, DeterministicName: "same", ExpectedLabelsDigest: labels, CreateCallOrdinal: &zero},
			},
		},
		{
			name: "missing target",
			resources: []port.PlannedResource{{
				Ordinal: 0, Kind: port.ResourceContainer, Role: port.ResourceImport,
				DeterministicName: "import", ExpectedLabelsDigest: labels, CreateCallOrdinal: &zero,
			}},
		},
		{
			name: "container call ordinal gap",
			resources: []port.PlannedResource{{
				Ordinal: 0, Kind: port.ResourceContainer, Role: port.ResourceTarget,
				DeterministicName: "target", ExpectedLabelsDigest: labels, CreateCallOrdinal: &one,
			}},
		},
		{
			name: "export without keeper",
			resources: []port.PlannedResource{
				{Ordinal: 0, Kind: port.ResourceContainer, Role: port.ResourceTarget, DeterministicName: "target", ExpectedLabelsDigest: labels, CreateCallOrdinal: &zero},
				{Ordinal: 1, Kind: port.ResourceContainer, Role: port.ResourceExport, DeterministicName: "export", ExpectedLabelsDigest: labels, CreateCallOrdinal: &one},
			},
		},
		{
			name: "cgroup missing identity fields",
			resources: []port.PlannedResource{
				{Ordinal: 0, Kind: port.ResourceCgroup, Role: port.ResourceReleaseParent, DeterministicName: "parent", ExpectedLabelsDigest: labels},
				{Ordinal: 1, Kind: port.ResourceContainer, Role: port.ResourceTarget, DeterministicName: "target", ExpectedLabelsDigest: labels, CreateCallOrdinal: &zero},
			},
		},
		{
			name: "volume carries cgroup fields",
			resources: []port.PlannedResource{
				{Ordinal: 0, Kind: port.ResourceVolume, Role: port.ResourceInput, DeterministicName: "input", ExpectedLabelsDigest: labels, CgroupRelativePath: &path, CreationNonce: &nonce},
				{Ordinal: 1, Kind: port.ResourceContainer, Role: port.ResourceTarget, DeterministicName: "target", ExpectedLabelsDigest: labels, CreateCallOrdinal: &zero},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := port.NewContainerPlan(engine, test.resources, 1024); err == nil {
				t.Fatal("invalid resource plan was accepted")
			}
		})
	}
}

func TestContainerPlanAllowsOnlyTheEmptyEnginePingPlan(t *testing.T) {
	engine := domain.SumBytes([]byte("engine"))
	plan, err := port.NewContainerPlan(engine, nil, 0)
	if err != nil {
		t.Fatalf("empty ping plan rejected: %v", err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := port.NewContainerPlan(engine, nil, 1); err == nil {
		t.Fatal("empty plan with transfer budget was accepted")
	}
}

func TestSandboxPlanEnumsRejectUnknownJSON(t *testing.T) {
	tests := []any{
		new(port.SandboxProfile),
		new(port.ResourceKind),
		new(port.ResourceRole),
	}
	for _, target := range tests {
		if err := json.Unmarshal([]byte(`"FUTURE_VALUE"`), target); err == nil {
			t.Fatalf("%T accepted an unknown enum", target)
		}
	}
}

func TestCapabilitySnapshotRequiresProfileEvidence(t *testing.T) {
	digest := domain.SumBytes([]byte("identity"))
	snapshot := port.CapabilitySnapshot{
		Profile:              port.ProfileExecuteMVPV2,
		EngineIdentityDigest: digest,
		EndpointDigest:       digest,
		ServerOS:             "linux",
		APIVersion:           "1.55",
		CgroupVersion:        2,
		Flags:                port.RequiredCapabilityFlags(port.ProfileExecuteMVPV2),
		BuilderImageDigest:   digest,
		RuntimeImageDigest:   digest,
		TransferImageDigest:  digest,
		ExecutionProtocol:    "docker-direct-v2",
	}
	if err := snapshot.Validate(port.ProfileExecuteMVPV2); err != nil {
		t.Fatalf("complete capability snapshot rejected: %v", err)
	}
	delete(snapshot.Flags, port.CapabilityMemorySwapDisabled)
	if err := snapshot.Validate(port.ProfileExecuteMVPV2); err == nil {
		t.Fatal("snapshot missing mandatory no-swap evidence was accepted")
	}
}

func canonicalPlanDigestForTest(engine domain.Digest, resources []port.PlannedResource, transferBytesMax int64) domain.Digest {
	var encoded bytes.Buffer
	writeString := func(value string) {
		if err := binary.Write(&encoded, binary.BigEndian, uint32(len(value))); err != nil {
			panic(err)
		}
		encoded.WriteString(value)
	}
	writeOptionalString := func(value *string) {
		if value == nil {
			encoded.WriteByte(0)
			return
		}
		encoded.WriteByte(1)
		writeString(*value)
	}

	writeString("cpgen.container-plan/v1")
	writeString(string(engine))
	_ = binary.Write(&encoded, binary.BigEndian, transferBytesMax)
	_ = binary.Write(&encoded, binary.BigEndian, uint32(len(resources)))
	for _, resource := range resources {
		_ = binary.Write(&encoded, binary.BigEndian, int64(resource.Ordinal))
		writeString(string(resource.Kind))
		writeString(string(resource.Role))
		writeString(resource.DeterministicName)
		writeString(string(resource.ExpectedLabelsDigest))
		if resource.CreateCallOrdinal == nil {
			encoded.WriteByte(0)
		} else {
			encoded.WriteByte(1)
			_ = binary.Write(&encoded, binary.BigEndian, int64(*resource.CreateCallOrdinal))
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
