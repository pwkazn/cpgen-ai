package docker

import (
	"fmt"
	"maps"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	moby "github.com/moby/moby/client"
)

const (
	targetUser         = "65532:65532"
	targetWorkingDir   = "/work"
	targetHome         = "/work/home"
	targetTemp         = "/work/tmp"
	targetGoCache      = "/work/go-cache"
	targetGoPath       = "/work/gopath"
	targetShmMaxBytes  = int64(16 << 20)
	targetScratchMax   = int64(256 << 20)
	targetScratchFloor = int64(1 << 20)
	targetNoFileLimit  = int64(256)
	baseGoVersion      = "1.24.13"
	builderPath        = "/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	runtimePath        = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// TargetWorkload is a closed request union. Callers can select a typed compile
// or run request, but cannot supply a command, image, environment, or mount.
type TargetWorkload struct {
	compile *port.CompileRequest
	run     *port.RunRequest
}

func CompileTarget(request port.CompileRequest) TargetWorkload {
	clone := request
	clone.SourceBundle.Files = slices.Clone(request.SourceBundle.Files)
	return TargetWorkload{compile: &clone}
}

func RunTarget(request port.RunRequest) TargetWorkload {
	clone := request
	if request.Seed != nil {
		seed := *request.Seed
		clone.Seed = &seed
	}
	if request.Args.GeneratorCase != nil {
		args := *request.Args.GeneratorCase
		clone.Args.GeneratorCase = &args
	}
	clone.Files = slices.Clone(request.Files)
	clone.Outputs = slices.Clone(request.Outputs)
	if request.Stdin != nil {
		stdin := *request.Stdin
		clone.Stdin = &stdin
	}
	return TargetWorkload{run: &clone}
}

func TargetCreateOptions(workload TargetWorkload, lock toolchain.Lock, identity PlanIdentity, plan port.ContainerPlan, callID domain.AttemptCallID) (moby.ContainerCreateOptions, error) {
	if err := lock.Validate(); err != nil {
		return moby.ContainerCreateOptions{}, fmt.Errorf("toolchain lock: %w", err)
	}
	if err := identity.Validate(); err != nil {
		return moby.ContainerCreateOptions{}, err
	}
	if err := callID.Validate(); err != nil {
		return moby.ContainerCreateOptions{}, err
	}
	if err := plan.Validate(); err != nil {
		return moby.ContainerCreateOptions{}, err
	}

	var (
		expectedPlan port.ContainerPlan
		image        toolchain.Image
		command      []string
		environment  map[string]string
		mounts       []mount.Mount
		memory       int64
		pids         int64
		fileSize     int64
		attachStdin  bool
		err          error
	)
	volumes := volumeResources(plan)
	switch {
	case workload.compile != nil && workload.run == nil:
		request := *workload.compile
		expectedPlan, err = BuildCompilePlan(request, lock, identity)
		if err != nil {
			return moby.ContainerCreateOptions{}, err
		}
		if len(volumes) != 2 {
			return moby.ContainerCreateOptions{}, fmt.Errorf("compile plan must contain source and output volumes")
		}
		compiler, findErr := lockedToolchain(lock, request.Toolchain)
		if findErr != nil {
			return moby.ContainerCreateOptions{}, findErr
		}
		command, err = compileCommand(compiler, request.SourceBundle.EntryPoint)
		if err != nil {
			return moby.ContainerCreateOptions{}, err
		}
		environment = builderEnvironment(compiler.Environment)
		image = lock.Builder
		mounts = []mount.Mount{
			volumeMount(volumes[0].DeterministicName, "/src", true),
			volumeMount(volumes[1].DeterministicName, "/result/files", false),
		}
		memory, pids, fileSize = request.Limits.MemoryBytes, request.Limits.PIDs, request.Limits.OutputBytes
	case workload.run != nil && workload.compile == nil:
		request := *workload.run
		expectedPlan, err = BuildRunPlan(request, lock, identity)
		if err != nil {
			return moby.ContainerCreateOptions{}, err
		}
		wantVolumes := 1
		withInput := len(request.Files) != 0
		if withInput {
			wantVolumes++
		}
		withOutput := len(request.Outputs) != 0
		if withOutput {
			wantVolumes++
		}
		if len(volumes) != wantVolumes {
			return moby.ContainerCreateOptions{}, fmt.Errorf("run plan volume topology does not match the request")
		}
		mounts = append(mounts, volumeMount(volumes[0].DeterministicName, "/program", true))
		index := 1
		if withInput {
			mounts = append(mounts, volumeMount(volumes[index].DeterministicName, "/input", true))
			index++
		}
		if withOutput {
			mounts = append(mounts, volumeMount(volumes[index].DeterministicName, "/result", false))
		}
		command = []string{"/program/main"}
		if request.Seed != nil {
			command = append(command, "--seed="+strconv.FormatUint(*request.Seed, 10))
		}
		if args := request.Args.GeneratorCase; args != nil {
			command = append(command, "--case="+strconv.Itoa(args.Ordinal), "--kind="+string(args.Kind))
		}
		environment = runtimeEnvironment()
		image = lock.Runtime
		memory, pids = request.Limits.MemoryBytes, request.Limits.PIDs
		fileSize = scratchBytes(memory)
		for _, output := range request.Outputs {
			if output.MaxBytes > fileSize {
				fileSize = output.MaxBytes
			}
		}
		attachStdin = request.Stdin != nil
	default:
		return moby.ContainerCreateOptions{}, fmt.Errorf("target workload must contain exactly one typed request")
	}
	if !reflect.DeepEqual(plan, expectedPlan) {
		return moby.ContainerCreateOptions{}, fmt.Errorf("authorized container plan does not exactly match the request")
	}

	target, err := targetResource(plan)
	if err != nil {
		return moby.ContainerCreateOptions{}, err
	}
	labels, err := ResourceLabels(identity, plan, target, &callID)
	if err != nil {
		return moby.ContainerCreateOptions{}, err
	}
	for key, value := range image.Labels {
		if current, exists := labels[key]; exists && current != value {
			return moby.ContainerCreateOptions{}, fmt.Errorf("image label %q conflicts with resource identity", key)
		}
		labels[key] = value
	}
	workTmpfs := scratchTmpfs(scratchBytes(memory))
	pidsLimit := pids
	initDisabled := false
	oomKillEnabled := false
	shmSize := shmBytes(memory)
	config := &container.Config{
		User:            targetUser,
		AttachStdin:     attachStdin,
		AttachStdout:    true,
		AttachStderr:    true,
		Tty:             false,
		OpenStdin:       attachStdin,
		StdinOnce:       attachStdin,
		Env:             sortedEnvironment(environment),
		Cmd:             nil,
		Image:           string(image.ImageID),
		Volumes:         nil,
		WorkingDir:      targetWorkingDir,
		Entrypoint:      slices.Clone(command),
		NetworkDisabled: true,
		Labels:          labels,
		StopSignal:      "SIGTERM",
		Shell:           nil,
	}
	host := &container.HostConfig{
		LogConfig:      container.LogConfig{Type: "none", Config: map[string]string{}},
		NetworkMode:    container.NetworkMode(network.NetworkNone),
		RestartPolicy:  container.RestartPolicy{Name: container.RestartPolicyDisabled},
		AutoRemove:     false,
		CapAdd:         nil,
		CapDrop:        []string{"ALL"},
		CgroupnsMode:   container.CgroupnsModePrivate,
		IpcMode:        container.IPCModePrivate,
		Privileged:     false,
		ReadonlyRootfs: true,
		SecurityOpt:    []string{"no-new-privileges"},
		Tmpfs:          map[string]string{targetWorkingDir: workTmpfs},
		Runtime:        "runc",
		ShmSize:        shmSize,
		Resources: container.Resources{
			Memory:         memory,
			MemorySwap:     memory,
			OomKillDisable: &oomKillEnabled,
			PidsLimit:      &pidsLimit,
			Ulimits: []*container.Ulimit{
				{Name: "core", Soft: 0, Hard: 0},
				{Name: "fsize", Soft: fileSize, Hard: fileSize},
				{Name: "nofile", Soft: targetNoFileLimit, Hard: targetNoFileLimit},
				{Name: "nproc", Soft: pids, Hard: pids},
			},
		},
		Mounts: mounts,
		Init:   &initDisabled,
	}
	options := moby.ContainerCreateOptions{Config: config, HostConfig: host, Name: target.DeterministicName}
	if err := validateTargetCreateOptions(options); err != nil {
		return moby.ContainerCreateOptions{}, err
	}
	return options, nil
}

func VerifyTargetInspect(expected moby.ContainerCreateOptions, actual moby.ContainerInspectResult) error {
	if err := validateTargetCreateOptions(expected); err != nil {
		return fmt.Errorf("invalid expected target specification: %w", err)
	}
	got := actual.Container
	if got.Config == nil || got.HostConfig == nil {
		return fmt.Errorf("container inspect omitted Config or HostConfig")
	}
	if strings.TrimPrefix(got.Name, "/") != expected.Name {
		return fmt.Errorf("container name is %q, want %q", got.Name, expected.Name)
	}
	wantConfig, wantHost := expected.Config, expected.HostConfig
	if got.Image != wantConfig.Image || got.Config.Image != wantConfig.Image {
		return fmt.Errorf("container image is %q/%q, want exact ID %q", got.Image, got.Config.Image, wantConfig.Image)
	}
	if got.Config.User != wantConfig.User || got.Config.WorkingDir != wantConfig.WorkingDir {
		return fmt.Errorf("container user or working directory drifted")
	}
	if !slices.Equal(got.Config.Entrypoint, wantConfig.Entrypoint) || !slices.Equal(got.Config.Cmd, wantConfig.Cmd) || len(got.Config.Shell) != 0 {
		return fmt.Errorf("container command drifted")
	}
	if !equalEnvironment(got.Config.Env, wantConfig.Env) {
		return fmt.Errorf("container environment drifted")
	}
	if !maps.Equal(got.Config.Labels, wantConfig.Labels) {
		return fmt.Errorf("container labels drifted")
	}
	if got.Config.AttachStdin != wantConfig.AttachStdin || got.Config.AttachStdout != wantConfig.AttachStdout || got.Config.AttachStderr != wantConfig.AttachStderr ||
		got.Config.OpenStdin != wantConfig.OpenStdin || got.Config.StdinOnce != wantConfig.StdinOnce || got.Config.Tty || got.Config.NetworkDisabled != wantConfig.NetworkDisabled {
		return fmt.Errorf("container stream configuration drifted")
	}
	if len(got.Config.Volumes) != 0 || len(got.Config.ExposedPorts) != 0 || got.Config.Healthcheck != nil || len(got.Config.OnBuild) != 0 {
		return fmt.Errorf("container image injected volumes, ports, healthcheck, or on-build actions")
	}
	if got.Config.StopSignal != wantConfig.StopSignal || !optionalIntEqual(got.Config.StopTimeout, wantConfig.StopTimeout) {
		return fmt.Errorf("container stop configuration drifted")
	}

	if got.HostConfig.ReadonlyRootfs != wantHost.ReadonlyRootfs || got.HostConfig.NetworkMode != wantHost.NetworkMode ||
		!slices.Equal(got.HostConfig.CapDrop, wantHost.CapDrop) || len(got.HostConfig.CapAdd) != 0 ||
		!slices.Equal(got.HostConfig.SecurityOpt, wantHost.SecurityOpt) || got.HostConfig.Privileged {
		return fmt.Errorf("container isolation or capability configuration drifted")
	}
	if got.HostConfig.LogConfig.Type != wantHost.LogConfig.Type || !maps.Equal(got.HostConfig.LogConfig.Config, wantHost.LogConfig.Config) ||
		got.HostConfig.RestartPolicy != wantHost.RestartPolicy || got.HostConfig.AutoRemove != wantHost.AutoRemove || !optionalBoolEqual(got.HostConfig.Init, wantHost.Init) {
		return fmt.Errorf("container lifecycle configuration drifted")
	}
	if got.HostConfig.IpcMode != wantHost.IpcMode || got.HostConfig.PidMode != wantHost.PidMode || got.HostConfig.UTSMode != wantHost.UTSMode ||
		got.HostConfig.UsernsMode != wantHost.UsernsMode || got.HostConfig.CgroupnsMode != wantHost.CgroupnsMode || got.HostConfig.Cgroup != wantHost.Cgroup {
		return fmt.Errorf("container namespace configuration drifted")
	}
	if len(got.HostConfig.Binds) != 0 || len(got.HostConfig.VolumesFrom) != 0 || len(got.HostConfig.Devices) != 0 || len(got.HostConfig.DeviceRequests) != 0 ||
		len(got.HostConfig.PortBindings) != 0 || got.HostConfig.PublishAllPorts || len(got.HostConfig.Links) != 0 || len(got.HostConfig.GroupAdd) != 0 ||
		len(got.HostConfig.DNS) != 0 || len(got.HostConfig.DNSOptions) != 0 || len(got.HostConfig.DNSSearch) != 0 || len(got.HostConfig.ExtraHosts) != 0 ||
		len(got.HostConfig.Sysctls) != 0 || len(got.HostConfig.StorageOpt) != 0 || len(got.HostConfig.Annotations) != 0 || got.HostConfig.Runtime != wantHost.Runtime ||
		got.HostConfig.VolumeDriver != wantHost.VolumeDriver || got.HostConfig.ContainerIDFile != wantHost.ContainerIDFile || got.HostConfig.Isolation != wantHost.Isolation || got.HostConfig.OomScoreAdj != wantHost.OomScoreAdj {
		return fmt.Errorf("container HostConfig contains a forbidden attachment")
	}
	if !reflect.DeepEqual(got.HostConfig.Resources, wantHost.Resources) || got.HostConfig.ShmSize != wantHost.ShmSize {
		return fmt.Errorf("container resource limits drifted")
	}
	if !maps.Equal(got.HostConfig.Tmpfs, wantHost.Tmpfs) {
		return fmt.Errorf("container scratch tmpfs drifted")
	}
	if !equalMountSpecifications(got.HostConfig.Mounts, wantHost.Mounts) {
		return fmt.Errorf("container HostConfig mounts drifted")
	}
	if !equalInspectMounts(got.Mounts, wantHost.Mounts) {
		return fmt.Errorf("container realized mounts drifted")
	}
	return nil
}

func validateTargetCreateOptions(options moby.ContainerCreateOptions) error {
	if options.Config == nil || options.HostConfig == nil {
		return fmt.Errorf("target Config and HostConfig are required")
	}
	if options.Image != "" || options.NetworkingConfig != nil || options.Platform != nil {
		return fmt.Errorf("target uses a create shortcut, network attachment, or platform override")
	}
	if !dockerResourceName.MatchString(options.Name) {
		return fmt.Errorf("invalid target name %q", options.Name)
	}
	config, host := options.Config, options.HostConfig
	if _, err := domain.ParseDigest(config.Image); err != nil {
		return fmt.Errorf("target image: %w", err)
	}
	if config.User != targetUser || config.WorkingDir != targetWorkingDir || len(config.Entrypoint) == 0 || len(config.Cmd) != 0 || len(config.Shell) != 0 {
		return fmt.Errorf("target portable execution contract is invalid")
	}
	if _, ok := strictEnvironment(config.Env); !ok {
		return fmt.Errorf("target environment contains malformed or duplicate entries")
	}
	if config.Labels["org.cpgen.call"] == "" || config.Labels["org.cpgen.plan-digest"] == "" || config.Labels["org.cpgen.engine-digest"] == "" {
		return fmt.Errorf("target ownership labels are incomplete")
	}
	if len(config.Volumes) != 0 || len(config.ExposedPorts) != 0 || config.Healthcheck != nil || len(config.OnBuild) != 0 || config.Tty || !config.NetworkDisabled {
		return fmt.Errorf("target portable configuration exposes an unsafe feature")
	}
	if !host.ReadonlyRootfs || !host.NetworkMode.IsNone() || !slices.Equal(host.CapDrop, []string{"ALL"}) || len(host.CapAdd) != 0 ||
		!slices.Equal(host.SecurityOpt, []string{"no-new-privileges"}) || host.Privileged || host.AutoRemove || host.LogConfig.Type != "none" || !host.RestartPolicy.IsNone() ||
		host.Init == nil || *host.Init {
		return fmt.Errorf("target HostConfig security contract is invalid")
	}
	if host.IpcMode != container.IPCModePrivate || host.CgroupnsMode != container.CgroupnsModePrivate || host.PidMode != "" || host.UTSMode != "" || host.UsernsMode != "" || host.Cgroup != "" || host.Runtime != "runc" {
		return fmt.Errorf("target namespace or runtime contract is invalid")
	}
	if host.Memory <= 0 || host.MemorySwap != host.Memory || host.PidsLimit == nil || *host.PidsLimit <= 0 || host.ShmSize <= 0 {
		return fmt.Errorf("target resource limits are incomplete")
	}
	if len(host.Binds) != 0 || len(host.VolumesFrom) != 0 || len(host.Devices) != 0 || len(host.DeviceRequests) != 0 || len(host.PortBindings) != 0 || host.PublishAllPorts || len(host.Links) != 0 || len(host.GroupAdd) != 0 ||
		len(host.DNS) != 0 || len(host.DNSOptions) != 0 || len(host.DNSSearch) != 0 || len(host.ExtraHosts) != 0 || len(host.Sysctls) != 0 || len(host.StorageOpt) != 0 {
		return fmt.Errorf("target HostConfig contains a forbidden attachment")
	}
	if len(host.Tmpfs) != 1 || host.Tmpfs[targetWorkingDir] == "" {
		return fmt.Errorf("target scratch tmpfs is missing or expanded")
	}
	if err := validateMountSpecifications(host.Mounts); err != nil {
		return err
	}
	return nil
}

var dockerResourceName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func compileCommand(compiler toolchain.Toolchain, entry domain.SafeRelPath) ([]string, error) {
	command := slices.Clone(compiler.Command)
	replacements := 0
	for index, argument := range command {
		if argument == "/src/<entry>" {
			command[index] = "/src/" + string(entry)
			replacements++
		}
	}
	if replacements != 1 {
		return nil, fmt.Errorf("locked compiler command must contain exactly one entry placeholder")
	}
	return command, nil
}

func builderEnvironment(locked map[string]string) map[string]string {
	environment := maps.Clone(locked)
	for key, value := range map[string]string{
		"HOME": targetHome, "TMPDIR": targetTemp, "GOTMPDIR": targetTemp, "GOCACHE": targetGoCache,
		"GOPATH": targetGoPath, "GOTOOLCHAIN": "local", "GOLANG_VERSION": baseGoVersion, "PATH": builderPath,
	} {
		environment[key] = value
	}
	return environment
}

func runtimeEnvironment() map[string]string {
	return map[string]string{
		"LANG": "C.UTF-8", "TZ": "UTC", "HOME": targetHome, "TMPDIR": targetTemp, "PATH": runtimePath,
	}
}

func sortedEnvironment(environment map[string]string) []string {
	keys := make([]string, 0, len(environment))
	for key := range environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	result := make([]string, 0, len(keys))
	for _, key := range keys {
		result = append(result, key+"="+environment[key])
	}
	return result
}

func strictEnvironment(environment []string) (map[string]string, bool) {
	result := make(map[string]string, len(environment))
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return nil, false
		}
		if _, exists := result[key]; exists {
			return nil, false
		}
		result[key] = value
	}
	return result, true
}

