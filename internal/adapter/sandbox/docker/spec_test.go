package docker_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"testing"
	"time"

	"cpgen/internal/adapter/sandbox/docker"
	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	moby "github.com/moby/moby/client"
)

func TestDockerTargetInspectCanary(t *testing.T) {
	if os.Getenv("CPGEN_DOCKER_CANARY") != "1" {
		t.Skip("set CPGEN_DOCKER_CANARY=1 to exercise the local Docker Engine")
	}
	lockFile, err := os.Open(filepath.Join("..", "..", "..", "..", "config", "toolchains", "docker-v1.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	lock, err := toolchain.LoadLock(lockFile)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "unix:///var/run/docker.sock"
	if runtime.GOOS == "windows" {
		endpoint = "npipe:////./pipe/docker_engine"
	}
	engine, err := docker.NewEngineClient(docker.Config{
		EngineEndpoint: endpoint, APIVersion: docker.RequiredAPIVersion,
		BuilderImage: string(lock.Builder.ImageID), RuntimeImage: string(lock.Runtime.ImageID), TransferImage: string(lock.Transfer.ImageID),
		ExecutionProtocol: docker.ExecutionProtocolDockerDirectV2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	identity := planIdentity()
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	identity.OperationNonce = hex.EncodeToString(nonce[:])
	request := compileRequest()
	plan, err := docker.BuildCompilePlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var volumeNames []string
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		for index := len(volumeNames) - 1; index >= 0; index-- {
			_, _ = engine.VolumeRemove(cleanup, volumeNames[index], moby.VolumeRemoveOptions{Force: true})
		}
	}()
	for _, resource := range plan.Resources {
		if resource.Kind != port.ResourceVolume {
			continue
		}
		labels, err := docker.ResourceLabels(identity, plan, resource, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := engine.VolumeCreate(ctx, moby.VolumeCreateOptions{Name: resource.DeterministicName, Driver: "local", Labels: labels}); err != nil {
			t.Fatal(err)
		}
		volumeNames = append(volumeNames, resource.DeterministicName)
	}
	expected, err := docker.TargetCreateOptions(docker.CompileTarget(request), lock, identity, plan, "call_00000000000000000000000000000004")
	if err != nil {
		t.Fatal(err)
	}
	created, err := engine.ContainerCreate(ctx, expected)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
		defer stop()
		_, _ = engine.ContainerRemove(cleanup, created.ID, moby.ContainerRemoveOptions{Force: true})
	}()
	inspected, err := engine.ContainerInspect(ctx, created.ID, moby.ContainerInspectOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := docker.VerifyTargetInspect(expected, inspected); err != nil {
		t.Fatalf("real Docker inspect rejected: %v\nwant resources: %#v\ngot resources: %#v", err, expected.HostConfig.Resources, inspected.Container.HostConfig.Resources)
	}
}

func TestTargetCreateOptionsBuildsExactCompileSecurityContract(t *testing.T) {
	request := compileRequest()
	identity := planIdentity()
	lock := toolchainLock(t)
	plan, err := docker.BuildCompilePlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	callID := domain.AttemptCallID("call_00000000000000000000000000000004")
	options, err := docker.TargetCreateOptions(docker.CompileTarget(request), lock, identity, plan, callID)
	if err != nil {
		t.Fatal(err)
	}

	config, host := options.Config, options.HostConfig
	if options.Image != "" || config.Image != string(lock.Builder.ImageID) {
		t.Fatalf("image shortcut=%q config image=%q", options.Image, config.Image)
	}
	wantCommand := []string{"/usr/bin/g++", "-std=c++20", "-O2", "-pipe", "-static", "-s", "-I/opt/cpgen/include", "/src/main.cpp", "-o", "/result/files/main"}
	if !slices.Equal(config.Entrypoint, wantCommand) || len(config.Cmd) != 0 || len(config.Shell) != 0 {
		t.Fatalf("entrypoint=%#v cmd=%#v shell=%#v", config.Entrypoint, config.Cmd, config.Shell)
	}
	if config.User != "65532:65532" || config.WorkingDir != "/work" || !config.NetworkDisabled {
		t.Fatalf("portable target config = %#v", config)
	}
	wantEnv := map[string]string{
		"LANG": "C.UTF-8", "TZ": "UTC", "HOME": "/work/home", "TMPDIR": "/work/tmp",
		"PATH":           "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		"GOLANG_VERSION": "1.24.13", "GOTOOLCHAIN": "local", "GOPATH": "/work/gopath",
		"GOTMPDIR": "/work/tmp", "GOCACHE": "/work/go-cache",
	}
	if got := envMap(config.Env); !maps.Equal(got, wantEnv) {
		t.Fatalf("environment = %#v, want %#v", got, wantEnv)
	}
	if !config.AttachStdout || !config.AttachStderr || config.AttachStdin || config.Tty || len(config.Volumes) != 0 {
		t.Fatalf("stream or image volume contract = %#v", config)
	}

	if !host.ReadonlyRootfs || string(host.NetworkMode) != "none" || !slices.Equal(host.CapDrop, []string{"ALL"}) || len(host.CapAdd) != 0 {
		t.Fatalf("namespace/capability contract = %#v", host)
	}
	if !slices.Equal(host.SecurityOpt, []string{"no-new-privileges"}) || host.Privileged || host.AutoRemove || host.Init == nil || *host.Init {
		t.Fatalf("security booleans = %#v", host)
	}
	if host.Memory != request.Limits.MemoryBytes || host.MemorySwap != request.Limits.MemoryBytes || host.PidsLimit == nil || *host.PidsLimit != request.Limits.PIDs {
		t.Fatalf("resource limits = %#v", host.Resources)
	}
	if host.LogConfig.Type != "none" || len(host.LogConfig.Config) != 0 || host.RestartPolicy.Name != container.RestartPolicyDisabled || host.ShmSize != 16<<20 {
		t.Fatalf("lifecycle limits = %#v", host)
	}
	if len(host.Binds) != 0 || len(host.Devices) != 0 || len(host.DeviceRequests) != 0 || host.PidMode != "" || host.IpcMode != container.IPCModePrivate || host.UTSMode != "" || host.UsernsMode != "" {
		t.Fatalf("host injection surface is non-empty: %#v", host)
	}
	assertTargetMounts(t, host.Mounts, map[string]mountExpectation{
		"/src":          {kind: mount.TypeVolume, readOnly: true, noCopy: true},
		"/result/files": {kind: mount.TypeVolume, readOnly: false, noCopy: true},
	})
	if len(host.Tmpfs) != 1 || host.Tmpfs["/work"] == "" {
		t.Fatalf("scratch tmpfs = %#v", host.Tmpfs)
	}
	if got := ulimitMap(host.Ulimits); !reflect.DeepEqual(got, map[string][2]int64{
		"core": {0, 0}, "fsize": {request.Limits.OutputBytes, request.Limits.OutputBytes}, "nofile": {256, 256}, "nproc": {request.Limits.PIDs, request.Limits.PIDs},
	}) {
		t.Fatalf("ulimits = %#v", got)
	}
	if config.Labels["org.cpgen.call"] != string(callID) || config.Labels["org.cpgen.plan-digest"] != string(plan.PlanDigest) {
		t.Fatalf("target labels = %#v", config.Labels)
	}
}

func TestTargetCreateOptionsBuildsDirectRunWithoutRequestInjection(t *testing.T) {
	request := runRequest()
	seed := uint64(18446744073709551615)
	request.Role = port.RoleGenerator
	request.Seed = &seed
	request.Outputs = []port.OutputDeclaration{{Path: "files/answer.txt", MaxBytes: 2048}}
	identity := planIdentity()
	lock := toolchainLock(t)
	plan, err := docker.BuildRunPlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	options, err := docker.TargetCreateOptions(docker.RunTarget(request), lock, identity, plan, "call_00000000000000000000000000000004")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(options.Config.Entrypoint, []string{"/program/main", "--seed=18446744073709551615"}) || len(options.Config.Cmd) != 0 {
		t.Fatalf("run command = %#v %#v", options.Config.Entrypoint, options.Config.Cmd)
	}
	if options.Config.Image != string(lock.Runtime.ImageID) || !options.Config.AttachStdin || !options.Config.OpenStdin || !options.Config.StdinOnce {
		t.Fatalf("run image/stdin config = %#v", options.Config)
	}
	wantEnv := map[string]string{
		"LANG": "C.UTF-8", "TZ": "UTC", "HOME": "/work/home", "TMPDIR": "/work/tmp",
		"PATH": "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	}
	if got := envMap(options.Config.Env); !maps.Equal(got, wantEnv) {
		t.Fatalf("run environment = %#v", got)
	}
	assertTargetMounts(t, options.HostConfig.Mounts, map[string]mountExpectation{
		"/program": {kind: mount.TypeVolume, readOnly: true, noCopy: true},
		"/input":   {kind: mount.TypeVolume, readOnly: true, noCopy: true},
		"/result":  {kind: mount.TypeVolume, readOnly: false, noCopy: true},
	})
}

func TestTargetCreateOptionsUsesTheLockedGoArgvAsOneArgumentPerEntry(t *testing.T) {
	request := compileRequest()
	request.Language = port.LanguageGo
	request.Toolchain = toolchain.GoToolchainID
	request.SourceBundle.EntryPoint = "main.go"
	request.SourceBundle.Files[0].Path = "main.go"
	request.SourceBundle.Digest, _ = port.ComputeSourceBundleDigest(request.SourceBundle)
	identity := planIdentity()
	lock := toolchainLock(t)
	plan, err := docker.BuildCompilePlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	options, err := docker.TargetCreateOptions(docker.CompileTarget(request), lock, identity, plan, "call_00000000000000000000000000000004")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/local/go/bin/go", "build", "-trimpath", "-ldflags=-s -w -buildid=",
		"-o", "/result/files/main", "/src/main.go",
	}
	if !slices.Equal(options.Config.Entrypoint, want) {
		t.Fatalf("Go argv = %#v, want %#v", options.Config.Entrypoint, want)
	}
	environment := envMap(options.Config.Env)
	for key, value := range map[string]string{
		"CGO_ENABLED": "0", "GO111MODULE": "off", "GOPROXY": "off", "GOSUMDB": "off",
		"GOCACHE": "/work/go-cache", "GOTMPDIR": "/work/tmp", "GOPATH": "/work/gopath",
	} {
		if environment[key] != value {
			t.Fatalf("Go environment %s = %q, want %q", key, environment[key], value)
		}
	}
}

func TestTargetCreateOptionsRejectsTamperedPlan(t *testing.T) {
	request := compileRequest()
	identity := planIdentity()
	lock := toolchainLock(t)
	plan, err := docker.BuildCompilePlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	plan.Resources[0].DeterministicName = "attacker-volume"
	if _, err := docker.TargetCreateOptions(docker.CompileTarget(request), lock, identity, plan, "call_00000000000000000000000000000004"); err == nil {
		t.Fatal("tampered plan was accepted")
	}
}

func TestVerifyTargetInspectAcceptsExactConfigurationAndRejectsDrift(t *testing.T) {
	request := compileRequest()
	identity := planIdentity()
	lock := toolchainLock(t)
	plan, err := docker.BuildCompilePlan(request, lock, identity)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := docker.TargetCreateOptions(docker.CompileTarget(request), lock, identity, plan, "call_00000000000000000000000000000004")
	if err != nil {
		t.Fatal(err)
	}
	if err := docker.VerifyTargetInspect(expected, inspectFrom(expected)); err != nil {
		t.Fatalf("exact inspect rejected: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*moby.ContainerInspectResult)
	}{
		{name: "image", mutate: func(got *moby.ContainerInspectResult) { got.Container.Image = string(lock.Runtime.ImageID) }},
		{name: "user", mutate: func(got *moby.ContainerInspectResult) { got.Container.Config.User = "0:0" }},
		{name: "entrypoint", mutate: func(got *moby.ContainerInspectResult) {
			got.Container.Config.Entrypoint = []string{"/bin/sh", "-c", "evil"}
		}},
		{name: "writable root", mutate: func(got *moby.ContainerInspectResult) { got.Container.HostConfig.ReadonlyRootfs = false }},
		{name: "cap add", mutate: func(got *moby.ContainerInspectResult) { got.Container.HostConfig.CapAdd = []string{"SYS_ADMIN"} }},
		{name: "host network", mutate: func(got *moby.ContainerInspectResult) { got.Container.HostConfig.NetworkMode = "host" }},
		{name: "container pid namespace", mutate: func(got *moby.ContainerInspectResult) { got.Container.HostConfig.PidMode = "container:foreign" }},
		{name: "supplementary group", mutate: func(got *moby.ContainerInspectResult) { got.Container.HostConfig.GroupAdd = []string{"0"} }},
		{name: "memory", mutate: func(got *moby.ContainerInspectResult) { got.Container.HostConfig.Memory++ }},
		{name: "logging", mutate: func(got *moby.ContainerInspectResult) { got.Container.HostConfig.LogConfig.Type = "json-file" }},
		{name: "restart", mutate: func(got *moby.ContainerInspectResult) {
			got.Container.HostConfig.RestartPolicy.Name = container.RestartPolicyAlways
		}},
		{name: "image volume", mutate: func(got *moby.ContainerInspectResult) {
			got.Container.Config.Volumes = map[string]struct{}{`/secret`: {}}
		}},
		{name: "missing nocopy", mutate: func(got *moby.ContainerInspectResult) {
			got.Container.HostConfig.Mounts[0].VolumeOptions.NoCopy = false
		}},
		{name: "extra writable mount", mutate: func(got *moby.ContainerInspectResult) {
			got.Container.HostConfig.Mounts = append(got.Container.HostConfig.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/host", Target: "/escape"})
			got.Container.Mounts = append(got.Container.Mounts, container.MountPoint{Type: mount.TypeBind, Source: "/host", Destination: "/escape", RW: true})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := inspectFrom(expected)
			test.mutate(&got)
			if err := docker.VerifyTargetInspect(expected, got); err == nil {
				t.Fatal("unsafe inspect drift was accepted")
			}
		})
	}
}

type mountExpectation struct {
	kind     mount.Type
	readOnly bool
	noCopy   bool
}

func assertTargetMounts(t *testing.T, mounts []mount.Mount, want map[string]mountExpectation) {
	t.Helper()
	if len(mounts) != len(want) {
		t.Fatalf("mount count = %d, want %d: %#v", len(mounts), len(want), mounts)
	}
	for _, item := range mounts {
		expected, ok := want[item.Target]
		if !ok || item.Type != expected.kind || item.ReadOnly != expected.readOnly {
			t.Fatalf("unexpected mount %#v", item)
		}
		if item.Type == mount.TypeVolume && (item.Source == "" || item.VolumeOptions == nil || item.VolumeOptions.NoCopy != expected.noCopy) {
			t.Fatalf("unsafe volume mount %#v", item)
		}
		if item.Type == mount.TypeTmpfs && (item.Source != "" || item.TmpfsOptions == nil || item.TmpfsOptions.SizeBytes <= 0) {
			t.Fatalf("unsafe tmpfs mount %#v", item)
		}
	}
}

func envMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, entry := range environment {
		for index := range entry {
			if entry[index] == '=' {
				result[entry[:index]] = entry[index+1:]
				break
			}
		}
	}
	return result
}

func ulimitMap(limits []*container.Ulimit) map[string][2]int64 {
	result := make(map[string][2]int64, len(limits))
	for _, limit := range limits {
		result[limit.Name] = [2]int64{limit.Soft, limit.Hard}
	}
	return result
}

func inspectFrom(expected moby.ContainerCreateOptions) moby.ContainerInspectResult {
	configCopy := *expected.Config
	configCopy.Entrypoint = slices.Clone(expected.Config.Entrypoint)
	configCopy.Cmd = slices.Clone(expected.Config.Cmd)
	configCopy.Env = slices.Clone(expected.Config.Env)
	configCopy.Labels = maps.Clone(expected.Config.Labels)
	hostCopy := *expected.HostConfig
	hostCopy.CapAdd = slices.Clone(expected.HostConfig.CapAdd)
	hostCopy.CapDrop = slices.Clone(expected.HostConfig.CapDrop)
	hostCopy.SecurityOpt = slices.Clone(expected.HostConfig.SecurityOpt)
	hostCopy.Tmpfs = maps.Clone(expected.HostConfig.Tmpfs)
	hostCopy.Mounts = cloneMounts(expected.HostConfig.Mounts)
	hostCopy.Ulimits = cloneUlimits(expected.HostConfig.Ulimits)
	result := moby.ContainerInspectResult{Container: container.InspectResponse{
		Name:       "/" + expected.Name,
		Image:      expected.Config.Image,
		Config:     &configCopy,
		HostConfig: &hostCopy,
	}}
	for _, item := range expected.HostConfig.Mounts {
		point := container.MountPoint{Type: item.Type, Destination: item.Target, RW: !item.ReadOnly}
		if item.Type == mount.TypeVolume {
			point.Name = item.Source
		} else {
			point.Source = item.Source
		}
		result.Container.Mounts = append(result.Container.Mounts, point)
	}
	return result
}

func cloneMounts(source []mount.Mount) []mount.Mount {
	result := make([]mount.Mount, len(source))
	for index, item := range source {
		result[index] = item
		if item.VolumeOptions != nil {
			copy := *item.VolumeOptions
			result[index].VolumeOptions = &copy
		}
		if item.TmpfsOptions != nil {
			copy := *item.TmpfsOptions
			copy.Options = slices.Clone(item.TmpfsOptions.Options)
			result[index].TmpfsOptions = &copy
		}
	}
	return result
}

func cloneUlimits(source []*container.Ulimit) []*container.Ulimit {
	result := make([]*container.Ulimit, len(source))
	for index, item := range source {
		copy := *item
		result[index] = &copy
	}
	return result
}
