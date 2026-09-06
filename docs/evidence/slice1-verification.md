# Slice 1 Verification Evidence

Date: 2026-09-07 (Asia/Shanghai)

Branch: `codex/phase1`

Scope: the lightweight local workflow checkpoint from Slice 0 commit
`d49c0b1` through the completed Task 10 implementation and this Task 11
checkpoint. The implementation remains one foreground Go executor per run;
there is no workflow-hosting service, daemon, task queue, or general durable
runtime.

Result: **PASSED with documented environment limits.** Architecture, source
boundaries, migrations, SQLite integrity, filesystem maintenance tests, full
Go tests, vet, race, tagged probe tests, Linux cross-test, and patch checks
pass. The real Docker canaries are explicitly skipped because this host has
no reachable Docker daemon. The repository also has no `cmd/diffcheck`
directory; the existing `cmd/...` set builds successfully and that pre-existing
layout warning is recorded below.

## Verification environment

Exact toolchain gate:

```text
> $env:GOTOOLCHAIN='go1.24.13'; go version; go env GOVERSION GOOS GOARCH CGO_ENABLED
go version go1.24.13 windows/amd64
go1.24.13
windows
amd64
1
```

Module and dependency verification:

```text
> go mod verify
all modules verified

> go list -m -json go.yaml.in/yaml/v3
{
    "Path": "go.yaml.in/yaml/v3",
    "Version": "v3.0.3",
    "GoVersion": "1.22",
    "Sum": "h1:bXOww4E/J3f66rav3pX3m8w6jDE4knZjGOw8b5Y6iNE=",
    "GoModSum": "h1:tBHosrYAkRZjRAOREWbDnBXUf08JOwYq++0QNwQiWzI="
}
```

The JSON output above omits only local cache path fields; the command returned
exit code 0 and resolved the required YAML v3.0.3 pin.

## Architecture and source-boundary audit

Commands and results:

```text
> pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
Slice 1 architecture check passed for 26 normative file(s).

> rg -n 'go.temporal.io|langgraph|autogen|crewai' go.mod go.sum internal cmd
<no matches>

> rg -n 'database/sql|modernc.org/sqlite|internal/runlock|adapter/storage|adapter/sandbox/docker' internal/workflow internal/adapter/fake
internal/workflow\source_boundary_test.go:25:            for _, forbidden := range []string{"internal/adapter/storage/sqlite", "internal/runlock", "Services", "map[string]any", "[]Step["} {

> rg -n -i 'SetState|SetStage|type\s+Services\b|execution[ _-]?lease|lease[ _-]?epoch|owner[ _-]?epoch|\bOwnerID\b|fencing([ _]?(token|epoch))?|Cause(LeaseLost|Quiesce)|\bPROBING\b|\bQUIESCING\b|recovery[ _-]?intent|observation[ _-]?(ticket|floor)|startup[ _-]?janitor' internal cmd -g '*.go' -g '!*_test.go'
<no matches>

> rg -n -i 'TBD|TODO|implement later|fill in|appropriate error|handle edge|similar to' internal cmd
<no matches>
```

The one source-boundary result is an intentional negative-test fixture: the
test enumerates forbidden strings so production workflow/fake source cannot
import those capabilities. The only other boundary vocabulary match is the
intentional explanatory `PROBING` comment in
`internal/integration/slice1_docker_crash_test.go`; it is test documentation,
not production behavior. The negative tests remain excluded from the
production-only scans above.

## Migration and database integrity

The ordered migration chain is M1–M18. SHA-256 hashes of the embedded SQL
files are:

| Version | File | SHA-256 |
|---:|---|---|
| 1 | `000001_workflow_core.sql` | `868896872f3ef7d686e50431eb35e5d6e22c0a058ce3c17ab323775cfe70050c` |
| 2 | `000002_workflow_invariants.sql` | `fe1e3bbf31aa14df8f69e5be161266a2b7f8fc6ae05217f0b15424f78eebd094` |
| 3 | `000003_call_budget.sql` | `e2abddad5db8e12611c2373f4e2d821cd1c255faab77df5984be1500c4e017be` |
| 4 | `000004_call_budget_hardening.sql` | `4a97274482009bda039717a935b1e7d38909875873c05500d22fa97a8c9fd188` |
| 5 | `000005_call_budget_terminal_guards.sql` | `11988279575ef6d43d1c7ac8f9c02eb066dd5b2534394eb64ebaf31a4e0c896f` |
| 6 | `000006_artifacts.sql` | `3b368d47010a310f0345000e4f522c70207f3fcf7ca59b716990181e50fa3b9e` |
| 7 | `000007_artifact_scope_guards.sql` | `949d6de22ca454c8be32a79be856c6df5a48dd494be5ee8148ba9c76b77e1f17` |
| 8 | `000008_artifact_recovery.sql` | `aa9a29542813a9bd73e48cea5acd1fcc3e8202dabf092c9454d5d3b74a54eac0` |
| 9 | `000009_artifact_publication_owners.sql` | `5ba63baf2bc96a8aa53dad57f3d5b9c0a4b648ef7b858c7297e876465222df62` |
| 10 | `000010_artifact_publication_owner_guards.sql` | `1450d958eb7924cfa886d9a3bca147909718f7e20b66e86cc62e7afebe61b987` |
| 11 | `000011_cache_mutation.sql` | `87f5b82fc9cc5d0185ddeb0829c1599798ed0e735fd2e83df4bdaa5622dfc038` |
| 12 | `000012_cache_mutation_hardening.sql` | `e5d35a3fa530ed2cdbbfe22d9d0611ddbce8d445c1b4e2cc57e2a8c090a6530d` |
| 13 | `000013_cache_blob_order.sql` | `d462ad8c8ebba47ea83c6d8f653a082e73bd27341f731d40b992064e70578b61` |
| 14 | `000014_sandbox_execution.sql` | `711e02f0be5adee88f95f9a717c01b705efc3df0d6b979b701f6d0c09b679cbe` |
| 15 | `000015_sandbox_cleanup_evidence.sql` | `cbefc3e53a9bf700b60d1bdcf7bf90eb2753d7b18999dca67333961f52db2560` |
| 16 | `000016_sandbox_resource_call_scope.sql` | `9ea9edd571a4e27bd658cbfdb7ef1e60a1ef554840a32caaed30b5c4d98a8fc8` |
| 17 | `000017_sandbox_volume_call_compat.sql` | `4819cac76bed3e2d9ff8de21e1583d5dfb86ffc2e0af9f75651c66a9099549a5` |
| 18 | `000018_stage_attempt_blocked_binding.sql` | `541186b69fa95fa8700d48584c1f64c1ea0c794c0ba8333cc154fdcd73eb8d12` |

The migration and integrity fixture suite was run with:

```text
> $env:GOTOOLCHAIN='go1.24.13'; go test ./internal/adapter/storage/sqlite -run 'TestMigration' -count=1 -v
--- PASS: TestMigrationThirteenBackfillsSameDigestBlobsByRole
--- PASS: TestMigrationSimultaneousFirstOpenIsIdempotent
--- PASS: TestMigrationOnePreservesHistoricalBytes
--- PASS: TestMigrationPreservesAppliedCallBudgetBytesAndUpgradesTerminalGuards
--- PASS: TestMigrationFreshOpenAppliesForwardWorkflowMigration
--- PASS: TestMigrationUpgradesM14VolumeWithoutPhysicalCallID
--- PASS: TestMigrationNineBackfillsPhysicalBytesByHistoricalPinIdentity
--- PASS: TestMigrationTenBindsAndProtectsPublicationOwner
--- PASS: TestMigrationUpgradesHistoricalWorkflowDatabase
--- PASS: TestMigrationRecordsVersionNameAndHashAndReopens
--- PASS: TestMigrationRejectsChangedHash
--- PASS: TestMigrationRejectsMissingVersion
--- PASS: TestMigrationLeavesHealthyDatabase
PASS
ok   cpgen/internal/adapter/storage/sqlite  5.013s
```

`TestMigrationRecordsVersionNameAndHashAndReopens` verifies all applied
version/name/hash rows. `TestMigrationLeavesHealthyDatabase` executes both
`PRAGMA foreign_key_check` (zero rows) and `PRAGMA integrity_check` (`ok`).
Every test database is a private temporary fixture and is removed by the test
harness. The only retained storage fixture is the historical SQL seed
`internal/adapter/storage/sqlite/testdata/000001_workflow_core_691b611.sql`;
there are no retained `.db` or `.sqlite` files in the repository.

Artifact maintenance and root safety were checked under the exclusive
maintenance guard:

```text
> $env:GOTOOLCHAIN='go1.24.13'; go test ./internal/application -run 'Test(ArtifactMaintenance|ReconcileTrash)' -count=1 -v
--- PASS: TestArtifactMaintenanceHoldsExclusiveGuardThroughFilesystemCommit
--- PASS: TestArtifactMaintenanceReconcileTrashRejectsReplacementWithoutFollowingIt
PASS
ok   cpgen/internal/application  1.060s
```

The recursive canonical/temp/quarantine/trash scan of the worktree produced no
such runtime roots. Test-owned temporary roots are private `t.TempDir()`
directories and are empty after teardown; canonical files are opened through
the verified Blob reader and filesystem tests reject non-regular, multi-link,
symlink, junction, traversal, and untracked trash replacements.

## Process lock, crash, and failpoint evidence

The real-process integration matrix passed:

```text
> $env:GOTOOLCHAIN='go1.24.13'; go test ./internal/integration -run 'TestSlice1(Process|Crash|LockBoundary)' -count=1 -timeout=20m -v
--- PASS: TestSlice1CrashDurableBoundariesConvergeAfterForceKill
    run_create, stage_begin, budget_reserve, dispatching, sent,
    physical_completion, blob_sealed, blob_published, blob_ready,
    occurrence_commit, sandbox_resource, stage_finish: PASS
--- PASS: TestSlice1LockBoundaryDifferentRunAndReadOnlyProgressWithinBusyBound
--- PASS: TestSlice1LockBoundaryCancellationIsDurableAndIdempotent
    network, docker, blob, watchdog, reconciler: PASS
--- PASS: TestSlice1ProcessOneExecutorPerRunAndForcedKillReleasesLock
--- PASS: TestSlice1ProcessDifferentRunsProgressConcurrently
--- PASS: TestSlice1ProcessReadOnlyViewsAndCancelDuringExecution
--- PASS: TestSlice1ProcessLiveCancelSurvivesOwnerDeath
PASS
ok   cpgen/internal/integration  4.866s
```

This proves same-run exclusion, different-run concurrency, release after
forced process death, durable cancellation delivery, and no SQLite writer held
across the five blocked external-adapter boundaries. The failpoint matrix
contains twelve named durable boundaries and the second resume is an
idempotent replay with contiguous events and no duplicate effects.

The first full tagged race run exposed a race in the test helper's concurrent
stderr diagnostic read. This was corrected in verification-only commit
`13ecbf5` by synchronizing that test buffer; no production code changed. The
targeted regression and the complete tagged race gate then passed.

## Docker capability and cleanup evidence

The host has the Docker CLI but not a reachable daemon:

```text
> docker version
Client: Version 29.7.2, API version 1.55, OS/Arch windows/amd64, Context desktop-linux
failed to connect to the docker API at npipe:////./pipe/dockerDesktopLinuxEngine

> $env:CPGEN_RUN_DOCKER_CANARY='1'; go test -tags=cpgen_slice0_probe ./internal/integration -run 'TestSlice1Docker(AB|Crash|WatchdogFailure)$' -count=1 -v -timeout=45m
--- SKIP: TestSlice1DockerCrash — Docker canary unavailable: ... npipe:////./pipe/docker_engine ...
--- SKIP: TestSlice1DockerWatchdogFailure — Docker canary unavailable: ... npipe:////./pipe/docker_engine ...
--- SKIP: TestSlice1DockerAB — Docker canary unavailable: ... npipe:////./pipe/docker_engine ...
PASS
ok   cpgen/internal/integration  0.269s
```

The skip is the typed `unavailable/BLOCKED` capability result, not a host
fallback. Consequently no Docker cleanup query can reach an engine on this
host; the local lifecycle tests still verify exact cleanup identity, watchdog
EOF/death handling, stop proof, and no unrelated-resource mutation. The
canaries must be rerun with `CPGEN_RUN_DOCKER_CANARY=1` on a compatible Docker
host before claiming real-engine Slice 1 coverage.

## Go and patch gates

Commands run with `GOTOOLCHAIN=go1.24.13`:

| Command | Result |
|---|---|
| `gofmt -l cmd internal` | no output |
| `go test ./... -count=1` | PASS; all packages, including `internal/integration` |
| `go vet ./...` | PASS |
| `go test -race ./... -count=1` | PASS; all packages |
| `go test -tags=cpgen_slice0_probe ./... -count=1` | PASS; all packages |
| `go vet -tags=cpgen_slice0_probe ./...` | PASS |
| `go test -race -tags=cpgen_slice0_probe ./... -count=1` | PASS after `13ecbf5` |
| `go test -exec 'cmd.exe /c exit 0' ./...` with `GOOS=linux`, `GOARCH=amd64`, `CGO_ENABLED=0` | PASS; all packages |
| `go build ./cmd/...` on Windows and Linux/amd64/cgo0 | PASS |
| `go mod verify` | PASS (`all modules verified`) |
| `git diff --check` | PASS (the checkpoint worktree was clean after commit) |

The requested explicit command `go build ./cmd/cpgen ./cmd/diffcheck` returns
`stat ...\cmd\diffcheck: directory not found` because this checkout contains
`cmd/cpgen`, `cmd/cpgen-image-lock`, and `cmd/cpgen-transfer`, but no
`cmd/diffcheck`. This is a pre-existing repository layout warning; no
unrelated CLI was invented in the checkpoint.

## Slice 1 commit ledger

All commits after the Slice 0 checkpoint `d49c0b1` and before this evidence
checkpoint are listed here. The checkpoint commit itself is the commit that
adds this file and the status updates below.