func equalEnvironment(left, right []string) bool {
	leftMap, leftOK := strictEnvironment(left)
	rightMap, rightOK := strictEnvironment(right)
	return leftOK && rightOK && maps.Equal(leftMap, rightMap)
}

func volumeMount(source, target string, readOnly bool) mount.Mount {
	return mount.Mount{
		Type:          mount.TypeVolume,
		Source:        source,
		Target:        target,
		ReadOnly:      readOnly,
		VolumeOptions: &mount.VolumeOptions{NoCopy: true},
	}
}

func scratchTmpfs(size int64) string {
	return fmt.Sprintf("rw,nosuid,nodev,noexec,size=%d,uid=65532,gid=65532,mode=0755", size)
}

func scratchBytes(memory int64) int64 {
	value := memory / 4
	if value < targetScratchFloor {
		value = targetScratchFloor
	}
	if value > targetScratchMax {
		value = targetScratchMax
	}
	return value
}

func shmBytes(memory int64) int64 {
	value := memory / 8
	if value < 64<<10 {
		value = 64 << 10
	}
	if value > targetShmMaxBytes {
		value = targetShmMaxBytes
	}
	return value
}

func volumeResources(plan port.ContainerPlan) []port.PlannedResource {
	var resources []port.PlannedResource
	for _, resource := range plan.Resources {
		if resource.Kind == port.ResourceVolume {
			resources = append(resources, resource)
		}
	}
	return resources
}

