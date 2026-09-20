# CPGen — auditable competitive-programming problem generation

CPGen turns a structured request into a **verified, reproducible problem package**:
statement, reference/brute solutions, generator, validator, checker, test data, and a
canonical ZIP export.

The design premise is that a language model is a *candidate generator*, never an
authority. Every acceptance decision is made by deterministic code — a compiler,
actual execution in a sandbox, differential judging, similarity policy, and package
gates. A model that hallucinates a wrong reference solution produces a **rejected
run**, not a bad problem in your archive.

~~~text
Request
  -> Idea -> Statement -> Similarity
  -> Solution -> Data -> Judge -> Quality
  -> Package gates -> READY
~~~

Generative models propose. Deterministic validators decide.

## Why this is not a wrapper around an LLM API

The interesting engineering is in making an unreliable generator safe to use and
cheap to re-run:

- **Budget ledger with two-phase settlement.** Nine budget dimensions (model calls,
  tokens, cost, similarity calls, sandbox runs, artifact bytes, stage attempts, active
  wall time) are *reserved before* irreversible work and settled monotonically after.
  Every physical external call carries a stable logical identity and a physical
  `CallTrace`, so retries and crash recovery settle each effect exactly once.
- **Untrusted code execution boundary.** Generated C++ is compiled and executed only
  through a dedicated Docker runner: non-root identity, dropped capabilities,
  read-only root, explicit mounts, pids/memory/CPU limits, no socket exposure. A
  **detached watchdog process** stops planned targets on deadline or process loss, and
  a narrow reconciler can clean up *only* the exact persisted resource identities.
- **Recovery with graded guarantees.** A process that dies mid-run is handled by
  stage-specific rules rather than a generic retry: rerun if no external effect was
  authorized; replay or reconcile the same stable provider identity if one was sent;
  charge conservatively and pause on an unknown send boundary. `READY` is never
  reachable from a partially verified state.
- **Content-addressed artifact store.** Immutable SHA-256 blobs written through
  declaration → writer token → verification → pin → occurrence, binding bytes to run,
  revision, role, and producer evidence.
- **One invariant held everywhere:** no external I/O ever occurs inside a SQLite write
  transaction. Provider calls, Docker calls, hashing, fsync, and blocking waits all
  happen outside; short transactions commit projections and effects atomically.

## Quick start

Requires Go 1.25.0 or later and a local Docker Engine.

~~~bash
go build ./cmd/...

# 1. Validate your configuration without touching the network.
#    `validate` takes no flags; `effective` requires --redact.
./cpgen --config config/mvp.example.yaml config validate
./cpgen --config config/mvp.example.yaml config effective --redact

# 2. Confirm the host, Docker engine and toolchain are usable.
./cpgen doctor --json --engine-endpoint unix:///var/run/docker.sock \
  --api-version 1.43 --builder-image sha256:... \
  --runtime-image sha256:... --transfer-image sha256:... \
  --execution-protocol docker-direct-v2

# 3. Start a run (foreground; one process per run).
./cpgen --config config/mvp.example.yaml generate --request config/mvp.request.yaml

# 4. Inspect, cancel, or resume it.
./cpgen --config config/mvp.example.yaml run list
./cpgen --config config/mvp.example.yaml run show   RUN_ID --json
./cpgen --config config/mvp.example.yaml run events RUN_ID
./cpgen --config config/mvp.example.yaml run resume RUN_ID
./cpgen --config config/mvp.example.yaml run cancel RUN_ID

# 5. Export the verified package, then re-verify it independently.
./cpgen --config config/mvp.example.yaml run export RUN_ID --output ./problem.zip
~~~

Copy an `*.example.yaml` from `config/` and replace the paths, endpoints, and the
all-zero toolchain lock digest. See [config/README.md](config/README.md).

Errors are typed and machine-readable, and configuration problems are caught before
any network or Docker work begins:

