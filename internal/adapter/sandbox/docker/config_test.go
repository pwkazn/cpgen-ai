package docker

import (
	"os"
	"testing"

	"cpgen/internal/domain"
	moby "github.com/moby/moby/client"
)

func TestParseLocalEndpointAcceptsOnlyExplicitPlatformLocalTransports(t *testing.T) {
	tests := []struct {
		name string
		goos string
		raw  string
		ok   bool
	}{
		{name: "Windows named pipe", goos: "windows", raw: "npipe:////./pipe/docker_engine", ok: true},
		{name: "Linux unix socket", goos: "linux", raw: "unix:///var/run/docker.sock", ok: true},
		{name: "TCP loopback is still remote transport", goos: "linux", raw: "tcp://127.0.0.1:2375"},
		{name: "SSH", goos: "linux", raw: "ssh://builder"},
		{name: "HTTP", goos: "windows", raw: "http://localhost"},
		{name: "missing", goos: "linux", raw: ""},
		{name: "unix on Windows", goos: "windows", raw: "unix:///var/run/docker.sock"},
		{name: "named pipe on Linux", goos: "linux", raw: "npipe:////./pipe/docker_engine"},
		{name: "unix traversal", goos: "linux", raw: "unix:///var/run/../docker.sock"},
		{name: "pipe traversal", goos: "windows", raw: "npipe:////./pipe/../docker_engine"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			endpoint, err := ParseLocalEndpoint(test.raw, test.goos)
			if test.ok && err != nil {
				t.Fatalf("valid endpoint rejected: %v", err)
			}
			if !test.ok && err == nil {
				t.Fatalf("unsafe endpoint accepted as %#v", endpoint)
			}
			if test.ok && endpoint.String() != test.raw {
				t.Fatalf("endpoint = %q, want %q", endpoint.String(), test.raw)
			}
		})
	}
}

func TestNewEngineClientIgnoresDockerEnvironment(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://attacker.example:2375")
	t.Setenv("DOCKER_CONTEXT", "remote-production")
	t.Setenv("DOCKER_API_VERSION", "9.99")

	config := validConfig()
	var gotHost, gotVersion string
	engine, err := newEngineClient(config, "windows", func(options ...moby.Opt) (*moby.Client, error) {
		client, err := moby.New(options...)
		if err == nil {
			gotHost = client.DaemonHost()
			gotVersion = client.ClientVersion()
		}
		return client, err
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = engine.Close() })
	if gotHost != config.EngineEndpoint {
		t.Fatalf("client host = %q, want explicit %q", gotHost, config.EngineEndpoint)
	}
	if gotVersion != config.APIVersion {
		t.Fatalf("client API version = %q, want explicit %q", gotVersion, config.APIVersion)
	}
	if os.Getenv("DOCKER_HOST") == gotHost {
		t.Fatal("test setup did not use a hostile ambient Docker host")
	}
}

func validConfig() Config {
	digest := domain.SumBytes([]byte("image"))
	return Config{
		EngineEndpoint:    "npipe:////./pipe/docker_engine",
		APIVersion:        "1.55",
		BuilderImage:      string(digest),
		RuntimeImage:      string(digest),
		TransferImage:     string(digest),
		ExecutionProtocol: "docker-direct-v2",
	}
}
