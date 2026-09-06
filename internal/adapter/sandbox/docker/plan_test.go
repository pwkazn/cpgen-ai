package docker_test

import (
	"slices"
	"testing"
	"time"

	"cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

func TestBuildCompilePlanFixesResourceOrderNamesAndTransferBudget(t *testing.T) {
	request := compileRequest()
	identity := planIdentity()
	plan, err := docker.BuildCompilePlan(request, toolchainLock(t), identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := plan.Validate(); err != nil {
		t.Fatal(err)
	}
	wantTransfer := request.SourceBundle.Files[0].Blob.Size + request.SourceBundle.Files[1].Blob.Size + request.Limits.OutputBytes
	if plan.TransferBytesMax != wantTransfer {
		t.Fatalf("transfer budget = %d, want %d", plan.TransferBytesMax, wantTransfer)
	}
	if got := containerRoles(plan); !slices.Equal(got, []port.ResourceRole{port.ResourceImport, port.ResourceKeeper, port.ResourceTarget, port.ResourceExport}) {
		t.Fatalf("container roles = %#v", got)
	}
	if got := volumeRoles(plan); !slices.Equal(got, []port.ResourceRole{port.ResourceInput, port.ResourceOutput}) {
		t.Fatalf("volume roles = %#v", got)
	}
	assertPlanResourceIdentity(t, plan, identity)
}

func TestBuildRunPlanUsesOnlyRequiredImportsAndOutputLifecycle(t *testing.T) {
	request := runRequest()
	identity := planIdentity()
	lock := toolchainLock(t)

	withoutOutput, err := docker.BuildRunPlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	if got := containerRoles(withoutOutput); !slices.Equal(got, []port.ResourceRole{port.ResourceImport, port.ResourceImport, port.ResourceTarget}) {
		t.Fatalf("containers without output = %#v", got)
	}
	if got := volumeRoles(withoutOutput); !slices.Equal(got, []port.ResourceRole{port.ResourceInput, port.ResourceInput}) {
		t.Fatalf("volumes without output = %#v", got)
	}
	wantInput := request.Program.Size + request.Stdin.Size + request.Files[0].Blob.Size
	if withoutOutput.TransferBytesMax != wantInput {
		t.Fatalf("input transfer budget = %d, want %d", withoutOutput.TransferBytesMax, wantInput)
	}

	request.Outputs = []port.OutputDeclaration{{Path: "files/generated.txt", MaxBytes: 4096}}
	withOutput, err := docker.BuildRunPlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	if got := containerRoles(withOutput); !slices.Equal(got, []port.ResourceRole{port.ResourceImport, port.ResourceImport, port.ResourceKeeper, port.ResourceTarget, port.ResourceExport}) {
		t.Fatalf("containers with output = %#v", got)
	}
	if got := volumeRoles(withOutput); !slices.Equal(got, []port.ResourceRole{port.ResourceInput, port.ResourceInput, port.ResourceOutput}) {
		t.Fatalf("volumes with output = %#v", got)
	}
	if withOutput.TransferBytesMax != wantInput+4096 {
		t.Fatalf("output transfer budget = %d", withOutput.TransferBytesMax)
	}
	assertPlanResourceIdentity(t, withOutput, identity)
}

func TestBuildRunPlanStreamsStdinWithoutCreatingAnInputVolume(t *testing.T) {
	request := runRequest()
	request.Files = nil
	plan, err := docker.BuildRunPlan(request, toolchainLock(t), planIdentity())
	if err != nil {
		t.Fatal(err)
	}
	if got := containerRoles(plan); !slices.Equal(got, []port.ResourceRole{port.ResourceImport, port.ResourceTarget}) {
		t.Fatalf("container roles = %#v", got)
	}
	if got := volumeRoles(plan); !slices.Equal(got, []port.ResourceRole{port.ResourceInput}) {
		t.Fatalf("volume roles = %#v", got)
	}
}

func TestBuildPlansRejectUntrustedOrInconsistentInputs(t *testing.T) {
	lock := toolchainLock(t)
	identity := planIdentity()
	compile := compileRequest()
	run := runRequest()

	tests := []struct {
		name string
		run  func() error
	}{
		{name: "invalid operation nonce", run: func() error {
			bad := identity
			bad.OperationNonce = "../predictable"
			_, err := docker.BuildCompilePlan(compile, lock, bad)
			return err
		}},
		{name: "toolchain language mismatch", run: func() error {
			bad := compile
			bad.Language = port.LanguageGo
			_, err := docker.BuildCompilePlan(bad, lock, identity)
			return err
		}},
		{name: "compile output path injection", run: func() error {
			bad := compile
			bad.ExpectedOutput = "somewhere/else"
			_, err := docker.BuildCompilePlan(bad, lock, identity)
			return err
		}},
		{name: "invalid lock", run: func() error {
			bad := lock
			bad.ExecutionProtocol = "mutable"
			_, err := docker.BuildRunPlan(run, bad, identity)
			return err
		}},
		{name: "transfer overflow", run: func() error {
			bad := run
			bad.Program.Size = int64(^uint64(0) >> 1)
			_, err := docker.BuildRunPlan(bad, lock, identity)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.run(); err == nil {
				t.Fatal("unsafe input was accepted")
			}
		})
	}
}

func assertPlanResourceIdentity(t *testing.T, plan port.ContainerPlan, identity docker.PlanIdentity) {
	t.Helper()
	seen := map[string]struct{}{}
	for index, resource := range plan.Resources {
		if resource.Ordinal != index || resource.DeterministicName == "" {
			t.Fatalf("resource %d identity = %#v", index, resource)
		}
		if _, exists := seen[resource.DeterministicName]; exists {
			t.Fatalf("duplicate resource name %q", resource.DeterministicName)
		}
		seen[resource.DeterministicName] = struct{}{}
		if err := resource.ExpectedLabelsDigest.Validate(); err != nil {
			t.Fatalf("resource %d labels digest: %v", index, err)
		}
		labels, err := docker.ResourceLabels(identity, plan, resource, callFor(resource))
		if err != nil {
			t.Fatalf("resource %d labels: %v", index, err)
		}
		for key, want := range map[string]string{
			"org.cpgen.run":               string(identity.RunID),
			"org.cpgen.attempt":           string(identity.AttemptID),
			"org.cpgen.logical-operation": identity.LogicalOperationID,
			"org.cpgen.sandbox-execution": string(identity.SandboxExecutionID),
			"org.cpgen.role":              string(resource.Role),
			"org.cpgen.plan-digest":       string(plan.PlanDigest),
			"org.cpgen.engine-digest":     string(identity.EngineIdentityDigest),
		} {
			if labels[key] != want {
				t.Fatalf("resource %d label %s = %q, want %q", index, key, labels[key], want)
			}
		}
		if resource.Kind == port.ResourceContainer && labels["org.cpgen.call"] == "" {
			t.Fatalf("container %d has no call label", index)
		}
	}
}

func callFor(resource port.PlannedResource) *domain.AttemptCallID {
	if resource.Kind != port.ResourceContainer {
		return nil
	}
	id := domain.AttemptCallID("call_00000000000000000000000000000004")
	return &id
}

func containerRoles(plan port.ContainerPlan) []port.ResourceRole {
	var roles []port.ResourceRole
	for _, resource := range plan.Resources {
		if resource.Kind == port.ResourceContainer {
			roles = append(roles, resource.Role)
		}
	}
	return roles
}

func volumeRoles(plan port.ContainerPlan) []port.ResourceRole {
	var roles []port.ResourceRole
	for _, resource := range plan.Resources {
		if resource.Kind == port.ResourceVolume {
			roles = append(roles, resource.Role)
		}
	}
	return roles
}

func planIdentity() docker.PlanIdentity {
	return docker.PlanIdentity{
		RunID:                "run_00000000000000000000000000000001",
		AttemptID:            "attempt_00000000000000000000000000000002",
		LogicalOperationID:   "compile-solution",
		SandboxExecutionID:   "sandbox_00000000000000000000000000000003",
		OperationNonce:       "0123456789abcdef0123456789abcdef",
		EngineIdentityDigest: domain.SumBytes([]byte("engine")),
	}
}

func toolchainLock(t *testing.T) toolchain.Lock {
	t.Helper()
	lock, err := toolchain.NewDockerV1Lock(
		domain.SumBytes([]byte("builder")),
		domain.SumBytes([]byte("runtime")),
		domain.SumBytes([]byte("transfer")),
	)
	if err != nil {
		t.Fatal(err)
	}
	return lock
}

func compileRequest() port.CompileRequest {
	request := port.CompileRequest{
		Language: port.LanguageCPP20,
		Role:     port.RoleSolution,
		SourceBundle: port.SourceBundleManifest{
			SchemaVersion: "cpgen.source-bundle/v1",
			EntryPoint:    "main.cpp",
			Files: []port.SourceFile{
				{Path: "main.cpp", Blob: blob("source-main", 100)},
				{Path: "helper.h", Blob: blob("source-helper", 20)},
			},
		},
		Toolchain:      toolchain.CPP20ToolchainID,
		Limits:         port.CompileLimits{Time: 10 * time.Second, MemoryBytes: 512 << 20, PIDs: 64, OutputBytes: 8 << 20},
		ExpectedOutput: "result/files/main",
	}
	digest, err := port.ComputeSourceBundleDigest(request.SourceBundle)
	if err != nil {
		panic(err)
	}
	request.SourceBundle.Digest = digest
	return request
}

func runRequest() port.RunRequest {
	stdin := blob("stdin", 30)
	return port.RunRequest{
		Role:    port.RoleSolution,
		Program: blob("program", 4096),
		Stdin:   &stdin,
		Files:   []port.InputMount{{Path: "case.txt", Blob: blob("case", 70)}},
		Limits:  port.RunLimits{Time: time.Second, MemoryBytes: 256 << 20, PIDs: 32, StdoutBytes: 1 << 20, StderrBytes: 1 << 20},
	}
}

func blob(seed string, size int64) domain.BlobRef {
	return domain.BlobRef{Digest: domain.SumBytes([]byte(seed)), Size: size}
}