~~~console
$ cpgen --config config/mvp.example.yaml config validate
{"schema_version":"cpgen.cli/v1","status":"ERROR","error":{"code":"config_invalid",
 "message":"sandbox.toolchain_lock_path: must be absolute",
 "field":"sandbox.toolchain_lock_path"}}
~~~

Every command maps failures to **stable exit codes** and stable error `code` values,
so the CLI can be driven from scripts without parsing prose.

Business outcomes come from the run's state, which lets a script distinguish "this
problem needs a human" from "the tool broke":

| Exit | Run state |
|---:|---|
| 0 | `READY` (or another success path) |
| 5 | `BLOCKED` |
| 6 | `NEEDS_REVIEW` — waiting on a human decision |
| 7 | `FAILED` |
| 8 | `CANCELLED` |

Operational failures carry a distinct code and a stable `error.code`:

| Exit | Meaning | `error.code` |
|---:|---|---|
| 2 | Malformed input or invalid configuration | `config_invalid` |
| 3 | Run or object not found | `not_found` |
| 4 | Another process holds the run lock | `lock_busy` |
| 5 | Invalid state transition | `invalid_state` |
| 7 | Concurrent version conflict | `version_conflict` |
| 9 | Unclassified operation failure | `operation_failed` |
| 10 | Sandbox cleanup still pending | `cleanup_pending` |

### What you get back

`run export` produces a `cpgen.package/v2` archive that a judge system can consume
directly:

~~~text
problem.zip
  statement/statement.md
  solution/reference.cpp
  solution/brute.cpp
  judge/generator.cpp
  judge/validator.cpp
  judge/checker.cpp
  data/tests.json
  tests/01.in  tests/01.ans
  ...
  report/quality.json
  report/provenance.json
~~~

The reference solution in that archive was compiled and executed against every test,
the brute force was differentially compared, and the whole archive can be
revalidated by an independent CLI invocation.

### Inspecting a run in progress

`run show --json` emits a stable, versioned projection that any frontend or
dashboard can consume — the CLI is not the only possible client:

~~~json
{"schema_version":"cpgen.cli/v1","status":"RUNNING","data":{
  "run_id":"run_ca074cfc318cd51f78b005a09eff8451",
  "state":"RUNNING","version":86,
  "workflow_revision":"mvp.idea.statement.similarity.solution.data.judge.package.v1",
  "current_stage":"solution","current_stage_ordinal":5,
  "request_digest":"sha256:b77be0c25168ec5768cedefa330179beb12fb46f2f88b72c57ab4fe078551573",
  "config_digest":"sha256:38f0cc4eca50edf3472bb9593401d550f479576b186003d368e1360f0b9576cd",
  "active_elapsed":68156221200}, "run_version":86}
~~~

## Architecture

~~~text
                 +-----------------------------+
   cpgen CLI --> |  application coordinator    |  fixed typed stage loop
                 |  run lock / attempts /      |  (compiled in, not a runtime
                 |  review / cancellation      |   graph, not a hosted service)
                 +--------------+--------------+
                                |
        +-----------------------+-----------------------+
        |                       |                       |
   +----v-----+          +------v------+         +------v-------+
   | execution|          |  artifact   |         |  adapter/    |
   | metered  |          |  content-   |         |  sandbox/    |
   | LLM +    |          |  addressed  |         |  docker      |
   | similar. |          |  blob store |         |  + watchdog  |
   +----+-----+          +------+------+         +------+-------+
        |                       |                       |
   +----v-----------------------v-----------------------v-------+
   |  SQLite: projections + ledgers (budgets, calls, artifacts,  |
   |          sandbox resources, reviews, packages)              |
   +-------------------------------------------------------------+
~~~

Phase 1 is deliberately small in scope: **one host, one private workspace, one
foreground executor per run, a fixed pipeline compiled into the binary, and SQLite as
the authoritative store.** There is no daemon, task queue, remote worker, or arbitrary
runtime graph. Different runs may proceed concurrently in separate processes; a
single run never has two mutating executors.

