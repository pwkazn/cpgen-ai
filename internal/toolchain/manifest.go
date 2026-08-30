package toolchain

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"

	"cpgen/internal/domain"
	"cpgen/internal/port"
)

const (
	LockSchemaVersion domain.SchemaVersion = "cpgen.toolchain-lock/v1"
	ExecutionProtocol                      = "docker-direct-v2"

	GoBaseRef     = "golang:1.24.13-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac"
	DebianBaseRef = "debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171"

	TestlibVersion = "0.9.45"
	TestlibCommit  = "1e4e8a24c79c6bad3becbdb5a332ffc352b7d5dd"
	TestlibHash    = "sha256:bb323e3c89285214966076e0d23d5a295c5f6126da7ff198c1276ddb95ecb1a0"
	TestlibPath    = "third_party/testlib/testlib.h"

	CPP20ToolchainID port.ToolchainID = "cpp20-gcc-bookworm-v1"
	GoToolchainID    port.ToolchainID = "go124-bookworm-v1"
)

var cpp20Command = []string{
	"/usr/bin/g++", "-std=c++20", "-O2", "-pipe", "-static", "-s",
	"-I/opt/cpgen/include", "/src/<entry>", "-o", "/result/files/main",
}

var goCommand = []string{
	"/usr/local/go/bin/go", "build", "-trimpath", "-ldflags=-s -w -buildid=",
	"-o", "/result/files/main", "/src/<entry>",
}

var commonEnvironment = map[string]string{"LANG": "C.UTF-8", "TZ": "UTC"}
var goEnvironment = map[string]string{
	"CGO_ENABLED": "0", "GO111MODULE": "off", "GOPROXY": "off", "GOSUMDB": "off",
	"LANG": "C.UTF-8", "TZ": "UTC",
}

type Lock struct {
	SchemaVersion     domain.SchemaVersion `json:"schema_version"`
	ExecutionProtocol string               `json:"execution_protocol"`
	Builder           Image                `json:"builder"`
	Runtime           Image                `json:"runtime"`
	Transfer          Image                `json:"transfer"`
	Toolchains        []Toolchain          `json:"toolchains"`
	Testlib           Testlib              `json:"testlib"`
}

type Image struct {
	Role    string            `json:"role"`
	ImageID domain.Digest     `json:"image_id"`
	BaseRef string            `json:"base_ref"`
	Labels  map[string]string `json:"labels"`
}

type Toolchain struct {
	ID          port.ToolchainID   `json:"id"`
	Language    port.Language      `json:"language"`
	ImageID     domain.Digest      `json:"image_id"`
	Command     []string           `json:"command"`
	Environment map[string]string  `json:"environment"`
	OutputPath  domain.SafeRelPath `json:"output_path"`
}

type Testlib struct {
	Version string             `json:"version"`
	Commit  string             `json:"commit"`
	SHA256  domain.Digest      `json:"sha256"`
	Path    domain.SafeRelPath `json:"path"`
}

