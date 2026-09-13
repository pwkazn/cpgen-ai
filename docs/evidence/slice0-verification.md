# Slice 0 Verification Evidence

Date: 2026-08-31 (Asia/Shanghai)

Result: **PASSED** for the approved Slice 0 scope. The implementation provides a Docker-only compile → validate → differential → structural-package probe. It does not create a formal run, `READY` state, package occurrence, SQLite state, or `PackageVerificationReceipt`.

## Verification environment

- Host: Windows amd64; Go `go1.26.5`; module language version `go 1.24.0`.
- Docker client/server: `29.7.2`; API `1.55`.
- Engine: Docker Desktop `4.86.0`, Linux amd64, cgroup v2, cgroupfs, runc `1.3.6`.
- Explicit Engine endpoint used by the probes: `npipe:////./pipe/docker_engine`. Ambient Docker context and environment variables are not used by the Engine client.
- Execution protocol: `docker-direct-v2`.
- Builder image: `sha256:fe432330efb137a6d713a05de0c5310a6736a23f1882612456cb40283ca1f107`.
- Runtime image: `sha256:d4cbcfb1c9cf9de450b2f5296a9fce8631992608c878ccae0e69edffaa17f2b5`.
- Transfer image: `sha256:875576235bfbfa8ecd995175ee078beca2afae16ab99cd8dca9bfce726904b6a`.
- Vendored testlib: version `0.9.45`, commit `1e4e8a24c79c6bad3becbdb5a332ffc352b7d5dd`, SHA-256 `bb323e3c89285214966076e0d23d5a295c5f6126da7ff198c1276ddb95ecb1a0`.

## Fresh verification commands

The following commands were run from the isolated `codex/phase1` worktree after the Task 12 commit.

| Command | Result |
|---|---|
| `gofmt -l (rg --files -g '*.go')` | no output |
| `go test ./... -count=1` | PASS |
| `go vet ./...` | PASS |
| `go test -race ./... -count=1` | PASS |
| `go test -race -tags=cpgen_slice0_probe ./... -count=1` | PASS |
| `go vet -tags=cpgen_slice0_probe ./...` | PASS |
| Linux amd64 cross-build through `go test -exec 'cmd.exe /c exit 0' -tags=cpgen_slice0_probe ./...` with `CGO_ENABLED=0` | PASS |
| `docker version` and `docker info` | PASS; local Linux Engine and cgroup v2 observed |
| `CPGEN_RUN_DOCKER_CANARY=1 go test -tags=cpgen_slice0_probe ./internal/probe -run 'TestSlice0(DockerCapabilities|ABVertical)' -count=1 -v -timeout=30m` | PASS; vertical 43.04 s, capability 20.89 s |
| real `TestWatchdogDockerOwnerEOFCanary`, `TestDockerTargetInspectCanary`, and `TestDockerTransferLifecycleCanary` | PASS |
| `docker ps -a --filter 'label=org.cpgen.slice=0'` | zero containers |
| `docker volume ls --filter 'label=org.cpgen.slice=0'` | zero volumes |

## Exit-condition matrix

| Requirement | Authoritative evidence | Result |
|---|---|---|
| Typed scaffold and ADR-0003 outcomes | domain, port, Judge and fixed-vector unit tests in the default suite | PASS |
| Explicit local endpoint; reject TCP, SSH, remote/ambient configuration | `TestParseLocalEndpointAcceptsOnlyExplicitPlatformLocalTransports`, `TestNewEngineClientIgnoresDockerEnvironment`, doctor tests, and the explicit npipe real probes | PASS |
| Fixed immutable images, toolchains and testlib | strict lock parser tests, image-lock generator tests, pinned image IDs above, and fresh real image use | PASS |
| Canonical resource plans and one claim per physical create | plan/ledger/Runner call-order tests; MVP capability trace contains exactly 13 physical calls | PASS |
| Direct PID 1 and non-root target | the real MVP introspection program requires PID 1 and UID/GID 65532 | PASS |
| Target isolation | real introspection plus exact Inspect verification require `network none`, read-only rootfs, `CapDrop=ALL`, no-new-privileges, no Docker socket, no host bind, and no host Docker environment | PASS |
| Exact memory, no swap and PIDs | real introspection requires `memory.max=67108864`, `memory.swap.max=0`, and `pids.max=16`; exact create/inspect drift tests also pass | PASS |
| PID 1 and child OOM classification | mandatory real `P` and `C` canaries both produce MLE; fake mandatory-canary mutation fails closed | PASS |
| Import, quota output, keeper and read-only export | real compile/capability flow plus `TestDockerTransferLifecycleCanary`; malformed, same-size corruption, special-file and traversal attacks are rejected | PASS |
| `LogConfig=none`, Attach and OLE stop | exact spec tests and real `O` canary; limit+1/portable-stop unit tests prove bounded capture and stop evidence | PASS |
| Watchdog protocol, ACL, deadline, owner EOF and late Create | protocol/ACL/foreign/late-create tests, immediate-rearm race regression, detached child tests, and fresh real owner-EOF canary | PASS |
| Docker unavailable is typed, without host fallback | fake Engine and unused local socket/pipe tests require `unavailable/BLOCKED`, retain a valid trace, and dispatch no container canary | PASS |
| Release capability does not false-pass | fresh capability test requires `capability_missing/INCOMPATIBLE` and one dispatched ping call on this Docker Desktop host | PASS |
| A+B compile → validate → generate → differential → checker | fresh real vertical: five fixed programs, legal/illegal validator boundary, six seeded cases, four checker attacks, and exactly 42 logical call traces | PASS |
| Structural package and adversarial reverse read | builder/reader/import tests reject path, Unicode/case, links/special files, missing/extra/hash/size/schema/ID/limit attacks and verify imported blobs; fresh vertical builds the same package identity | PASS |
| No formal verification state in Slice 0 | build-tag boundary test rejects workflow/persistence/SQL imports, `READY`, and `PackageVerificationReceipt`; report kind is exactly `PROBE_STRUCTURAL_ONLY` | PASS |
| Cleanup | fresh post-canary Docker queries found zero Slice 0 containers and zero Slice 0 volumes | PASS |

## Stable fixture and package identities

- Reference and brute: `sha256:8ed1de2cdafb1650b1695d2dcb10333fd443690e6a03ece6b77d71523e10e7c1`.
- Validator: `sha256:fc1d431b643d790821aabe6742856485905fd4e23886e1af14a55fe2a3aff578`.
- Checker: `sha256:557483a66576c44a98e3ccc53b258519fcad81506e28b77f7b922f38635cd441`.
- Generator: `sha256:48786b254ff33ed340a877005c6040e3dfc3ebcb6118d5cd91a458db27689fb7`.
- Structural probe package ID: `sha256:e680b9bd726499a1946c1fa3ae5508a14cc832f4fb526a4e80650c8ab435e4a3`.
- Structural manifest Blob: `sha256:c5666c6aba707e49948bab25d691e1e4192ea4527ef1f78ee7eae04f0538cfd1`.
- Package contents: 23 declared files and 24 imported artifacts including `manifest.json`.

## Deliberate boundary

Full rootful Linux release-cgroup measurement (path/nonce/owner epoch/inode/device and `populated=0`) remains a Slice 3 deliverable. Slice 0 proves that this Docker Desktop environment cannot satisfy that profile and returns the required typed incompatible result; it does not weaken isolation or claim release readiness.