```text
a98474f158bc1339a127df7bf57f6d5556e7b5b1 docs(architecture): choose lightweight local workflow
581b609a9406d400d71afa576a2849bcdfe1dea5 docs(slice-1): plan lightweight local workflow
efa7c1791c1bf195ce5fbcbfaca2c997aa0040be docs(slice-1): adopt lightweight local workflow
284aaedc93720ef6417739386e66347b161fe220 docs(slice-1): clarify GC lock scope
b7bcc0483a00c8e8a3fcde62d52cd622939f4106 phase1(slice-1): add local lifecycle and run locks
93f3100d3fb8b3ae1f3b7023ade6a296b3d8b15b phase1(slice-1): harden lifecycle and lock validation
691b6110275a9b9b7d8e31d8e2daae913f93f2fe phase1(slice-1): persist lightweight workflow state
c51b6e01cea42aea80d5d8357676c1f10e02eb19 phase1(slice-1): harden persisted workflow invariants
5a2c09207c98d905f1ba029eacd1e19b899c01c4 phase1(slice-1): add forward workflow migration
a4f3b48563e2d8e7f934df92545e1e4a147051b6 phase1(slice-1): preserve legacy create replay
01751a3d0e5b08e92efeb8f26454b5a94bc97b86 phase1(slice-1): add call and budget ledgers
745bc306c40154f8a263a39223c7bd8a50c26af5 phase1(slice-1): harden call budget enforcement
3e0281ea58a51586230ae3fd822079cdd805f084 phase1(slice-1): close terminal insert invariant
b81f368520c7e10629598042a213c1be8b7b088b phase1(slice-1): preserve migration history
e3ecf43f2dbc49930b9a33f4b3da85cc447b2675 phase1(slice-1): add verified artifact ledger
6b6b0df57f0bfd9ebf29ee7144090887aa655f94 phase1(slice-1): harden artifact integrity
cd3e59e45bab1ff77284447e1f41d1c60c4a5cea phase1(slice-1): harden artifact recovery
a3eba62c8e12e1c1c9fc62d5d6cfba34c3ecc36e phase1(slice-1): close artifact publication races
ba715459a16fdf0156da452dfffdf0ad1145495e1 phase1(slice-1): anchor artifact roots
42b7edaf704463a7e9779842f621969c3e7a8481 phase1(slice-1): close windows directory race
900c1ad22bab3dddd92b50b78847fa60381d782f phase1(slice-1): add cache and maintenance ledgers
b99373dab11063769f746005a925b79de7bf1835 phase1(slice-1): harden cache and mutation integrity
899cb6c17d11b1770247c56af0aa0f5a721ecc10 phase1(slice-1): close cache reuse ordering
0a3813bc862714983eb9d2bbd08f5a0d6edaa991 phase1(slice-1): persist sandbox cleanup evidence
2bb127fb41ffad8be3c9ae6e4a594bb07308ede5 fix: harden sandbox lifecycle boundaries and replay identity
af344b3a7307ecf6574bc5e8b2fd7578b6d299f1 fix: close sandbox lifecycle recovery boundaries
01855bf3ec6a72a0a21c6f00ca09f4bcb5b8d2c0 fix: harden sandbox resource call scope and cleanup
3665094c56aacea892cd207fcd6775a9e4222f58 fix: close sandbox watchdog lifecycle gaps
f49173233c48d4d1f4b525a6b5c4e12c1bb61395 fix: harden sandbox lifecycle replay
cbae2fd758125c9c7393da84e60ff1ef6ab870e4 fix: close sandbox reconciliation settlement gaps
3b6eb30b7587c543d117d96553333500944f7f22 phase1(slice-1): run fixed local workflow
76999c23b7af7ae2cb1a75bd51b75402956d30b9 fix: harden local workflow recovery
6c16147dffa4f2a6a8be2d470a0512868f5e3ed9 phase1(slice-1): expose local workflow CLI
f074d999bcad9b6e47b22a82e7769b795aeb1a24 fix: harden local workflow CLI
c46bdf193a5e444b18a61c483336a1c001110c64 fix: tighten local workflow CLI errors
047a3edeeda169120345208a4a86d606a38fd63a fix: return not-found for missing reviews
c068784dc09e75033a3d7242e98b86517f23348f fix: tighten config and event lookup
5d00487fff753c701fc49dd228cffcb28a31b469 phase1(slice-1): prove local crash recovery
2c0f85b613d52ec4b58ad0b1854f038746d6cf48 fix: harden crash recovery evidence
6387389deb9d0fe721ca14795c4e7ba8d6e3ad25 fix: make crash evidence executable
73b35b89c1e1aa56188accd6a33479c313f57047 docs: record Task10 fix rereview
13ecbf519b31361bf12304fefdf431687a903931 test: synchronize crash helper diagnostics
```

Slice 1 is complete. Slice 2 is the next planned checkpoint.
