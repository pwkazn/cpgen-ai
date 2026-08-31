# Slice 0 Execution Probe Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (- [ ]) syntax for tracking.

**Goal:** Build and verify the Docker-only Slice 0 compile → validate → differential → structural-package probe without creating a formal run, READY state, or PackageVerificationReceipt.

**Architecture:** Keep the existing typed domain and port contracts, add a sealed test-only dispatch harness, and place all Docker Engine behavior behind a Moby Engine interface. The runner imports verified blobs through trusted volumes, runs direct non-root PID 1 targets under docker-direct-v2, exports declared files through a trusted helper, and uses a detached watchdog plus capability canaries to fail closed.

**Tech Stack:** Go 1.24, github.com/moby/moby/client v0.5.1, github.com/moby/moby/api v1.55.0, golang.org/x/text v0.29.0, Docker Engine API v1.55, Docker Desktop/Linux Engine, C++20, Go 1.24.13, testlib 0.9.45.

**Spec:** docs/superpowers/specs/2026-08-31-phase1-mvp-design.md

## Global Constraints

- The authoritative order is plan.md → ARCHITECTURE.md → docs/adr → docs/design → schemas/migrations/code.
- The execution protocol is exactly docker-direct-v2; there is no host process fallback.
- Only explicit local unix:// or npipe:// endpoints are accepted; DOCKER_HOST, DOCKER_CONTEXT, TCP, HTTP(S), SSH, and remote contexts are never inherited.
- Target containers are direct non-root PID 1 with CapDrop=ALL, no-new-privileges, read-only rootfs, network none, fixed environment, fixed mounts, MemorySwap=Memory, PIDs limit, and LogConfig=none.
- Every import, keeper, target, and export ContainerCreate consumes one distinct pre-authorized probe grant and appears in the logical CallTrace.
- Source, program, and input bytes enter Docker only through VerifiedBlobReader.OpenVerified; declared outputs leave through a read-only trusted export helper and MeteredArtifactSink.
- Slice 0 may use only the build-tagged test harness. The normal CLI must not be able to construct a dispatch authorization or run probe workloads.
- Slice 0 may emit only probe artifacts and a structurally verified probe package. It must not create SQLite state, READY, a package occurrence, or PackageVerificationReceipt.
- Base image and toolchain identities are immutable digests. Image builds use --pull=false after explicit digest pulls.
- Default tests never contact an LLM or Similarity service.
- Existing uncommitted M0 files belong to the user and must be inspected, preserved, and staged explicitly.
- This Slice is executed inline. The one user-authorized subagent is reserved for the single whole-Phase-1 review after Slice 5.

## File Structure

### Existing files to retain or extend