Full system contract: [ARCHITECTURE.md](ARCHITECTURE.md).
Decisions and their tradeoffs: [docs/adr](docs/adr).
Per-component detail: [docs/design](docs/design).
The two problems that shaped the design: [docs/DESIGN-NOTES.md](docs/DESIGN-NOTES.md).

## Design decisions worth reading

Each of these records a path *not* taken, which is usually the more informative half:

| ADR | Decision |
|---|---|
| [0001](docs/adr/0001-static-typed-workflow.md) | Static typed pipeline and activity contracts |
| [0002](docs/adr/0002-run-state-review.md) | Seven run states, review, retry, cancellation, restart |
| [0003](docs/adr/0003-judge-outcomes.md) | Judge outcomes and deterministic precedence |
| [0004](docs/adr/0004-docker-direct-execution.md) | Direct target execution in an isolated Docker cgroup |
| [0005](docs/adr/0005-docker-execution-lifecycle.md) | Execution lifecycle, watchdog, cross-stop transfer |
| [0006](docs/adr/0006-lightweight-local-workflow.md) | Lightweight local workflow boundary |

ADR-0006 is amended to replace an earlier LangGraph-based scheduler with a local
fixed loop, keeping SQLite authoritative and preventing provider library types from
leaking into domain contracts.

## Verification

Correctness claims in this repository are backed by recorded evidence in
[docs/evidence](docs/evidence), not by assertion. Release gates are enforced in CI:

~~~bash
go test ./...
go vet ./...
go test -race -timeout 30m ./...
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
git diff --check
~~~

CI (`.github/workflows/ci.yml`) additionally verifies `go mod verify` / `go mod tidy`
cleanliness, `gofmt`, an executable architecture-consistency check over the documented
file set, and Linux plus Windows cross-builds, on Go 1.25.0 and the current stable
release.

Tests cover the failure paths that matter for this kind of system: process death at
every durable stage boundary, two-process lock exclusion and release after abnormal
termination, unknown provider send boundaries, Blob corruption and traversal, watchdog
death and deadline handling, and the assertion that no external I/O occurs inside a
SQLite write transaction. Tests requiring a real Docker Engine or a paid provider are
opt-in and clearly marked, so the default suite is deterministic and offline.

## Project status

Slice 0 (execution foundation), Slice 1 (lightweight local workflow), and the MVP
generation loop — Similarity ACCEPT → Solution → Data → Docker/Judge → Quality →
Package → `READY` — are complete and pass real Docker plus independent CLI acceptance
for an ordinary C++ problem, including transaction-crash recovery and recompilation
from the exported ZIP.

Known limitations, stated plainly:

- Similarity/originality checks currently run against a **local fixture**, so
  originality against a real problem archive is not established.
- Special judges (SPJ), solution minimization, and generic untrusted import execution
  are out of scope for the current MVP.
- Automatic problem mutation is deferred; non-accepted business results stop for
  human review by design.
- Acceptance evidence for the full loop is for ordinary C++ problems; Go end-to-end
  acceptance and external service availability remain follow-up work.

[docs/development-log.md](docs/development-log.md) and
[docs/internal/TODO.md](docs/internal/TODO.md) track the detailed state;
[docs/traceability.md](docs/traceability.md) maps requirements to design and
evidence.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/` | `cpgen`, `cpgen-image-lock`, `cpgen-transfer` executables |
| `internal/domain` | Versioned immutable domain values and contracts |
| `internal/port` | Capability interfaces between layers |
| `internal/application` | Run coordination and the fixed typed stage loop |
| `internal/execution` | Metered LLM/similarity calls, retry, receipts, cache |
| `internal/adapter` | Docker sandbox, SQLite storage, blob store |
| `internal/cli` | Command surface, progress events, exit codes |
| `docs/` | ADRs, component design, evidence, plans |
| `config/` | Example configurations and toolchain locks |

## License

MIT — see [LICENSE](LICENSE).
