package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"cpgen/internal/domain"
	moby "github.com/moby/moby/client"
)

type StaticReport struct {
	SchemaVersion        string        `json:"schema_version"`
	EndpointDigest       domain.Digest `json:"endpoint_digest"`
	DaemonID             string        `json:"daemon_id"`
	ServerVersion        string        `json:"server_version"`
	APIVersion           string        `json:"api_version"`
	MinimumAPIVersion    string        `json:"minimum_api_version"`
	ServerOS             string        `json:"server_os"`
	Architecture         string        `json:"architecture"`
	CgroupDriver         string        `json:"cgroup_driver"`
	CgroupVersion        string        `json:"cgroup_version"`
	SecurityOptions      []string      `json:"security_options"`
	BuilderImageID       string        `json:"builder_image_id"`
	RuntimeImageID       string        `json:"runtime_image_id"`
	TransferImageID      string        `json:"transfer_image_id"`
	ExecutionProtocol    string        `json:"execution_protocol"`
	EngineIdentityDigest domain.Digest `json:"engine_identity_digest"`
}

type CheckError struct {
	Failure domain.PortFailure
	Cause   error
}

func (e *CheckError) Error() string {
	if e == nil {
		return "Docker static check failed"
	}
	if e.Cause != nil {
		return fmt.Sprintf("Docker static check failed (%s/%s): %v", e.Failure.Code, e.Failure.Class, e.Cause)
	}
	return fmt.Sprintf("Docker static check failed (%s/%s)", e.Failure.Code, e.Failure.Class)
}

func (e *CheckError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

type Doctor struct {
	engine   Engine
	config   Config
	endpoint Endpoint
}

func NewDoctor(engine Engine, config Config, goos string) (*Doctor, error) {
	if engine == nil {
		return nil, fmt.Errorf("Docker Engine client is required")
	}
	endpoint, err := config.Validate(goos)
	if err != nil {
		return nil, err
	}
	return &Doctor{engine: engine, config: config, endpoint: endpoint}, nil
}

func (d *Doctor) Check(ctx context.Context) (StaticReport, error) {
	ping, err := d.engine.Ping(ctx, moby.PingOptions{})
	if err != nil {
		return StaticReport{}, checkFailure(domain.FailureUnavailable, domain.FailureBlocked, fmt.Errorf("ping Docker Engine: %w", err))
	}
	if ping.OSType != "" && ping.OSType != "linux" {
		return StaticReport{}, checkFailure(domain.FailureCapabilityMissing, domain.FailureIncompatible, fmt.Errorf("ping reported server OS %q", ping.OSType))
	}

	version, err := d.engine.ServerVersion(ctx, moby.ServerVersionOptions{})
	if err != nil {
		return StaticReport{}, checkFailure(domain.FailureUnavailable, domain.FailureBlocked, fmt.Errorf("read Docker server version: %w", err))
	}
	if version.Os != "linux" {
		return StaticReport{}, checkFailure(domain.FailureCapabilityMissing, domain.FailureIncompatible, fmt.Errorf("Docker server OS is %q", version.Os))
	}
	if !apiAtLeast(version.APIVersion, 1, 55) {
		return StaticReport{}, checkFailure(domain.FailureVersionMismatch, domain.FailureIncompatible, fmt.Errorf("Docker API %q is below pinned client API %s", version.APIVersion, d.config.APIVersion))
	}

	infoResult, err := d.engine.Info(ctx, moby.InfoOptions{})
	if err != nil {
		return StaticReport{}, checkFailure(domain.FailureUnavailable, domain.FailureBlocked, fmt.Errorf("read Docker server info: %w", err))
	}
	info := infoResult.Info
	if info.ID == "" {
		return StaticReport{}, checkFailure(domain.FailureProtocol, domain.FailureIncompatible, fmt.Errorf("Docker daemon ID is empty"))
	}
	if info.OSType != "" && info.OSType != "linux" {
		return StaticReport{}, checkFailure(domain.FailureCapabilityMissing, domain.FailureIncompatible, fmt.Errorf("Docker info reported server OS %q", info.OSType))
	}

	builderID, err := d.inspectPinnedImage(ctx, "builder", d.config.BuilderImage)
	if err != nil {
		return StaticReport{}, err
	}
	runtimeID, err := d.inspectPinnedImage(ctx, "runtime", d.config.RuntimeImage)
	if err != nil {
		return StaticReport{}, err
	}
	transferID, err := d.inspectPinnedImage(ctx, "transfer", d.config.TransferImage)
	if err != nil {
		return StaticReport{}, err
	}

	securityOptions := append([]string(nil), info.SecurityOptions...)
	sort.Strings(securityOptions)
	report := StaticReport{
		SchemaVersion:     "cpgen.docker-static-report/v1",
		EndpointDigest:    d.endpoint.Digest(),
		DaemonID:          info.ID,
		ServerVersion:     version.Version,
		APIVersion:        version.APIVersion,
		MinimumAPIVersion: version.MinAPIVersion,
		ServerOS:          version.Os,
		Architecture:      version.Arch,
		CgroupDriver:      info.CgroupDriver,
		CgroupVersion:     info.CgroupVersion,
		SecurityOptions:   securityOptions,
		BuilderImageID:    builderID,
		RuntimeImageID:    runtimeID,
		TransferImageID:   transferID,
		ExecutionProtocol: d.config.ExecutionProtocol,
	}
	identityBytes, err := json.Marshal(report)
	if err != nil {
		return StaticReport{}, checkFailure(domain.FailureProtocol, domain.FailureUnknown, fmt.Errorf("encode Engine identity: %w", err))
	}
	report.EngineIdentityDigest = domain.SumBytes(identityBytes)
	return report, nil
}

func (d *Doctor) inspectPinnedImage(ctx context.Context, role, expected string) (string, error) {
	result, err := d.engine.ImageInspect(ctx, expected)
	if err != nil {
		return "", checkFailure(domain.FailureCapabilityMissing, domain.FailureIncompatible, fmt.Errorf("inspect %s image %s: %w", role, expected, err))
	}
	if result.ID != expected {
		return "", checkFailure(domain.FailureCapabilityMissing, domain.FailureIncompatible, fmt.Errorf("%s image ID is %q, want %q", role, result.ID, expected))
	}
	if _, err := domain.ParseDigest(result.ID); err != nil {
		return "", checkFailure(domain.FailureProtocol, domain.FailureIncompatible, fmt.Errorf("%s image returned invalid ID: %w", role, err))
	}
	return result.ID, nil
}

func CheckStatic(ctx context.Context, config Config) (StaticReport, error) {
	engine, err := NewEngineClient(config)
	if err != nil {
		return StaticReport{}, err
	}
	defer engine.Close()
	doctor, err := NewDoctor(engine, config, runtime.GOOS)
	if err != nil {
		return StaticReport{}, err
	}
	return doctor.Check(ctx)
}

func checkFailure(code domain.PortFailureCode, class domain.FailureClass, cause error) error {
	return &CheckError{Failure: domain.PortFailure{Code: code, Class: class}, Cause: cause}
}

func apiAtLeast(raw string, wantMajor, wantMinor int) bool {
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	return major > wantMajor || major == wantMajor && minor >= wantMinor
}
