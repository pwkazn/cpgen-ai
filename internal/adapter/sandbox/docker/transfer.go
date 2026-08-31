package docker

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/transfer"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	moby "github.com/moby/moby/client"
)

const (
	transferBinary       = "/usr/local/bin/cpgen-transfer"
	transferVolumeRoot   = "/volume"
	helperStderrMaxBytes = 64 << 10
)

func (op *operation) runImport(ctx context.Context, group payloadGroup) error {
	if len(group.files) == 0 {
		return fmt.Errorf("import group must contain at least one file")
	}
	var total int64
	for _, file := range group.files {
		if int64(len(file.Data)) > op.runner.limits.MaxTransferBytes-total {
			return fmt.Errorf("import payload exceeds control transfer limit")
		}
		total += int64(len(file.Data))
	}
	command := []string{
		transferBinary, "import", "--root", transferVolumeRoot,
		"--max-files", strconv.Itoa(len(group.files)),
		"--max-total-bytes", strconv.FormatInt(total, 10),
	}
	factory := func(resource port.PlannedResource, callID domain.AttemptCallID) (moby.ContainerCreateOptions, error) {
		return op.helperCreateOptions(resource, callID, "0:0", command, true, true, true, []mount.Mount{
			volumeMount(group.volume.DeterministicName, transferVolumeRoot, false),
		})
	}
	helper, _, err := op.createContainer(ctx, port.ContainerImport, factory)
	if err != nil {
		return err
	}
	attached, err := op.runner.engine.ContainerAttach(ctx, helper.id, moby.ContainerAttachOptions{
		Stream: true, Stdin: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return err
	}
	defer attached.Close()

	stderr := &boundedCapture{max: helperStderrMaxBytes}
	drainDone := make(chan error, 1)
	go func() {
		_, copyErr := stdcopy.StdCopy(discardWriter{}, stderr, attached.Reader)
		drainDone <- copyErr
	}()
	if err := op.startContainer(ctx, helper); err != nil {
		return err
	}
	_, writeErr := transfer.WriteStream(ctx, attached.Conn, group.files)
	closeWriteErr := attached.CloseWrite()
	status, waitErr := waitContainer(ctx, op.runner.engine, helper.id)
	helper.running = false
	attached.Close()
	drainErr := <-drainDone
	if writeErr != nil || closeWriteErr != nil || waitErr != nil || status != 0 {
		return errors.Join(
			writeErr,
			closeWriteErr,
			waitErr,
			helperExitError("import", status, stderr.String()),
		)
	}
	if drainErr != nil && !isClosedStreamError(drainErr) {
		return fmt.Errorf("drain import helper output: %w", drainErr)
	}
	return nil
}

func (op *operation) startKeeper(ctx context.Context) (*ownedContainer, error) {
	output, err := outputVolume(op.plan)
	if err != nil {
		return nil, err
	}
	factory := func(resource port.PlannedResource, callID domain.AttemptCallID) (moby.ContainerCreateOptions, error) {
		return op.helperCreateOptions(resource, callID, targetUser, []string{transferBinary, "keep"}, false, false, false, []mount.Mount{
			volumeMount(output.DeterministicName, transferVolumeRoot, false),
		})
	}
	keeper, _, err := op.createContainer(ctx, port.ContainerKeeper, factory)
	if err != nil {
		return nil, err
	}
	if err := op.startContainer(ctx, keeper); err != nil {
		return nil, err
	}
	return keeper, nil
}

func (op *operation) runExport(ctx context.Context, artifacts []*preparedArtifact, maxTotalBytes int64) error {
	if len(artifacts) == 0 || maxTotalBytes <= 0 {
		return fmt.Errorf("export requires declared artifacts and a positive total limit")
	}
	if err := op.verifyTargetStopped(ctx); err != nil {
		return err
	}
	output, err := outputVolume(op.plan)
	if err != nil {
		return err
	}
	declarations := make([]port.OutputDeclaration, 0, len(artifacts))
	targets := make([]transfer.ReceiveTarget, 0, len(artifacts))
	for _, artifact := range artifacts {
		declarations = append(declarations, artifact.frame)
		targets = append(targets, transfer.ReceiveTarget{Declaration: artifact.frame, Writer: artifact.writer})
	}
	exportPlan := transfer.ExportPlan{
		SchemaVersion: transfer.ExportPlanSchemaVersion,
		Files:         declarations, MaxFiles: len(declarations), MaxTotalBytes: maxTotalBytes,
	}
	if err := exportPlan.Validate(); err != nil {
		return err
	}
	encodedPlan, err := json.Marshal(exportPlan)
	if err != nil {
		return fmt.Errorf("encode export plan: %w", err)
	}
	command := []string{
		transferBinary, "export", "--root", transferVolumeRoot,
		"--plan-base64", base64.StdEncoding.EncodeToString(encodedPlan),
	}
	factory := func(resource port.PlannedResource, callID domain.AttemptCallID) (moby.ContainerCreateOptions, error) {
		return op.helperCreateOptions(resource, callID, targetUser, command, false, true, true, []mount.Mount{
			volumeMount(output.DeterministicName, transferVolumeRoot, true),
		})
	}
	helper, exportCall, err := op.createContainer(ctx, port.ContainerExport, factory)
	if err != nil {
		return err
	}
	attached, err := op.runner.engine.ContainerAttach(ctx, helper.id, moby.ContainerAttachOptions{
		Stream: true, Stdout: true, Stderr: true,
	})
	if err != nil {
		return err
	}
	defer attached.Close()

	stdoutReader, stdoutWriter := io.Pipe()
	stderr := &boundedCapture{max: helperStderrMaxBytes}
	copyDone := make(chan error, 1)
	go func() {
		_, copyErr := stdcopy.StdCopy(stdoutWriter, stderr, attached.Reader)
		_ = stdoutWriter.CloseWithError(copyErr)
		copyDone <- copyErr
	}()
	if err := op.startContainer(ctx, helper); err != nil {
		attached.Close()
		<-copyDone
		return err
	}
	exported, receiveErr := transfer.ReceiveExport(ctx, stdoutReader, targets, maxTotalBytes)
	_ = stdoutReader.Close()
	if receiveErr != nil {
		attached.Close()
		<-copyDone
		return receiveErr
	}
	status, waitErr := waitContainer(ctx, op.runner.engine, helper.id)
	helper.running = false
	copyErr := <-copyDone
	if waitErr != nil || status != 0 {
		return errors.Join(waitErr, helperExitError("export", status, stderr.String()))
	}
	if copyErr != nil && !isClosedStreamError(copyErr) {
		return fmt.Errorf("receive export helper output: %w", copyErr)
	}
	if len(exported) != len(artifacts) {
		return fmt.Errorf("export returned %d files for %d artifacts", len(exported), len(artifacts))
	}
	byPath := make(map[domain.SafeRelPath]transfer.ExportedFile, len(exported))
	for _, file := range exported {
		byPath[file.Path] = file
	}
	for _, artifact := range artifacts {
		file, ok := byPath[artifact.frame.Path]
		if !ok || file.Mode != artifact.wantMode {
			return fmt.Errorf("export path %q has an invalid or missing mode", artifact.frame.Path)
		}
	}
	for _, artifact := range artifacts {
		file := byPath[artifact.frame.Path]
		pending, err := artifact.writer.Finalize(ctx)
		if err != nil {
			return err
		}
		if pending.Blob.Digest != file.Digest || pending.Blob.Size != file.Size ||
			pending.MediaType != artifact.declaration.MediaType || pending.Role != artifact.declaration.Role ||
			pending.LogicalPath != artifact.declaration.LogicalPath || pending.Provenance != artifact.declaration.Provenance {
			return fmt.Errorf("finalized artifact %q does not match the verified export", artifact.declaration.LogicalPath)
		}
		pending.CallID = exportCall
		if err := pending.Validate(); err != nil {
			return fmt.Errorf("finalized artifact %q: %w", artifact.declaration.LogicalPath, err)
		}
		artifact.pending = pending
		artifact.finalized = true
	}
	return nil
}

func (op *operation) verifyTargetStopped(ctx context.Context) error {
	for index := len(op.containers) - 1; index >= 0; index-- {
		target := op.containers[index]
		if target.resource.Role != port.ResourceTarget {
			continue
		}
		if target.running {
			return fmt.Errorf("target is still running before export")
		}
		inspected, err := op.runner.engine.ContainerInspect(ctx, target.id, moby.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		if err := verifyContainerOwnership(inspected, target); err != nil {
			return err
		}
		if inspected.Container.State == nil || inspected.Container.State.Running {
			return fmt.Errorf("target is not STOPPED before export dispatch")
		}
		return nil
	}
	return fmt.Errorf("target container is missing before export")
}

func outputVolume(plan port.ContainerPlan) (port.PlannedResource, error) {
	for _, resource := range plan.Resources {
		if resource.Kind == port.ResourceVolume && resource.Role == port.ResourceOutput {
			return resource, nil
		}
	}
	return port.PlannedResource{}, fmt.Errorf("output volume is missing")
}

func (op *operation) helperCreateOptions(resource port.PlannedResource, callID domain.AttemptCallID, user string, command []string, stdin, stdout, stderr bool, mounts []mount.Mount) (moby.ContainerCreateOptions, error) {
	labels, err := ResourceLabels(op.identity, op.plan, resource, &callID)
	if err != nil {
		return moby.ContainerCreateOptions{}, err
	}
	for key, value := range op.runner.lock.Transfer.Labels {
		if current, exists := labels[key]; exists && current != value {
			return moby.ContainerCreateOptions{}, fmt.Errorf("transfer image label %q conflicts with resource identity", key)
		}
		labels[key] = value
	}
	pids := op.runner.limits.HelperPIDs
	oomKillEnabled := false
	initDisabled := false
	config := &container.Config{
		User: user, AttachStdin: stdin, AttachStdout: stdout, AttachStderr: stderr,
		OpenStdin: stdin, StdinOnce: stdin, Tty: false,
		Image: string(op.runner.lock.Transfer.ImageID), Entrypoint: slices.Clone(command), Cmd: nil,
		WorkingDir: "/", NetworkDisabled: true, Labels: labels, StopSignal: "SIGTERM",
	}
	host := &container.HostConfig{
		LogConfig:   container.LogConfig{Type: "none", Config: map[string]string{}},
		NetworkMode: container.NetworkMode(network.NetworkNone), RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		CapDrop: []string{"ALL"}, CgroupnsMode: container.CgroupnsModePrivate, IpcMode: container.IPCModePrivate,
		ReadonlyRootfs: true, SecurityOpt: []string{"no-new-privileges"}, Runtime: "runc", ShmSize: 1 << 20,
		Resources: container.Resources{
			Memory: op.runner.limits.HelperMemoryBytes, MemorySwap: op.runner.limits.HelperMemoryBytes,
			OomKillDisable: &oomKillEnabled, PidsLimit: &pids,
			Ulimits: []*container.Ulimit{
				{Name: "core", Soft: 0, Hard: 0},
				{Name: "fsize", Soft: op.runner.limits.MaxTransferBytes, Hard: op.runner.limits.MaxTransferBytes},
				{Name: "nofile", Soft: 64, Hard: 64},
				{Name: "nproc", Soft: pids, Hard: pids},
			},
		},
		Mounts: slices.Clone(mounts), Init: &initDisabled,
	}
	options := moby.ContainerCreateOptions{Config: config, HostConfig: host, Name: resource.DeterministicName}
	if err := validateHelperCreateOptions(options, resource, op.runner.lock.Transfer.ImageID); err != nil {
		return moby.ContainerCreateOptions{}, err
	}
	return options, nil
}

func validateHelperCreateOptions(options moby.ContainerCreateOptions, resource port.PlannedResource, imageID domain.Digest) error {
	if options.Config == nil || options.HostConfig == nil || options.Name != resource.DeterministicName {
		return fmt.Errorf("helper specification is incomplete")
	}
	config, host := options.Config, options.HostConfig
	if config.Image != string(imageID) || len(config.Entrypoint) < 2 || config.Entrypoint[0] != transferBinary || len(config.Cmd) != 0 || config.Tty || !config.NetworkDisabled {
		return fmt.Errorf("helper command or image is outside the closed contract")
	}
	if config.Labels["org.cpgen.role"] != string(resource.Role) || config.Labels["org.cpgen.call"] == "" {
		return fmt.Errorf("helper ownership labels are incomplete")
	}
	if !host.ReadonlyRootfs || !host.NetworkMode.IsNone() || !slices.Equal(host.CapDrop, []string{"ALL"}) || len(host.CapAdd) != 0 || host.Privileged || host.AutoRemove ||
		!host.RestartPolicy.IsNone() || host.LogConfig.Type != "none" || !slices.Equal(host.SecurityOpt, []string{"no-new-privileges"}) || host.Runtime != "runc" ||
		host.IpcMode != container.IPCModePrivate || host.CgroupnsMode != container.CgroupnsModePrivate || host.Init == nil || *host.Init {
		return fmt.Errorf("helper isolation contract is invalid")
	}
	if host.Memory <= 0 || host.MemorySwap != host.Memory || host.PidsLimit == nil || *host.PidsLimit <= 0 || len(host.Mounts) != 1 {
		return fmt.Errorf("helper resource or mount contract is incomplete")
	}
	item := host.Mounts[0]
	if item.Type != mount.TypeVolume || item.Source == "" || item.Target != transferVolumeRoot || item.VolumeOptions == nil || !item.VolumeOptions.NoCopy {
		return fmt.Errorf("helper volume mount is outside the closed contract")
	}
	return nil
}

// VerifyHelperInspect rejects daemon- or image-injected changes to the closed
// transfer-helper specification before a helper is allowed to start.
func VerifyHelperInspect(expected moby.ContainerCreateOptions, actual moby.ContainerInspectResult) error {
	if expected.Config == nil {
		return fmt.Errorf("invalid expected helper specification: Config is required")
	}
	resource := port.PlannedResource{DeterministicName: expected.Name}
	resource.Role = port.ResourceRole(expected.Config.Labels["org.cpgen.role"])
	if err := validateHelperCreateOptions(expected, resource, domain.Digest(expected.Config.Image)); err != nil {
		return fmt.Errorf("invalid expected helper specification: %w", err)
	}
	got := actual.Container
	if got.Config == nil || got.HostConfig == nil {
		return fmt.Errorf("helper inspect omitted Config or HostConfig")
	}
	wantConfig, wantHost := expected.Config, expected.HostConfig
	if trimContainerName(got.Name) != expected.Name || got.Image != wantConfig.Image || got.Config.Image != wantConfig.Image ||
		got.Config.User != wantConfig.User || got.Config.WorkingDir != wantConfig.WorkingDir ||
		!slices.Equal(got.Config.Entrypoint, wantConfig.Entrypoint) || !slices.Equal(got.Config.Cmd, wantConfig.Cmd) ||
		got.Config.AttachStdin != wantConfig.AttachStdin || got.Config.AttachStdout != wantConfig.AttachStdout || got.Config.AttachStderr != wantConfig.AttachStderr ||
		got.Config.OpenStdin != wantConfig.OpenStdin || got.Config.StdinOnce != wantConfig.StdinOnce || got.Config.Tty ||
		got.Config.NetworkDisabled != wantConfig.NetworkDisabled || !maps.Equal(got.Config.Labels, wantConfig.Labels) ||
		got.Config.StopSignal != wantConfig.StopSignal {
		return fmt.Errorf("helper portable configuration drifted")
	}
	if got.HostConfig.ReadonlyRootfs != wantHost.ReadonlyRootfs || got.HostConfig.NetworkMode != wantHost.NetworkMode ||
		!slices.Equal(got.HostConfig.CapDrop, wantHost.CapDrop) || len(got.HostConfig.CapAdd) != 0 ||
		!slices.Equal(got.HostConfig.SecurityOpt, wantHost.SecurityOpt) || got.HostConfig.Privileged ||
		got.HostConfig.LogConfig.Type != wantHost.LogConfig.Type || !maps.Equal(got.HostConfig.LogConfig.Config, wantHost.LogConfig.Config) ||
		got.HostConfig.RestartPolicy != wantHost.RestartPolicy || got.HostConfig.AutoRemove != wantHost.AutoRemove ||
		got.HostConfig.IpcMode != wantHost.IpcMode || got.HostConfig.CgroupnsMode != wantHost.CgroupnsMode ||
		got.HostConfig.Runtime != wantHost.Runtime || !optionalBoolEqual(got.HostConfig.Init, wantHost.Init) ||
		got.HostConfig.ShmSize != wantHost.ShmSize || !reflect.DeepEqual(got.HostConfig.Resources, wantHost.Resources) ||
		!equalMountSpecifications(got.HostConfig.Mounts, wantHost.Mounts) || !equalInspectMounts(got.Mounts, wantHost.Mounts) {
		return fmt.Errorf("helper HostConfig or realized mounts drifted")
	}
	return nil
}

func (op *operation) startContainer(ctx context.Context, owned *ownedContainer) error {
	if _, err := op.runner.engine.ContainerStart(ctx, owned.id, moby.ContainerStartOptions{}); err != nil {
		return err
	}
	owned.running = true
	return nil
}

func helperExitError(role string, status int64, stderr string) error {
	if status == 0 {
		return nil
	}
	message := strings.TrimSpace(stderr)
	if message == "" {
		return fmt.Errorf("%s helper exited with status %d", role, status)
	}
	return fmt.Errorf("%s helper exited with status %d: %s", role, status, message)
}

type boundedCapture struct {
	buffer bytes.Buffer
	max    int
}

func (w *boundedCapture) Write(data []byte) (int, error) {
	original := len(data)
	remaining := w.max - w.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = w.buffer.Write(data)
	}
	return original, nil
}

func (w *boundedCapture) String() string { return w.buffer.String() }

type discardWriter struct{}

func (discardWriter) Write(data []byte) (int, error) { return len(data), nil }

func isClosedStreamError(err error) bool {
	return errors.Is(err, context.Canceled) || strings.Contains(strings.ToLower(err.Error()), "closed")
}
