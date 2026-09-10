package application

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"cpgen/internal/domain"
	"cpgen/internal/port"
	"cpgen/internal/toolchain"
)

func TestGeneratorCaseChangesSandboxOperationScope(t *testing.T) {
	file, err := os.Open(filepath.Join("..", "..", "config", "toolchains", "docker-v1.lock.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	lock, err := toolchain.LoadLock(file)
	if err != nil {
		t.Fatal(err)
	}
	base := DockerSandboxConfig{Lock: lock, EngineIdentity: domain.SumBytes([]byte("engine"))}
	base.Identity = port.SandboxAuthorizationIdentity{RunID: "run_00000000000000000000000000000001", StageName: "data_verify", AttemptID: "attempt_00000000000000000000000000000001", SandboxExecutionID: "sandbox_00000000000000000000000000000001", LogicalOperationID: "data-verification", Kind: domain.CallSandboxRun, ScopeDigest: domain.SumBytes([]byte("data content")), ExpectedRunVersion: 1}
	seed := ^uint64(0)
	request := port.RunRequest{Role: port.RoleGenerator, Program: domain.BlobRef{Digest: domain.SumBytes([]byte("program")), Size: 7}, Seed: &seed, Limits: port.RunLimits{Time: time.Second, MemoryBytes: 256 << 20, PIDs: 64, StdoutBytes: 1 << 20, StderrBytes: 4096}, Args: port.RoleArgs{GeneratorCase: &port.GeneratorCaseArgs{Ordinal: 1, Kind: domain.DataCaseSmall}}}
	first, _, err := sandboxOperationIdentity(base, domain.CallSandboxRun, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range []port.GeneratorCaseArgs{{Ordinal: 2, Kind: domain.DataCaseSmall}, {Ordinal: 1, Kind: domain.DataCaseBoundary}} {
		request.Args.GeneratorCase = &args
		changed, _, err := sandboxOperationIdentity(base, domain.CallSandboxRun, request)
		if err != nil || changed.ScopeDigest == first.ScopeDigest || changed.SandboxExecutionID == first.SandboxExecutionID {
			t.Fatal("changed generator case retained the original execution scope")
		}
	}
}