func targetResource(plan port.ContainerPlan) (port.PlannedResource, error) {
	var target *port.PlannedResource
	for index := range plan.Resources {
		resource := &plan.Resources[index]
		if resource.Kind == port.ResourceContainer && resource.Role == port.ResourceTarget {
			if target != nil {
				return port.PlannedResource{}, fmt.Errorf("plan contains multiple target containers")
			}
			target = resource
		}
	}
	if target == nil {
		return port.PlannedResource{}, fmt.Errorf("plan has no target container")
	}
	return *target, nil
}

func validateMountSpecifications(mounts []mount.Mount) error {
	seen := make(map[string]struct{}, len(mounts))
	for _, item := range mounts {
		if _, exists := seen[item.Target]; exists {
			return fmt.Errorf("duplicate target mount %q", item.Target)
		}
		seen[item.Target] = struct{}{}
		switch item.Type {
		case mount.TypeVolume:
			if item.Source == "" || item.VolumeOptions == nil || !item.VolumeOptions.NoCopy || item.BindOptions != nil || item.TmpfsOptions != nil {
				return fmt.Errorf("unsafe Engine volume mount at %q", item.Target)
			}
			allowedReadOnly := item.Target == "/src" || item.Target == "/program" || item.Target == "/input"
			allowedWritable := item.Target == "/result" || item.Target == "/result/files"
			if item.ReadOnly != allowedReadOnly || !allowedReadOnly && !allowedWritable {
				return fmt.Errorf("volume mount at %q violates the fixed allowlist", item.Target)
			}
		default:
			return fmt.Errorf("forbidden mount type %q", item.Type)
		}
	}
	return nil
}