func LoadLock(reader io.Reader) (Lock, error) {
	if reader == nil {
		return Lock{}, fmt.Errorf("toolchain lock reader is required")
	}
	const maximumLockBytes = 1 << 20
	data, err := io.ReadAll(io.LimitReader(reader, maximumLockBytes+1))
	if err != nil {
		return Lock{}, fmt.Errorf("read toolchain lock: %w", err)
	}
	if len(data) > maximumLockBytes {
		return Lock{}, fmt.Errorf("toolchain lock exceeds %d bytes", maximumLockBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var lock Lock
	if err := decoder.Decode(&lock); err != nil {
		return Lock{}, fmt.Errorf("decode toolchain lock: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return Lock{}, fmt.Errorf("toolchain lock contains a trailing JSON value")
		}
		return Lock{}, fmt.Errorf("decode trailing toolchain lock JSON: %w", err)
	}
	if err := lock.Validate(); err != nil {
		return Lock{}, err
	}
	return lock, nil
}

func NewDockerV1Lock(builderID, runtimeID, transferID domain.Digest) (Lock, error) {
	lock := Lock{
		SchemaVersion:     LockSchemaVersion,
		ExecutionProtocol: ExecutionProtocol,
		Builder:           Image{Role: "builder", ImageID: builderID, BaseRef: GoBaseRef, Labels: RequiredImageLabels("builder")},
		Runtime:           Image{Role: "runtime", ImageID: runtimeID, BaseRef: DebianBaseRef, Labels: RequiredImageLabels("runtime")},
		Transfer:          Image{Role: "transfer", ImageID: transferID, BaseRef: DebianBaseRef, Labels: RequiredImageLabels("transfer")},
		Toolchains: []Toolchain{
			{
				ID: CPP20ToolchainID, Language: port.LanguageCPP20, ImageID: builderID,
				Command: slices.Clone(cpp20Command), Environment: maps.Clone(commonEnvironment), OutputPath: "result/files/main",
			},
			{
				ID: GoToolchainID, Language: port.LanguageGo, ImageID: builderID,
				Command: slices.Clone(goCommand), Environment: maps.Clone(goEnvironment), OutputPath: "result/files/main",
			},
		},
		Testlib: Testlib{Version: TestlibVersion, Commit: TestlibCommit, SHA256: domain.Digest(TestlibHash), Path: TestlibPath},
	}
	if err := lock.Validate(); err != nil {
		return Lock{}, err
	}
	return lock, nil
}

func RequiredImageLabels(role string) map[string]string {
	labels := map[string]string{
		"org.cpgen.execution-protocol": ExecutionProtocol,
		"org.cpgen.image-role":         role,
		"org.cpgen.image-schema":       "cpgen.image/v1",
	}
	if role == "builder" {
		labels["org.cpgen.testlib-sha256"] = TestlibHash[len("sha256:"):]
	}
	return labels
}

func (l Lock) Validate() error {
	if l.SchemaVersion != LockSchemaVersion {
		return fmt.Errorf("toolchain lock schema version must be %q", LockSchemaVersion)
	}
	if l.ExecutionProtocol != ExecutionProtocol {
		return fmt.Errorf("execution protocol must be %q", ExecutionProtocol)
	}
	images := []struct {
		name    string
		image   Image
		baseRef string
	}{
		{name: "builder", image: l.Builder, baseRef: GoBaseRef},
		{name: "runtime", image: l.Runtime, baseRef: DebianBaseRef},
		{name: "transfer", image: l.Transfer, baseRef: DebianBaseRef},
	}
	seenImages := make(map[domain.Digest]struct{}, len(images))
	for _, item := range images {
		if item.image.Role != item.name {
			return fmt.Errorf("%s image role is %q", item.name, item.image.Role)
		}
		if err := item.image.ImageID.Validate(); err != nil {
			return fmt.Errorf("%s image ID: %w", item.name, err)
		}
		if item.image.BaseRef != item.baseRef {
			return fmt.Errorf("%s base image must be pinned to %q", item.name, item.baseRef)
		}
		if !maps.Equal(item.image.Labels, RequiredImageLabels(item.name)) {
			return fmt.Errorf("%s image labels do not match the fixed contract", item.name)
		}
		if _, exists := seenImages[item.image.ImageID]; exists {
			return fmt.Errorf("sandbox image IDs must be distinct")
		}
		seenImages[item.image.ImageID] = struct{}{}
	}

	if len(l.Toolchains) != 2 {
		return fmt.Errorf("toolchain lock must contain exactly C++20 and Go toolchains")
	}
	seenToolchains := make(map[port.ToolchainID]struct{}, len(l.Toolchains))
	for _, candidate := range l.Toolchains {
		if err := candidate.ID.Validate(); err != nil {
			return err
		}
		if _, exists := seenToolchains[candidate.ID]; exists {
			return fmt.Errorf("duplicate toolchain ID %q", candidate.ID)
		}
		seenToolchains[candidate.ID] = struct{}{}
		if candidate.ImageID != l.Builder.ImageID {
			return fmt.Errorf("toolchain %q does not use the pinned builder image", candidate.ID)
		}
		if candidate.OutputPath != "result/files/main" {
			return fmt.Errorf("toolchain %q has an unexpected output path", candidate.ID)
		}
		switch candidate.ID {
		case CPP20ToolchainID:
			if candidate.Language != port.LanguageCPP20 || !slices.Equal(candidate.Command, cpp20Command) || !maps.Equal(candidate.Environment, commonEnvironment) {
				return fmt.Errorf("C++20 compiler command or environment does not match the fixed contract")
			}
		case GoToolchainID:
			if candidate.Language != port.LanguageGo || !slices.Equal(candidate.Command, goCommand) || !maps.Equal(candidate.Environment, goEnvironment) {
				return fmt.Errorf("Go compiler command or environment does not match the fixed contract")
			}
		default:
			return fmt.Errorf("unknown toolchain ID %q", candidate.ID)
		}
	}
	if _, ok := seenToolchains[CPP20ToolchainID]; !ok {
		return fmt.Errorf("C++20 toolchain is missing")
	}
	if _, ok := seenToolchains[GoToolchainID]; !ok {
		return fmt.Errorf("Go toolchain is missing")
	}

	if l.Testlib.Version != TestlibVersion || l.Testlib.Commit != TestlibCommit || string(l.Testlib.SHA256) != TestlibHash || l.Testlib.Path != TestlibPath {
		return fmt.Errorf("testlib provenance does not match the pinned source")
	}
	if err := l.Testlib.SHA256.Validate(); err != nil {
		return fmt.Errorf("testlib digest: %w", err)
	}
	return l.Testlib.Path.Validate()
}

func (l Lock) Digest() (domain.Digest, error) {
	if err := l.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(l)
	if err != nil {
		return "", fmt.Errorf("encode toolchain lock: %w", err)
	}
	return domain.SumBytes(encoded), nil
}

func (l Lock) MarshalIndent() ([]byte, error) {
	if err := l.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode toolchain lock: %w", err)
	}
	return append(encoded, '\n'), nil
}