- go.mod / go.sum — Go version and exact dependency graph.
- internal/domain/* — IDs, digest, outcomes, artifacts, call traces, execution causes.
- internal/port/sandbox.go — public compile/run/probe contracts and sealed authorization interfaces.
- internal/judge/role_adapter.go — ADR-0003 role mapping.
- internal/adapter/fake/* — deterministic non-Docker contract adapters.
- internal/cli/run.go and cmd/cpgen/main.go — public CLI and hidden watchdog dispatch.
- README.md / TODO.md / .github/workflows/ci.yml — status and verification commands.

### New focused units

- internal/port/sandbox_plan.go — canonical resource plan, profile, capability, and trace validation.
- internal/port/sandbox_probe_factory.go — build-tagged cpgen_slice0_probe authorization constructor.
- internal/probe/ledger.go — in-memory one-shot claim ledger, build-tagged cpgen_slice0_probe.
- internal/probe/harness.go — Slice0ProbeHarness orchestration, build-tagged cpgen_slice0_probe.
- internal/adapter/sandbox/docker/config.go — explicit endpoint and pinned image configuration.
- internal/adapter/sandbox/docker/doctor.go — ping/version/info/image static checks and engine identity.
- internal/adapter/sandbox/docker/engine.go — narrow Moby Engine interface and production client wrapper.
- internal/adapter/sandbox/docker/spec.go — immutable target/helper create specifications and inspect checks.
- internal/adapter/sandbox/docker/runner.go — plan execution and Compile/Run/Probe entry points.
- internal/adapter/sandbox/docker/transfer.go — import/keeper/export lifecycle.
- internal/adapter/sandbox/docker/process.go — attach, limits, evidence, classification, and cleanup.
- internal/adapter/sandbox/docker/watchdog.go — owner-side watchdog protocol.
- internal/adapter/sandbox/docker/control_windows.go / control_unix.go — owner-only IPC and detached process launch.
- internal/watchdog/service.go — detached reconcile service used by the hidden CLI command.
- internal/securefs/root.go plus platform files — same-handle regular-file opening and link-count checks.
- internal/transfer/* and cmd/cpgen-transfer/main.go — trusted length-prefixed import/export/keeper helper.
- internal/toolchain/manifest.go — strict toolchain/image lock model.
- cmd/cpgen-image-lock/main.go — trusted developer command that builds images and writes exact image IDs.
- build/docker/{builder,runtime,transfer}/Dockerfile — pinned trusted images.
- third_party/testlib/testlib.h / NOTICE — pinned testlib source and provenance.
- internal/fixture/ab/* — fixed A+B sources and expected vectors.
- internal/packageprobe/* — minimal cpgen.package/v1 builder, safe reader, reverse reader, and StructuralGate.
- internal/probe/slice0_integration_test.go — Docker vertical and capability tests.

---

### Task 1: Adopt and freeze the existing M0 baseline

**Files:**
- Modify: go.mod
- Create: go.sum
- Retain and stage: .gitignore, .github/workflows/ci.yml, cmd/cpgen/main.go, internal/**, README.md, TODO.md
- Create and stage: docs/superpowers/plans/2026-08-31-slice0-execution-probe.md
- Modify and stage: docs/superpowers/specs/2026-08-31-phase1-mvp-design.md

**Interfaces:**
- Consumes: the current untracked typed scaffold.
- Produces: a reviewed, reproducible Go baseline on which all Slice 0 tasks build.

- [x] **Step 1: Re-read the working tree before taking ownership**

Run:

~~~powershell
git status --short --branch
git diff -- .gitignore README.md TODO.md
rg --files cmd internal .github
~~~

Expected: only the known M0 scaffold, documentation status edits, this plan, and no secret/config credential files.

- [x] **Step 2: Verify the unmodified baseline**

Run:

~~~powershell
go test ./...
go vet ./...
go test -race ./...
~~~

Expected: all current packages PASS before new dependencies are introduced.

- [x] **Step 3: Pin the Slice 0 Go dependencies**

Run:

~~~powershell
go mod edit '-require=github.com/moby/moby/client@v0.5.1'
go mod edit '-require=github.com/moby/moby/api@v1.55.0'
go mod download github.com/moby/moby/client@v0.5.1 github.com/moby/moby/api@v1.55.0
~~~

Expected: go.mod remains at go 1.24; go.sum records the selected Moby modules without `go mod tidy` removing them before their first imports exist; no deprecated github.com/docker/docker module is added. Add golang.org/x/text@v0.29.0 in Task 12 immediately before its normalization package is first imported.

- [x] **Step 4: Record approval in the design**

Change the design status line to:

~~~markdown
状态：已批准，按 Slice 0 → 5 顺序交付
~~~

- [x] **Step 5: Re-run the baseline and inspect staged scope**

Run:

~~~powershell
go test ./...
go vet ./...
git diff --check
git status --short
~~~

Expected: PASS; only explicit M0/design/plan files are ready to stage.

- [x] **Step 6: Commit the reviewed scaffold**

Run:

~~~powershell
git add -- .gitignore .github cmd go.mod go.sum internal README.md TODO.md docs/superpowers/plans/2026-08-31-slice0-execution-probe.md docs/superpowers/specs/2026-08-31-phase1-mvp-design.md
git diff --cached --check
git commit -m "phase1(slice-0): establish typed scaffold"
~~~

Expected: one baseline commit; unrelated files remain untouched.

### Task 2: Complete canonical plans and sealed probe authorization

**Files:**
- Modify: internal/port/sandbox.go
- Create: internal/port/sandbox_plan.go
- Create: internal/port/sandbox_plan_test.go
- Create: internal/port/sandbox_probe_factory.go
- Create: internal/probe/ledger.go
- Create: internal/probe/ledger_test.go

**Interfaces:**
- Consumes: domain.Digest, IDs, CallTrace, ContainerRole.
- Produces:
  - NewContainerPlan(engineIdentity domain.Digest, resources []PlannedResource, transferBytesMax int64) (ContainerPlan, error)
  - ContainerPlan.Clone() ContainerPlan
  - ContainerPlan.Validate() error
  - CapabilitySnapshot.Validate(profile SandboxProfile) error
  - build-tagged port.NewSlice0ProbeAuthorization(identity ProbeAuthorizationIdentity, plan ContainerPlan, claims ProbeClaimStore) (SandboxDispatchAuthorization, error)
  - probe.Ledger implementing port.ProbeClaimStore under cpgen_slice0_probe.

- [x] **Step 1: Write failing canonical-plan and one-shot-claim tests**

Add table tests that assert:

~~~go
func TestNewContainerPlanCanonicalDigestAndClone(t *testing.T) {
    resources := []port.PlannedResource{
        {Ordinal: 0, Kind: port.ResourceVolume, Role: port.ResourceInput, DeterministicName: "cpgen-v-0"},
        {Ordinal: 1, Kind: port.ResourceContainer, Role: port.ResourceTarget, DeterministicName: "cpgen-c-1", CreateCallOrdinal: intPtr(0)},
    }
    plan, err := port.NewContainerPlan(engineDigest, resources, 1024)
    if err != nil { t.Fatal(err) }
    clone := plan.Clone()
    clone.Resources[0].DeterministicName = "mutated"
    if plan.Resources[0].DeterministicName != "cpgen-v-0" { t.Fatal("plan was aliased") }
    if err := plan.Validate(); err != nil { t.Fatal(err) }
}

func TestProbeLedgerRejectsWrongOrderAndDuplicateClaim(t *testing.T) {
    auth := newProbeAuth(t, []port.ContainerRole{port.ContainerImport, port.ContainerTarget})
    if _, err := auth.ClaimNextContainer(context.Background(), port.ContainerTarget); err == nil {
        t.Fatal("wrong-order claim succeeded")
    }
    if _, err := auth.ClaimNextContainer(context.Background(), port.ContainerImport); err != nil {
        t.Fatal(err)
    }
    if _, err := auth.ClaimNextContainer(context.Background(), port.ContainerImport); err == nil {
        t.Fatal("duplicate claim succeeded")
    }
}
~~~

- [x] **Step 2: Run the tests and verify the expected compile failure**

Run:

~~~powershell
go test -tags=cpgen_slice0_probe ./internal/port ./internal/probe
~~~

Expected: FAIL because PlannedResource, NewContainerPlan, ProbeClaimStore, and the probe factory do not exist.

- [x] **Step 3: Define the exact plan and capability types**

Implement these public shapes with strict enum JSON decoding and validation:

~~~go
type SandboxProfile string
const (
    ProfileCompileV2 SandboxProfile = "compile-v2"
    ProfileExecuteMVPV2 SandboxProfile = "execute-mvp-v2"
    ProfileExecuteReleaseV2 SandboxProfile = "execute-release-v2"
)

type ResourceKind string
const (
    ResourceContainer ResourceKind = "CONTAINER"
    ResourceVolume ResourceKind = "VOLUME"
    ResourceCgroup ResourceKind = "CGROUP"
)

type ResourceRole string
const (
    ResourceImport ResourceRole = "IMPORT"
    ResourceKeeper ResourceRole = "KEEPER"
    ResourceTarget ResourceRole = "TARGET"
    ResourceExport ResourceRole = "EXPORT"
    ResourceInput ResourceRole = "INPUT"
    ResourceOutput ResourceRole = "OUTPUT"
    ResourceReleaseParent ResourceRole = "RELEASE_PARENT"
)

type PlannedResource struct {
    Ordinal int
    Kind ResourceKind
    Role ResourceRole
    DeterministicName string
    ExpectedLabelsDigest domain.Digest
    CreateCallOrdinal *int
    CgroupRelativePath *domain.SafeRelPath
    CreationNonce *string
}

type ContainerPlan struct {
    PlanDigest domain.Digest
    EngineIdentityDigest domain.Digest
    Resources []PlannedResource
    TransferBytesMax int64
}

type CapabilitySnapshot struct {
    Profile SandboxProfile
    EngineIdentityDigest domain.Digest
    EndpointDigest domain.Digest
    ServerOS string
    APIVersion string
    CgroupVersion int
    Flags map[string]bool
    BuilderImageDigest domain.Digest
    RuntimeImageDigest domain.Digest
    TransferImageDigest domain.Digest
    ExecutionProtocol string
}
~~~

Canonical hashing must length-prefix schema version, engine digest, transfer limit, and every resource field in ordinal order. Validation requires contiguous ordinals, exactly one TARGET container, IMPORT*→KEEPER?→TARGET→EXPORT ordering, unique names, and exact CreateCallOrdinal mapping. The sole exception is an empty zero-transfer plan, which represents the mutually exclusive Engine-ping probe described by the authoritative sandbox design.

- [x] **Step 4: Implement the build-tagged authorization**

Place the factory behind:

~~~go
//go:build cpgen_slice0_probe
~~~

The concrete authorization lives in package port, owns the private seal methods, returns deep plan copies, and delegates claims to:

~~~go
type ProbeClaimStore interface {
    ClaimEnginePing(context.Context, ProbeAuthorizationIdentity) (domain.AttemptCallID, error)
    ClaimContainer(context.Context, ProbeAuthorizationIdentity, int, ContainerRole) (domain.AttemptCallID, error)
}
~~~

The in-memory ledger must CAS AUTHORIZED→DISPATCHING once, enforce run/attempt/owner/epoch/scope/plan identity, and expose a read-only claim snapshot for tests. It must never be compiled without cpgen_slice0_probe.

- [x] **Step 5: Run focused and full tests**

Run:

~~~powershell
go test -tags=cpgen_slice0_probe ./internal/port ./internal/probe
go test ./...
~~~

Expected: PASS, including unknown enum rejection and deep-copy tests.

- [x] **Step 6: Commit**

Run:

~~~powershell
git add -- internal/port/sandbox.go internal/port/sandbox_plan.go internal/port/sandbox_plan_test.go internal/port/sandbox_probe_factory.go internal/probe/ledger.go internal/probe/ledger_test.go
git commit -m "phase1(slice-0): seal probe dispatch plans"
~~~

### Task 3: Enforce explicit local endpoints and implement the static doctor

**Files:**
- Create: internal/adapter/sandbox/docker/config.go
- Create: internal/adapter/sandbox/docker/config_test.go
- Create: internal/adapter/sandbox/docker/engine.go
- Create: internal/adapter/sandbox/docker/doctor.go
- Create: internal/adapter/sandbox/docker/doctor_test.go
- Modify: internal/cli/run.go
- Modify: internal/cli/run_test.go

**Interfaces:**
- Consumes: pinned ImageLock from Task 5, but supports injected test locks now.
- Produces:
  - ParseLocalEndpoint(raw, goos string) (Endpoint, error)
  - NewEngineClient(Config) (Engine, error), never using client.FromEnv
  - Doctor.Check(ctx) (StaticReport, error)
  - public cpgen doctor --json --engine-endpoint ... read-only command.

- [x] **Step 1: Write failing endpoint and environment-isolation tests**

Cover this matrix:

~~~go
tests := []struct{ goos, raw string; ok bool }{
    {"windows", "npipe:////./pipe/docker_engine", true},
    {"linux", "unix:///var/run/docker.sock", true},
    {"linux", "tcp://127.0.0.1:2375", false},
    {"linux", "ssh://builder", false},
    {"windows", "http://localhost", false},
    {"linux", "", false},
}
~~~

Set DOCKER_HOST and DOCKER_CONTEXT to hostile values, inject a client constructor spy, and assert only the explicit endpoint reaches client.WithHost.

- [x] **Step 2: Run and verify failure**

Run:

~~~powershell
go test ./internal/adapter/sandbox/docker ./internal/cli -run "Test(ParseLocalEndpoint|Doctor|DockerEnvironment)"
~~~

Expected: FAIL because the docker adapter package does not exist.

- [x] **Step 3: Implement strict config and a narrow Engine interface**

Use:

~~~go
type Config struct {
    EngineEndpoint string
    APIVersion string
    BuilderImage string
    RuntimeImage string
    TransferImage string
    ExecutionProtocol string
}

type Engine interface {
    Ping(context.Context, client.PingOptions) (client.PingResult, error)
    ServerVersion(context.Context, client.ServerVersionOptions) (client.ServerVersionResult, error)
    Info(context.Context, client.InfoOptions) (client.SystemInfoResult, error)
    ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error)
    Close() error
}
~~~

Construct the production client with client.New(client.WithHost(endpoint), client.WithAPIVersion("1.55")); do not pass client.FromEnv or client.WithAPIVersionFromEnv. Close idle connections through a Close method on the wrapper.

- [x] **Step 4: Implement static capability and engine identity checks**

StaticReport must contain endpoint digest, daemon ID, server/API/OS/arch, cgroup/security options, the three inspected image IDs, and EngineIdentityDigest. It must reject non-Linux servers, missing/mismatched pinned images, protocol other than docker-direct-v2, or API below 1.40. Map connection failures to domain.PortFailure{Code: unavailable, Class: BLOCKED}; version/OS/image mismatch maps to incompatible.

- [x] **Step 5: Add a read-only doctor command**

The command accepts explicit flags and emits versioned JSON:

~~~json
{"schema_version":"cpgen.doctor/v1","status":"HEALTHY","engine_identity_digest":"sha256:...","checks":[]}
~~~

It must not create a container, call the probe factory, read ambient Docker variables, or write workflow state. Unknown/missing flags exit 2; unreachable Engine exits 10 with status BLOCKED; healthy exits 0.

- [x] **Step 6: Run focused and full tests**

Run:

~~~powershell
go test ./internal/adapter/sandbox/docker ./internal/cli
go test ./...
go vet ./...
~~~

Expected: PASS; fake Engine call log contains Ping, Version, Info, and three ImageInspect calls only.

- [x] **Step 7: Commit**

Run:

~~~powershell
git add -- internal/adapter/sandbox/docker internal/cli/run.go internal/cli/run_test.go
git commit -m "phase1(slice-0): add explicit docker doctor"
~~~

### Task 4: Build the trusted same-handle transfer helper

**Files:**
- Create: internal/securefs/root.go
- Create: internal/securefs/root_test.go
- Create: internal/securefs/nlink_unix.go
- Create: internal/securefs/nlink_windows.go
- Create: internal/transfer/protocol.go
- Create: internal/transfer/import.go
- Create: internal/transfer/export.go
- Create: internal/transfer/keeper.go
- Create: internal/transfer/transfer_test.go
- Create: cmd/cpgen-transfer/main.go

**Interfaces:**
- Produces:
  - securefs.OpenRegular(root *os.Root, path domain.SafeRelPath, requireSingleLink bool) (*os.File, os.FileInfo, error)
  - transfer.Import(ctx context.Context, root *os.Root, in io.Reader, limits transfer.ImportLimits) ([]transfer.ImportedFile, error)
  - transfer.Export(ctx context.Context, root *os.Root, plan transfer.ExportPlan, out io.Writer) ([]transfer.ExportedFile, error)
  - transfer.Keep(ctx context.Context) error
  - a static cpgen-transfer binary with import, export, and keep subcommands.

- [x] **Step 1: Write failing traversal and protocol tests**

Tests must create nested regular files and attempt traversal, absolute paths, symlinks, hardlinks, directories, FIFO/socket where supported, duplicate paths, declared-size underflow/overflow, and limit+1 payloads. The accepted path must be read from the exact file handle returned by securefs.OpenRegular.

Define the frame protocol:

~~~go
type FrameHeader struct {
    SchemaVersion domain.SchemaVersion `json:"schema_version"`
    Path domain.SafeRelPath `json:"path"`
    Size int64 `json:"size"`
    Mode FrameMode `json:"mode"`
}

type ExportPlan struct {
    SchemaVersion domain.SchemaVersion `json:"schema_version"`
    Files []port.OutputDeclaration `json:"files"`
    MaxFiles int `json:"max_files"`
    MaxTotalBytes int64 `json:"max_total_bytes"`
}
~~~

Each frame is uint32 big-endian header length, strict JSON header, uint64 big-endian data length, then exactly Size bytes. A zero header length terminates the stream.

- [x] **Step 2: Run and verify failure**

Run:

~~~powershell
go test ./internal/securefs ./internal/transfer ./cmd/cpgen-transfer
~~~

Expected: FAIL because the packages do not exist.

- [x] **Step 3: Implement same-handle safe opening**

Use os.OpenRoot. For every directory component compare parent.Lstat(component) with childRoot.Stat(".") using os.SameFile; reject a symlink mode before descending. For the final component compare Root.Lstat(path) with openedFile.Stat(), require Mode().IsRegular(), and require platform link count 1 when requested. Continue reading only from the opened handle.

- [x] **Step 4: Implement import/export/keeper**

Import creates only declared directories below the opened root, uses O_CREATE|O_EXCL, writes exactly Size bytes, fsyncs, chmods 0444, and rejects trailing frames. Export opens only planned paths, checks link count/type/size, writes through the same handle, and enforces file plus aggregate limits before the terminating frame. Keep waits for SIGTERM/SIGINT without opening any path.

- [x] **Step 5: Run tests and build a static Linux helper**

Run:

~~~powershell
go test ./internal/securefs ./internal/transfer
$env:CGO_ENABLED = "0"
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -buildvcs=false -trimpath -ldflags="-s -w -buildid=" -o "$env:TEMP\cpgen-transfer" ./cmd/cpgen-transfer
Remove-Item Env:CGO_ENABLED,Env:GOOS,Env:GOARCH
~~~

Expected: tests PASS and the Linux binary builds without cgo.

- [x] **Step 6: Commit**

Run:

~~~powershell
git add -- internal/securefs internal/transfer cmd/cpgen-transfer
git commit -m "phase1(slice-0): add trusted volume transfer helper"
~~~

### Task 5: Pin testlib, toolchains, and all three Docker images

**Files:**
- Create: third_party/testlib/testlib.h
- Create: third_party/testlib/NOTICE
- Create: internal/toolchain/manifest.go
- Create: internal/toolchain/manifest_test.go
- Create: build/docker/builder/Dockerfile
- Create: build/docker/runtime/Dockerfile
- Create: build/docker/transfer/Dockerfile
- Create: cmd/cpgen-image-lock/main.go
- Create: cmd/cpgen-image-lock/main_test.go
- Create during execution: config/toolchains/docker-v1.lock.json

**Interfaces:**
- Produces:
  - toolchain.LoadLock(io.Reader) (Lock, error)
  - Lock.Validate() error
  - a trusted image-lock command that writes a complete lock atomically after three successful digest builds.

- [x] **Step 1: Write failing strict-lock tests**

Use a known-good JSON fixture and reject unknown fields, floating tags, uppercase/malformed digests, duplicate toolchain IDs, testlib hash mismatch, protocol mismatch, and missing compiler flags.

~~~go
type Lock struct {
    SchemaVersion domain.SchemaVersion `json:"schema_version"`
    ExecutionProtocol string `json:"execution_protocol"`
    Builder Image `json:"builder"`
    Runtime Image `json:"runtime"`
    Transfer Image `json:"transfer"`
    Toolchains []Toolchain `json:"toolchains"`
    Testlib Testlib `json:"testlib"`
}
~~~

- [x] **Step 2: Run and verify failure**

Run:

~~~powershell
go test ./internal/toolchain ./cmd/cpgen-image-lock
~~~

Expected: FAIL because the manifest package does not exist.

- [x] **Step 3: Vendor the exact testlib source**

Fetch commit 1e4e8a24c79c6bad3becbdb5a332ffc352b7d5dd and verify:

~~~text
testlib.h SHA-256 = bb323e3c89285214966076e0d23d5a295c5f6126da7ff198c1276ddb95ecb1a0
testlib VERSION = 0.9.45
~~~

NOTICE must record the upstream repository, commit, file hash, version, and upstream permissive notice. Treat the fetched header as a mechanical vendored asset; never edit its contents.

- [x] **Step 4: Implement reproducible Dockerfiles**

Builder base:

~~~dockerfile
FROM golang:1.24.13-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac
COPY third_party/testlib/testlib.h /opt/cpgen/include/testlib.h
RUN groupadd --gid 65532 cpgen && useradd --uid 65532 --gid 65532 --no-create-home --home-dir /nonexistent cpgen
USER 65532:65532
ENTRYPOINT []
~~~

Runtime and final transfer base:

~~~dockerfile
FROM debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171
RUN groupadd --gid 65532 cpgen && useradd --uid 65532 --gid 65532 --no-create-home --home-dir /nonexistent cpgen
USER 65532:65532
ENTRYPOINT []
~~~

The transfer Dockerfile uses the pinned Go image as a build stage, compiles cmd/cpgen-transfer with CGO_ENABLED=0 and -trimpath, and copies only /usr/local/bin/cpgen-transfer into the pinned Debian final stage. All three images set fixed org.cpgen.* labels; no Dockerfile performs apt, curl, git, or another network fetch.

The C++ command in the lock is fixed to:

~~~text
/usr/bin/g++ -std=c++20 -O2 -pipe -static -s -I/opt/cpgen/include /src/<entry> -o /result/files/main
~~~

The Go command is fixed to this argv; the entire `-ldflags=...` value is one argument:

~~~text
/usr/local/go/bin/go
build
-trimpath
-ldflags=-s -w -buildid=
-o
/result/files/main
/src/<entry>
~~~

with GO111MODULE=off, GOPROXY=off, GOSUMDB=off, CGO_ENABLED=0.

- [x] **Step 5: Implement image-lock generation**

The command checks that both pinned base refs already exist locally, then runs docker build --pull=false --iidfile for builder, runtime, and transfer. It validates each iid as sha256:<64 lowercase hex>, inspects labels, and writes config/toolchains/docker-v1.lock.json through create-temp → fsync → rename. It must remove the temp file on any failed build and never emit a partial lock.

- [x] **Step 6: Pull the exact bases, build, and generate the lock**

Run:

~~~powershell
docker pull "golang:1.24.13-bookworm@sha256:1a6d4452c65dea36aac2e2d606b01b4a029ec90cc1ae53890540ce6173ea77ac"
docker pull "debian:bookworm-20260824-slim@sha256:88200866dfff7ea7f5cbcb6ec7c8a701889efe6fe859fe64d6990e4b07ea4171"
go run ./cmd/cpgen-image-lock --output config/toolchains/docker-v1.lock.json
~~~

Expected: the lock contains three distinct sha256 image IDs and exact toolchain/testlib metadata; no floating image tag is used by runtime config.

- [x] **Step 7: Verify and commit**

Run:

~~~powershell
go test ./internal/toolchain ./cmd/cpgen-image-lock
$lock = Get-Content -Raw config/toolchains/docker-v1.lock.json | ConvertFrom-Json
docker image inspect $lock.builder.image_id
git add -- third_party/testlib internal/toolchain build/docker cmd/cpgen-image-lock config/toolchains/docker-v1.lock.json
git commit -m "phase1(slice-0): pin sandbox images and toolchains"
~~~

### Task 6: Build immutable Docker create specifications

**Files:**
- Extend: internal/adapter/sandbox/docker/engine.go
- Create: internal/adapter/sandbox/docker/spec.go
- Create: internal/adapter/sandbox/docker/spec_test.go
- Create: internal/adapter/sandbox/docker/plan.go
- Create: internal/adapter/sandbox/docker/plan_test.go

**Interfaces:**
- Consumes: port.ContainerPlan, toolchain.Lock, Docker Config.
- Produces:
  - BuildCompilePlan(request, lock, identity) (ContainerPlan, error)
  - BuildRunPlan(request, lock, identity) (ContainerPlan, error)
  - TargetCreateOptions(...) (client.ContainerCreateOptions, error)
  - VerifyTargetInspect(expected, client.ContainerInspectResult) error.

- [x] **Step 1: Write failing exact-spec tests**

Assert the target options contain:

~~~go
want := TargetSecurity{
    User: "65532:65532",
    ReadonlyRootfs: true,
    NetworkMode: "none",
    CapDrop: []string{"ALL"},
    SecurityOpt: []string{"no-new-privileges"},
    LogType: "none",
    AutoRemove: false,
    Init: false,
    Memory: request.Limits.MemoryBytes,
    MemorySwap: request.Limits.MemoryBytes,
    PidsLimit: request.Limits.PIDs,
}
~~~

Also assert fixed LANG=C.UTF-8, TZ=UTC, HOME/TMPDIR under /work; WorkingDir=/work; ShmSize, ulimits, tmpfs, NoCopy volumes; no bind mount, device, privileged, host namespace, inherited environment, shell, or image from the request.

- [x] **Step 2: Run and verify failure**

Run:

~~~powershell
go test ./internal/adapter/sandbox/docker -run "Test(Build.*Plan|TargetCreateOptions|VerifyTargetInspect)"
~~~

Expected: FAIL because plan/spec functions do not exist.

- [x] **Step 3: Extend the Engine interface only for required calls**

Add typed Moby v0.5.1 methods for ContainerCreate/Start/Attach/Wait/Inspect/Stop/Kill/Remove, VolumeCreate/Inspect/Remove, Events, and ImageInspect. Keep the interface local to the adapter so unit tests use a deterministic fake Engine.

- [x] **Step 4: Implement plans and immutable create options**

Compile plans are IMPORT*→KEEPER→TARGET→EXPORT because the program artifact is declared output. Run plans without file outputs are IMPORT*→TARGET; with outputs they are IMPORT*→KEEPER→TARGET→EXPORT. Names derive from random operation nonce plus ordinal; labels contain run, attempt, logical operation, call, lease epoch, role, plan digest, and engine digest.

Target command is an argument array selected from the lock; Run always starts /program/main directly. Compile starts the locked compiler directly. The request cannot inject a command, environment, image, mount, capability, network, or compiler flag.

- [x] **Step 5: Implement post-create inspect verification**

Before Start, compare exact image ID, user, entrypoint/cmd, read-only root, security options, capabilities, resource limits, network mode, log config, restart policy, AutoRemove, and every mount destination/type/read-only/NoCopy property. Any unexpected image Config.Volumes or writable mount fails closed and triggers cleanup.

- [x] **Step 6: Run tests and commit**

Run:

~~~powershell
go test ./internal/adapter/sandbox/docker
go test ./...
git add -- internal/adapter/sandbox/docker
git commit -m "phase1(slice-0): lock docker resource plans"
~~~

### Task 7: Implement verified import, keeper, export, and artifact promotion

**Files:**
- Create: internal/adapter/sandbox/docker/transfer.go
- Create: internal/adapter/sandbox/docker/transfer_test.go
- Create: internal/adapter/sandbox/docker/runner.go
- Create: internal/adapter/sandbox/docker/runner_test.go

**Interfaces:**
- Consumes: port.VerifiedBlobReader, port.MeteredArtifactSink, sealed grants, trusted transfer image.
- Produces: docker.Runner implementing port.DockerSandbox with transfer-safe Compile/Run scaffolding.

- [x] **Step 1: Write failing plan-dispatch and blob-integrity tests**

Use a fake Engine, fake VerifiedBlobReader, and fake ArtifactSink to assert:

- no ContainerCreate occurs before OpenVerified and the next correct grant;
- import/keeper/target/export each claim exactly once and in order;
- a corrupt same-size Blob stops before any Create;
- target STOPPED precedes export Create;
- export PendingArtifact.CallID is the export grant call;
- failure releases unconsumed claims/tokens and removes only matching planned resources.

- [x] **Step 2: Run and verify failure**

Run:

~~~powershell
go test -tags=cpgen_slice0_probe ./internal/adapter/sandbox/docker -run "Test(VerifiedImport|GrantOrder|Keeper|Export)"
~~~

Expected: FAIL because Runner and transfer lifecycle do not exist.

- [x] **Step 3: Implement Runner dependencies and call-trace assembly**

~~~go
type Runner struct {
    engine Engine
    config Config
    lock toolchain.Lock
    blobs port.VerifiedBlobReader
    artifacts port.MeteredArtifactSink
    watchdog WatchdogController
    limits ControlLimits
}
~~~

Runner recomputes plan and engine identity, validates the authorization copy, opens all input blobs, prepares all declared artifact writers, and only then begins resource creation. PhysicalAttemptCallIDs are appended only after the associated Engine dispatch begins; ResultAttemptCallID is target unless an import/keeper/export failure determines the result.

- [x] **Step 4: Implement volume lifecycle**

Readonly payload uses ordinary named volumes populated by the trusted import container. Declared output uses local-driver tmpfs:

~~~text
type=tmpfs
device=tmpfs
o=size=<sum>,uid=65532,gid=65532,nosuid,nodev,noexec
~~~

Start keeper before target and retain it through export. Export mounts output read-only, sends the exact ExportPlan, and streams frames into predeclared artifact writers. Persist the returned PendingArtifact values in the result before keeper/volume removal.

- [x] **Step 5: Add strict cleanup ownership**

Remove by persisted ID or deterministic name only after engine identity and exact labels match. A name collision with different labels is an error, never a deletion target. Cleanup uses a fresh bounded context derived from context.Background().

- [x] **Step 6: Run focused and full tests**

Run:

~~~powershell
go test -tags=cpgen_slice0_probe ./internal/adapter/sandbox/docker
go test ./...
~~~

Expected: PASS; fake Engine call logs prove claim/open/create/start/stop/export order.

- [x] **Step 7: Commit**

Run:

~~~powershell
git add -- internal/adapter/sandbox/docker/transfer.go internal/adapter/sandbox/docker/transfer_test.go internal/adapter/sandbox/docker/runner.go internal/adapter/sandbox/docker/runner_test.go
git commit -m "phase1(slice-0): add verified docker transfer lifecycle"
~~~

### Task 8: Implement attach limits, outcome evidence, and cleanup

**Files:**
- Create: internal/adapter/sandbox/docker/process.go
- Create: internal/adapter/sandbox/docker/process_test.go
- Create: internal/adapter/sandbox/docker/evidence.go
- Create: internal/adapter/sandbox/docker/evidence_test.go
- Create: internal/adapter/sandbox/docker/cleanup.go
- Create: internal/adapter/sandbox/docker/cleanup_test.go
- Extend: internal/adapter/sandbox/docker/runner.go

**Interfaces:**
- Produces:
  - ExecutionRecord.Validate() error
  - classifyProcess(Evidence) (domain.ProcessOutcome, error)
  - Runner Compile/Run complete implementations.

- [x] **Step 1: Write failing outcome-priority tests**

Cover the documented order:

~~~go
tests := []struct{
    name string
    evidence Evidence
    want domain.ProcessOutcome
}{
    {"create failure", Evidence{CreateFailure: true}, domain.ProcessInfraError},
    {"hard deadline", Evidence{Started: true, HardDeadline: true, Stopped: true}, domain.ProcessTLE},
    {"oom beats ole", Evidence{OOMKilled: true, OutputExceeded: true}, domain.ProcessMLE},
    {"missing runtime evidence", Evidence{Started: true, EvidenceComplete: false}, domain.ProcessInfraError},
    {"ole", Evidence{Started: true, EvidenceComplete: true, OutputExceeded: true}, domain.ProcessOLE},
    {"signal", Evidence{Started: true, EvidenceComplete: true, Signal: "SIGSEGV"}, domain.ProcessSignaled},
    {"exit", Evidence{Started: true, EvidenceComplete: true, ExitCode: intPtr(0)}, domain.ProcessExited},
}
~~~

Add tests for each ExecutionCause racing TLE/OOM/exit; the result must be domain.ExecutionInterrupted, never a late ProcessOutcome.

- [x] **Step 2: Run and verify failure**

Run:

~~~powershell
go test ./internal/adapter/sandbox/docker -run "Test(ProcessOutcomePriority|ExecutionCauseRace|OutputLimiter|Cleanup)"
~~~

Expected: FAIL because evidence/process/cleanup units do not exist.

- [x] **Step 3: Implement attach and limit+1 accounting**

Attach before Start with Tty=false, stdout/stderr selected, stdin only when declared. Demultiplex with Moby stdcopy. Keep separate counters; when either reads limit+1, set OLE evidence, trigger Stop/Kill, and continue drain/discard until the attach stream closes. Never rely on daemon json-file logs.

- [x] **Step 4: Implement authoritative timing and OOM evidence**

Record start_boundary immediately before ContainerStart, arm the program timer from that boundary, subscribe to OOM/die events first, and reconcile Start/Wait/Inspect. For mvp-v2, only target OOM events or State.OOMKilled prove MLE. Unknown Start or contradictory evidence becomes INFRA_ERROR/UNKNOWN evidence, not TLE.

- [x] **Step 5: Implement portable stop proof**

Use SIGTERM with fixed grace, SIGKILL, Wait(NotRunning), then Inspect. A stopped proof is Wait success, exact-ID Inspect Running=false/Pid=0, or NotFound plus a terminal Create claim and watchdog final scan. Never return CANCELLED/BLOCKED/READY while target stop is unproven.

- [x] **Step 6: Produce immutable execution records**

Store protocol, started, outcome, raw exit/signal evidence, wall time, optional CPU/RSS, profile, byte counts, truncation flags, OOM flag, engine/plan/call identities. Target containers have no writable mount or writer token for this record.

- [x] **Step 7: Run tests and commit**

Run:

~~~powershell
go test ./internal/adapter/sandbox/docker
go test -race ./internal/adapter/sandbox/docker
git add -- internal/adapter/sandbox/docker
git commit -m "phase1(slice-0): classify bounded docker processes"
~~~

### Task 9: Add the detached watchdog and hidden service command

**Files:**
- Create: internal/watchdog/control.go
- Create: internal/watchdog/service.go
- Create: internal/watchdog/service_test.go
- Create: internal/adapter/sandbox/docker/watchdog.go
- Create: internal/adapter/sandbox/docker/watchdog_test.go
- Create: internal/adapter/sandbox/docker/control_windows.go
- Create: internal/adapter/sandbox/docker/control_unix.go
- Modify: internal/cli/run.go
- Modify: internal/cli/run_test.go

**Interfaces:**
- Consumes: immutable resource plan, engine identity, random token, safety deadline.
- Produces:
  - WatchdogController.Arm(ctx, ControlRecord) (Session, error)
  - Session.PreCreate/ResourceCreated/TargetPhase/Stopped/Cleaned acknowledgements
  - hidden cpgen sandbox-watchdog --control <owner-only-path>.

- [x] **Step 1: Write failing protocol, ACL, and late-create tests**

ControlRecord must contain:

~~~go
type ControlRecord struct {
    SchemaVersion domain.SchemaVersion `json:"schema_version"`
    TokenDigest domain.Digest `json:"token_digest"`
    EngineEndpoint string `json:"engine_endpoint"`
    EngineIdentityDigest domain.Digest `json:"engine_identity_digest"`
    LogicalOperationID string `json:"logical_operation_id"`
    Plan port.ContainerPlan `json:"plan"`
    SafetyDeadlineUTC time.Time `json:"safety_deadline_utc"`
}
~~~

Test invalid token/digest, widened plan, mismatched labels, deadline expiry, Create returning after deadline, owner EOF, watchdog EOF, and same-name foreign resource. Foreign resources must be reported and never removed.

- [x] **Step 2: Run and verify failure**

Run:

~~~powershell
go test -tags=cpgen_slice0_probe ./internal/watchdog ./internal/adapter/sandbox/docker -run "TestWatchdog"
~~~

Expected: FAIL because watchdog packages do not exist.

- [x] **Step 3: Implement owner-only platform IPC**

Windows uses a random npipe path and go-winio PipeConfig SecurityDescriptor restricted to the current user SID plus SYSTEM. Unix uses a 0700 control directory, 0600 record, Unix socket, and detached process group. The token exists only in the owner-only control record/channel; logs, artifacts, labels, and containers receive only its digest.

- [x] **Step 4: Implement detached reconcile**

Before any Create, the watchdog validates the full immutable plan, subscribes to Docker events, lists the plan names/labels, and ACKs. Each resource requires PRECREATE ACK then resource-ID ACK before Start. At the monotonic deadline or owner EOF, repeatedly Stop/Kill matching containers and reconcile names/labels until all create claims are terminal, no matching running resource remains, and the harness reports CLEANED.

- [x] **Step 5: Wire the hidden CLI command**

The command is absent from help, requires an absolute owner-only control path, does not accept Docker flags/image/mount/command input, and delegates only to watchdog.Service. Normal cpgen users still cannot construct a probe authorization.

- [x] **Step 6: Run child-process tests**

Run:

~~~powershell
go test -tags=cpgen_slice0_probe ./internal/watchdog ./internal/adapter/sandbox/docker ./internal/cli -run "TestWatchdog" -count=1 -v
~~~

Expected: PASS; child owner termination leaves watchdog alive until the labeled target is stopped.

- [x] **Step 7: Commit**

Run:

~~~powershell
git add -- internal/watchdog internal/adapter/sandbox/docker/watchdog.go internal/adapter/sandbox/docker/watchdog_test.go internal/adapter/sandbox/docker/control_windows.go internal/adapter/sandbox/docker/control_unix.go internal/cli
git commit -m "phase1(slice-0): add detached sandbox watchdog"
~~~

### Task 10: Implement Slice0ProbeHarness and Docker capability canaries

**Files:**
- Create: internal/probe/harness.go
- Create: internal/probe/harness_test.go
- Create: internal/probe/capability.go
- Create: internal/probe/capability_test.go
- Create: internal/probe/slice0_integration_test.go

**Interfaces:**
- Consumes: Runner, probe ledger, pinned lock, in-memory artifact sink.
- Produces:
  - probe.NewSlice0ProbeHarness(Dependencies) (*Harness, error)
  - Harness.Probe(ctx, profile) (port.DockerProbeResult, error)
  - a versioned CapabilitySnapshot with a complete CallTrace.

- [x] **Step 1: Write failing fake-engine canary aggregation tests**

Assert Engine ping has exactly one DOCKER_ENGINE_PING claim and no sandbox-run claim. Container canaries have no ping row and one claim per ContainerCreate. Missing, duplicate, or out-of-order claims fail before Engine dispatch. A failed mandatory canary yields capability_missing and BLOCKED, not a partial healthy snapshot.

- [x] **Step 2: Run and verify failure**

Run:

~~~powershell
go test -tags=cpgen_slice0_probe ./internal/probe -run "Test(Capability|Probe)"
~~~

Expected: FAIL because Harness and canary aggregation do not exist.

- [x] **Step 3: Implement compile and mvp execute canaries**

The Docker integration test must prove:

- local endpoint, Linux Engine, pinned image IDs, protocol docker-direct-v2;
- direct PID 1 and UID/GID 65532;
- CapDrop=ALL, no-new-privileges, read-only rootfs, network none, no Docker socket/bind/host environment;
- Memory exactly request, MemorySwap=Memory, memory.swap.max=0, and PIDs limit;
- PID 1 OOM and child OOM detection;
- ordinary volume import, tmpfs quota output, keeper survival after target stop, read-only export;
- LogConfig=none with Attach, stdout/stderr limit+1 OLE and stop;
- detached watchdog pre-create ACK, deadline Stop/Kill, owner death, late Create, and foreign-label refusal.

Release canary on Docker Desktop must return INCOMPATIBLE/BLOCKED with explicit release-cgroup evidence; it must not pretend release-v2 passed.

- [x] **Step 4: Add Docker-unavailable classification**

Inject a failing Engine and also test an unused local pipe/socket. Probe returns domain.PortFailure{Code: unavailable, Class: BLOCKED}, a valid NO_DISPATCH or dispatched ping trace as appropriate, and creates no formal run/READY state.

- [x] **Step 5: Run the real canary**

Run:

~~~powershell
go test -tags=cpgen_slice0_probe ./internal/probe -run TestSlice0DockerCapabilities -count=1 -v -timeout=15m
~~~

Expected on this host: compile-v2 and execute-mvp-v2 HEALTHY; execute-release-v2 explicitly BLOCKED because Docker Desktop is not an allowlisted rootful Linux release Engine.

- [x] **Step 6: Commit**

Run:

~~~powershell
git add -- internal/probe
git commit -m "phase1(slice-0): verify docker capability profile"
~~~

### Task 11: Add the fixed A+B compile, validate, differential, and Judge probe

**Files:**
- Create: internal/fixture/ab/reference.cpp
- Create: internal/fixture/ab/brute.cpp
- Create: internal/fixture/ab/validator.cpp
- Create: internal/fixture/ab/checker.cpp
- Create: internal/fixture/ab/generator.cpp
- Create: internal/fixture/ab/fixture.go
- Create: internal/fixture/ab/fixture_test.go
- Create: internal/probe/vertical.go
- Create: internal/probe/vertical_test.go

**Interfaces:**
- Consumes: Harness.Compile/Run, role adapters, trusted fixture blobs.
- Produces: Harness.RunVertical(ctx) (VerticalReport, error).

- [x] **Step 1: Write failing deterministic fixture and route tests**

The sources implement:

~~~cpp
// reference/brute contract
long long a, b;
if (!(std::cin >> a >> b)) return 2;
std::cout << a + b << '\n';
~~~

Validator accepts exactly two integers in [-1000000000, 1000000000] and EOF. Checker compares the single integer token and rejects trailing output. Generator uses a fixed numeric seed and emits boundary plus reproducible random cases.

Test valid/invalid, AC/WA/PE-or-DIRT, validator unknown, checker FAIL/unknown, solution nonzero/signal, TLE, MLE, OLE, and differential agreement.

- [x] **Step 2: Run and verify failure**

Run:

~~~powershell
go test -tags=cpgen_slice0_probe ./internal/fixture/ab ./internal/probe -run "Test(AB|Vertical)"
~~~

Expected: FAIL because fixtures and vertical harness do not exist.

- [x] **Step 3: Implement fixture blob loading and canonical bundle digests**

Embed only repository-controlled fixture bytes, insert them through ProbeArtifactSink, and build SourceBundleManifest with canonical sorted path/blob digest. Never pass a host path or archive into Runner.

- [x] **Step 4: Implement the vertical sequence**

Compile reference, brute, validator, checker, generator; validate legal and illegal input; generate fixed small inputs; run reference and brute; run checker for both; compare through CheckerOutcome rather than raw strings. Every call returns and validates a CallTrace. A Docker infrastructure failure yields a typed blocked report; content failure yields a failed probe report.

- [x] **Step 5: Run the real vertical probe**

Run:

~~~powershell
go test -tags=cpgen_slice0_probe ./internal/probe -run TestSlice0ABVertical -count=1 -v -timeout=15m
~~~

Expected: compile → validate → differential sequence PASS with deterministic artifact digests.

- [x] **Step 6: Commit**

Run:

~~~powershell
git add -- internal/fixture/ab internal/probe/vertical.go internal/probe/vertical_test.go
git commit -m "phase1(slice-0): add fixed vertical judge probe"
~~~

### Task 12: Implement minimal package builder, reverse reader, and StructuralGate

**Files:**
- Create: internal/packageprobe/model.go
- Create: internal/packageprobe/manifest.go
- Create: internal/packageprobe/builder.go
- Create: internal/packageprobe/reader.go
- Create: internal/packageprobe/gate.go
- Create: internal/packageprobe/gate_test.go
- Create: internal/packageprobe/path_test.go
- Create: internal/probe/package.go
- Extend: internal/probe/vertical_test.go

**Interfaces:**
- Consumes: synthetic VerifiedProblem, PrePackageQualityReport, fixture artifacts, ProbeArtifactSink.
- Produces:
  - packageprobe.Build(ctx, root, Problem) (Manifest, error)
  - packageprobe.Inspect(ctx, root, Limits) (VerifiedProblem, error)
  - packageprobe.Import(ctx, root, Limits, sink) (VerifiedProblem, []PendingArtifact, error)
  - Harness.RunVertical returns StructuralPackageReport.

- [x] **Step 1: Write failing structural and attack tests**

Cover path traversal, backslash/drive/NUL/control chars, NFC and case-fold collisions, link/special file, missing/extra file, hash/size tampering, duplicate test IDs, input without answer, unknown Schema fields, wrong package ID, manifest self-entry, total/file count limits, and concurrent replacement. The reverse reader must consume the same opened handles or freshly verified imported Blobs, never the observed path after import.

- [x] **Step 2: Run and verify failure**

Run:

~~~powershell
go test ./internal/packageprobe
~~~

Expected: FAIL because packageprobe does not exist.

- [x] **Step 3: Implement strict minimal package models**

Use schema_version cpgen.package/v1. The model includes manifest, statement/samples, reference/brute, validator/checker/generator, at least one .in/.ans pair, package-safe similarity report, synthetic prepackage report, provenance, toolchain manifest digest, and file entries. JSON decoding uses DisallowUnknownFields.

Package ID is the canonical manifest without package_id plus sorted path/digest/size/role entries. manifest.json is not in files.

Before importing `golang.org/x/text/unicode/norm`, pin and reconcile the dependency:

~~~powershell
go get golang.org/x/text@v0.29.0
go mod tidy
~~~

- [x] **Step 4: Implement safe build and read**

Builder writes only predeclared SafeRelPath files beneath an os.Root with O_CREATE|O_EXCL, calculates every digest/size, then writes manifest last. Reader walks with hard file/byte limits, rejects undeclared entries and all links/special files, verifies through same handles, and reconstructs VerifiedProblem.

Import prepares fixed ProbeArtifactSink declarations first, streams each verified same-handle file, validates PendingArtifact producer metadata, and reverse-reads only OpenVerified Blobs.

- [x] **Step 5: Prove Slice 0 cannot create formal verification state**

Keep PackageVerificationReceipt, READY, SQLite repository, and package occurrence types out of packageprobe and internal/probe. Add a source-level test that rejects imports of future persistence/workflow packages and asserts the report kind is PROBE_STRUCTURAL_ONLY.

- [x] **Step 6: Run package and vertical tests**

Run:

~~~powershell
go test ./internal/packageprobe
go test -tags=cpgen_slice0_probe ./internal/probe -run TestSlice0ABVertical -count=1 -v -timeout=15m
~~~

Expected: StructuralGate PASS and stable package ID; tamper fixtures fail.

- [x] **Step 7: Commit**

Run:

~~~powershell
git add -- internal/packageprobe internal/probe/package.go internal/probe/vertical_test.go
git commit -m "phase1(slice-0): add structural probe package"
~~~

### Task 13: Audit every Slice 0 exit condition and create the stage checkpoint

**Files:**
- Modify: TODO.md
- Modify: README.md
- Modify: docs/superpowers/plans/2026-08-31-slice0-execution-probe.md
- Create: docs/evidence/slice0-verification.md

**Interfaces:**
- Consumes: all Slice 0 tests and real Docker evidence.
- Produces: requirement-by-requirement evidence and the user-requested Slice 0 completion commit.

- [x] **Step 1: Run formatting and static checks**

Run:

~~~powershell
$bad = gofmt -l (rg --files -g '*.go')
if ($bad) { $bad; throw "gofmt required" }
go test ./...
go vet ./...
go test -race ./...
~~~

Expected: no gofmt output; all commands PASS.

- [x] **Step 2: Run fresh Docker verification**

Run:

~~~powershell
docker version
docker info
go test -tags=cpgen_slice0_probe ./internal/probe -run "TestSlice0(DockerCapabilities|ABVertical)" -count=1 -v -timeout=30m
~~~

Expected: mvp capability and vertical probe PASS; release capability is an explicit typed BLOCKED on Docker Desktop, not an omitted or false PASS.

- [x] **Step 3: Prove security and cleanup evidence**

Inspect test logs/results and Docker by the cpgen Slice 0 labels:

~~~powershell
docker ps -a --filter "label=org.cpgen.slice=0"
docker volume ls --filter "label=org.cpgen.slice=0"
~~~

Expected: no running/stopped probe containers and no leftover volumes. The evidence document records direct PID 1, UID, limits/no-swap, mount/network/socket/secret tests, OLE stop, OOM, keeper/export, watchdog/late-create, claim counts, CallTrace, package ID, and cleanup result.

- [x] **Step 4: Update the tracking documents only from proven evidence**

Mark every proven Slice 0 tracking item and exit condition complete. Keep any unproven requirement unchecked and continue implementation instead of claiming Slice 0 complete. README must say Slice 0 complete and point to Slice 1 only after all required evidence exists.

- [x] **Step 5: Check the implementation plan itself**

Mark every completed checkbox in this plan. Scan:

~~~powershell
$hits = rg -n -i "TBD|TODO|implement later|fill in|appropriate error|handle edge" docs/superpowers/plans/2026-08-31-slice0-execution-probe.md |
    Where-Object { $_ -notmatch 'TODO\.md|\$hits = rg' }
if ($hits) { $hits; throw "implementation plan still contains placeholders" }
git diff --check
~~~

Expected: no plan placeholders after filtering the literal scan command and TODO.md filenames.

- [x] **Step 6: Create the Slice 0 checkpoint commit**

Run:

~~~powershell
git add -- README.md TODO.md docs/superpowers/plans/2026-08-31-slice0-execution-probe.md docs/evidence/slice0-verification.md
git diff --cached --check
git commit -m "phase1(slice-0): complete execution probe"
~~~

Expected: the final Slice 0 stage commit exists after all exit conditions pass.

- [ ] **Step 7: Verify the checkpoint**

Run:

~~~powershell
git status --short
git log --oneline --decorate -15
go test ./...
go vet ./...
go test -race ./...
~~~

Expected: no unexplained changes; the Slice 0 checkpoint is visible; all default verification remains green.

## Slice 0 Coverage Map

| Requirement | Implemented by | Direct evidence |
|---|---|---|
| M0 typed scaffold and ADR-0003 vectors | Tasks 1–2 | default unit/vet/race |
| explicit local endpoint and no ambient context | Task 3 | endpoint/constructor/doctor tests |
| fixed images, toolchain, testlib | Tasks 4–5 | lock tests, image inspect, header hash |
| per-container grant and complete CallTrace | Tasks 2, 6–7 | ledger and fake Engine call-order tests |
| direct PID 1 and target isolation | Tasks 6, 10 | real canary Inspect/evidence |
| exact memory/no-swap/PIDs/OOM | Tasks 6, 8, 10 | mvp canary |
| import/keeper/export and safe promotion | Tasks 4, 7, 10 | transfer attacks and real volume canary |
| none+attach, OLE, cleanup | Tasks 8, 10 | limit+1 and daemon-log canary |
| detached watchdog and late Create | Tasks 9–10 | child-process and real Docker tests |
| A+B compile/validate/differential | Task 11 | TestSlice0ABVertical |
| minimal package StructuralGate | Task 12 | package attack tests and stable ID |
| no formal READY/receipt/run | Tasks 10–12 | source boundary tests and report schema |
| stage commit | Task 13 | git log plus fresh verification evidence |

## Execution Handoff

The plan is designed for inline execution because the user reserved a single subagent for the whole-Phase-1 review after Slice 5. If that constraint changes, the alternative is subagent-driven execution with a fresh implementer and review gate per task.