func equalMountSpecifications(left, right []mount.Mount) bool {
	if len(left) != len(right) {
		return false
	}
	leftByTarget := make(map[string]mount.Mount, len(left))
	for _, item := range left {
		if _, exists := leftByTarget[item.Target]; exists {
			return false
		}
		leftByTarget[item.Target] = item
	}
	for _, item := range right {
		candidate, ok := leftByTarget[item.Target]
		if !ok || !reflect.DeepEqual(candidate, item) {
			return false
		}
	}
	return true
}

func equalInspectMounts(actual []container.MountPoint, expected []mount.Mount) bool {
	if len(actual) != len(expected) {
		return false
	}
	expectedByTarget := make(map[string]mount.Mount, len(expected))
	for _, item := range expected {
		expectedByTarget[item.Target] = item
	}
	seen := make(map[string]struct{}, len(actual))
	for _, point := range actual {
		want, ok := expectedByTarget[point.Destination]
		if !ok {
			return false
		}
		if _, duplicate := seen[point.Destination]; duplicate {
			return false
		}
		seen[point.Destination] = struct{}{}
		if point.Type != want.Type || point.RW == want.ReadOnly {
			return false
		}
		if want.Type == mount.TypeVolume && point.Name != want.Source {
			return false
		}
		if want.Type == mount.TypeTmpfs && (point.Name != "" || point.Source != "") {
			return false
		}
	}
	return true
}

func optionalIntEqual(left, right *int) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func optionalBoolEqual(left, right *bool) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
