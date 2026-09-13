package docker

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"strings"

	"cpgen/internal/domain"
)

const RequiredAPIVersion = "1.55"
const ExecutionProtocolDockerDirectV2 = "docker-direct-v2"

type Config struct {
	EngineEndpoint    string
	APIVersion        string
	BuilderImage      string
	RuntimeImage      string
	TransferImage     string
	ExecutionProtocol string
}

type Endpoint struct {
	raw    string
	scheme string
	path   string
	digest domain.Digest
}

func (e Endpoint) String() string        { return e.raw }
func (e Endpoint) Scheme() string        { return e.scheme }
func (e Endpoint) Path() string          { return e.path }
func (e Endpoint) Digest() domain.Digest { return e.digest }

var pipeNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

func ParseLocalEndpoint(raw, goos string) (Endpoint, error) {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return Endpoint{}, fmt.Errorf("explicit Docker Engine endpoint is required")
	}
	switch goos {
	case "windows":
		const prefix = "npipe:////./pipe/"
		if !strings.HasPrefix(raw, prefix) {
			return Endpoint{}, fmt.Errorf("Windows Docker endpoint must use an explicit local npipe transport")
		}
		name := strings.TrimPrefix(raw, prefix)
		if !pipeNamePattern.MatchString(name) {
			return Endpoint{}, fmt.Errorf("invalid local Docker named pipe %q", name)
		}
		return Endpoint{raw: raw, scheme: "npipe", path: `//./pipe/` + name, digest: domain.SumBytes([]byte(raw))}, nil
	case "linux":
		parsed, err := url.Parse(raw)
		if err != nil {
			return Endpoint{}, fmt.Errorf("parse Docker endpoint: %w", err)
		}
		if parsed.Scheme != "unix" || parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return Endpoint{}, fmt.Errorf("Linux Docker endpoint must use an explicit local unix transport")
		}
		if parsed.Path == "" || !path.IsAbs(parsed.Path) || path.Clean(parsed.Path) != parsed.Path || parsed.RawPath != "" {
			return Endpoint{}, fmt.Errorf("Docker unix socket path must be canonical and absolute")
		}
		return Endpoint{raw: raw, scheme: "unix", path: parsed.Path, digest: domain.SumBytes([]byte(raw))}, nil
	default:
		return Endpoint{}, fmt.Errorf("unsupported client operating system %q", goos)
	}
}

func (c Config) Validate(goos string) (Endpoint, error) {
	endpoint, err := ParseLocalEndpoint(c.EngineEndpoint, goos)
	if err != nil {
		return Endpoint{}, err
	}
	if c.APIVersion != RequiredAPIVersion {
		return Endpoint{}, fmt.Errorf("Docker API version must be pinned to %s", RequiredAPIVersion)
	}
	for name, value := range map[string]string{
		"builder":  c.BuilderImage,
		"runtime":  c.RuntimeImage,
		"transfer": c.TransferImage,
	} {
		if _, err := domain.ParseDigest(value); err != nil {
			return Endpoint{}, fmt.Errorf("%s image must be an immutable image ID: %w", name, err)
		}
	}
	if c.ExecutionProtocol != ExecutionProtocolDockerDirectV2 {
		return Endpoint{}, fmt.Errorf("execution protocol must be %q", ExecutionProtocolDockerDirectV2)
	}
	return endpoint, nil
}
