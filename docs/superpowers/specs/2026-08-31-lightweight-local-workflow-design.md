# Lightweight Local Workflow Design

Status: Accepted

Date: 2026-08-31

## 1. Decision

Phase 1 uses one foreground Go CLI executor per run on a single host, a local SQLite database, private content-addressed artifact storage, and the existing detached Docker watchdog. Different runs may use separate CLI processes concurrently. It does not require Temporal, hosted LangGraph, AutoGen, CrewAI, a daemon, a task queue, or any workflow-hosting service. The 2026-09-08 ADR-0006 amendment admits LangGraphGo only for fixed serial assembly inside `internal/application` and LangChainGo only inside `internal/agent`; SQLite and CPGen domain ledgers retain all durable authority.

The normative contract is: one foreground executor per run, a per-run process lock, a fixed pipeline, and no workflow-hosting service.

The workflow is a fixed, statically assembled CPGen pipeline. SQLite persists the current run and stage projection for audit and restart; it is not an event-sourced general workflow engine. Restart recovery reruns or reconciles the current domain stage under a local process lock. The design does not attempt distributed ownership, generic DAG scheduling, or exactly-once execution across external systems.

This decision supersedes the uncommitted 16-task Slice 1 plan that proposed a general durable runtime with execution leases, fencing epochs, observation tickets, three-phase recovery intents, and generic cleanup takeover.

## 2. Product Constraints

The Phase 1 runtime is intentionally limited to:

- one host and one local project workspace;
- foreground CLI execution with no background workflow service;
- a fixed, versioned pipeline compiled into the Go binary;
- manual `run resume` after `BLOCKED`, `NEEDS_REVIEW`, or process restart;
- no remote workers, distributed queue, cross-host checkpoint, web/API server, or arbitrary runtime plugin graph;
- offline operation except for explicitly configured LLM and Similarity calls;
- deterministic Fake adapters in ordinary tests and opt-in real-service smoke tests.

If these constraints change, adopting Temporal is a separate architecture decision rather than an incremental expansion of this SQLite state machine.

## 3. What ADR-0001 Still Means

ADR-0001 remains valuable as a domain-contract decision, not as a mandate to build a workflow runtime. Its required properties are:

1. Adjacent pipeline stages use typed Go inputs and outputs.
2. A stage receives an immutable `RunView` and value/copy inputs.
3. A stage receives only the metered ports it is authorized to use.
4. A stage cannot receive a repository, raw Docker client, unrestricted artifact writer, or mutable run context.
5. Workflow revision, schema version, configuration digest, and downstream invalidation rules are auditable.
6. `map[string]any` and runtime registration cannot bypass type checks.

The application coordinator, not a generic `Step[I,O]` runtime, owns persistence and stage transitions. A concrete typed constructor assembles the Phase 1 pipeline. Later documentation should rename ADR-0001 to “Static Typed CPGen Pipeline and Activity Contracts.”

## 4. Responsibility Boundary

### 4.1 Minimal local coordinator

The coordinator owns only:

- acquiring a local per-run process lock;
- loading the immutable request/config/workflow revision;
- selecting the current fixed stage;
- creating a stage attempt and stable idempotency keys;
- invoking that stage outside SQLite write transactions;
- committing the stage result, budget usage, artifacts, evidence, and next projection atomically where they share one database;
- stopping at `BLOCKED`, `NEEDS_REVIEW`, `READY`, `FAILED`, or `CANCELLED`;
- cleaning or reconciling unfinished Docker work before resuming ordinary stages.

It does not implement a generic scheduler, task queue, lease service, timer service, visibility index, replay engine, or arbitrary DAG.

### 4.2 CPGen domain mechanisms retained

These mechanisms are specific to the product and remain implemented locally:

- Docker sandbox plans, deterministic names/labels, target verdicts, resource evidence, and detached watchdog safety;
- Judge and quality-gate evidence;
- LLM/Similarity/Docker/artifact budget accounting and `CallTrace` provenance;
- immutable Blob storage, verified reads, artifact occurrences, package staging, and publication;
- revisions, downstream invalidation, review decisions, waivers, and package verification;
- provider-specific idempotency and conservative handling of unknown external-call boundaries.

### 4.3 Generic mechanisms deliberately omitted

Phase 1 does not build:

- execution-lease heartbeat or monotone fencing epochs;
- distributed ownership or remote-worker claims;
- generic retry/backoff/timer tables;
- generic event-sourced snapshots or workflow replay;
- observation ticket/floor machinery for `BLOCKED` recovery;
- three-phase workflow recovery intents;
- generic startup janitor or generic cleanup-only workflow mode;
- a workflow visibility/search subsystem;
- a heterogeneous Step registry.

## 5. Runtime Model

### 5.1 Run states

The closed run state is:

```text
CREATED
RUNNING
BLOCKED
NEEDS_REVIEW
READY
FAILED
CANCELLED
```

`READY` remains unreachable until Slice 5 atomically binds the verified package occurrence and final quality report. There are no `PROBING` or `QUIESCING` workflow modes. Dependency probing is an ordinary new attempt of the blocked stage; Docker cleanup is represented by `SandboxExecution` state rather than a general workflow mode.

### 5.2 Fixed stages

The pipeline is statically assembled from versioned typed stages. Phase 1 supplies a small deterministic Fake pipeline that exercises persistence and resume. Slices 2–5 replace or extend the concrete typed constructor with the real Idea, Statement, Similarity, Solution, Data, Judge, Quality, and Package stages.

Every persisted stage record contains:

- run ID, stage name, and ordinal;
- workflow revision and schema version;
- input digest and optional output digest;
- attempt count and current attempt ID;
- state, last typed error, and timestamps;
- stable logical idempotency key;
- references to committed artifact/evidence occurrences.

There is no dynamic stage table that determines arbitrary graph edges. The binary’s typed constructor is authoritative; persisted stage names and workflow revision only select a compatible compiled definition.

### 5.3 Run process lock

Before mutating or executing a run, the CLI acquires an exclusive OS-backed lock on a deterministic per-run lock file under the private runtime directory. The operating system releases the lock when the process exits, including abnormal termination.

The lock provides the single-host property actually required by Phase 1:

- two CLI processes cannot execute the same run concurrently;
- a crashed process cannot continue after the OS has released its lock;
- different runs can execute concurrently;
- read-only `show` and `events` commands do not require the execution lock;
- `cancel` may insert an idempotent control request while another process owns the lock.

The lock path is derived from a validated RunID; it is never user-selected. SQLite state is still protected with expected-version compare-and-swap, but there is no heartbeat, lease expiry, or fencing epoch.

## 6. Persistence Model

### 6.1 Workflow projection tables

The minimal workflow projection uses:

```text
runs
stage_records
stage_attempts
run_events
control_requests
review_decisions
```

`runs` stores the immutable request/config/workflow digests, current state/stage/version, cancellation summary, active-time counters, and final package pointer. `stage_records` stores the latest projection for each compiled stage. `stage_attempts` is append-only audit for retries and restart. `run_events` is an append-only audit stream, not the source from which runtime state must be replayed.

Each state-changing command uses one short SQLite transaction to validate expected run version, update the projection, and append the corresponding event. No external I/O, hashing, fsync, Docker call, network call, or verified Blob read occurs while that transaction is open.

### 6.2 Domain ledger tables

Separate domain tables retain integrity where a simple stage projection is insufficient:

- `budget_accounts` and `call_records`;
- artifact declarations, writer tokens, Blobs, pins, and occurrences;
- cache entries and their source/artifact references;
- mutation claims and provenance records;
- `sandbox_executions` and deterministic resource records;
- packages, package occurrences, verification receipts, and quality reports.

These are product ledgers, not workflow scheduler tables. They use relational constraints to prevent cross-run provenance and budget corruption.

## 7. Stage Execution Protocol

For one stage attempt, the coordinator performs:

1. Acquire the run process lock.
2. Open/migrate SQLite and reconcile unfinished sandbox work for that run.
3. Load and validate the immutable run projection and compiled workflow revision.
4. In a short transaction, create or replay the stage attempt and stable logical idempotency key.
5. Reserve the declared budget and effect records required by the stage.
6. Execute network, Docker, hashing, and filesystem operations outside SQLite write transactions.
7. Verify returned artifacts and external-call evidence.
8. In a short transaction, settle budgets, attach artifact/evidence occurrences, finish the attempt, update the stage/run projection, and append events.
9. Continue to the next compiled stage or stop in a pause/terminal state.

The coordinator releases the process lock when the command exits. A run may remain `RUNNING` after abnormal termination; the next `resume` treats this as an interrupted current-stage attempt and applies the recovery rules below.

## 8. Retry, Blocking, Review, and Cancellation

### 8.1 Retry

Retry is a bounded loop owned by the current domain stage and its policy. It is not a general retry engine. Each physical attempt gets a new ordinal and persisted `call_record`; the logical idempotency key remains stable. Retry stops when the stage succeeds, becomes blocked, requires review, fails permanently, is cancelled, or exhausts its stage budget.

An `UNKNOWN` external boundary cannot be converted into a normal retry with a new key. The adapter must reconcile the original provider/Docker identity when supported, otherwise charge the conservative reservation and return a typed review/blocking result.

### 8.2 BLOCKED resume

`BLOCKED` stores the stage input digest, dependency identity, policy digest, error evidence, and retry-after time. `run resume` acquires the process lock and creates a fresh attempt of that same stage. That attempt probes the dependency through its normal metered port before performing ordinary work.

No observation ticket/floor is needed: the run lock ensures an earlier owner is no longer executing, and only a probe result produced or verified by the new attempt can unblock it. Historical capability cache entries may be diagnostic, but cannot by themselves prove recovery unless the new attempt revalidates them under the current policy.

### 8.3 Review

Review commands create immutable `PENDING` decisions while the workflow command is not executing that run. `run resume` validates and applies exactly one matching decision in a short transaction. `REVISE`, `RETRY`, `WAIVE`, and `REJECT` retain the existing domain semantics and evidence bindings.

### 8.4 Cancellation

`run cancel` inserts one idempotent cancellation request. An active workflow command polls it and cancels its root context. If no workflow command holds the run lock, the cancel command may acquire the lock, reconcile sandbox resources, and commit `CANCELLED` itself.

The run cannot become `CANCELLED` until all untrusted targets are proven stopped. This safety rule belongs to `SandboxExecution`, not to a generic `QUIESCING` workflow mode.

## 9. Crash Recovery

Recovery is local and stage-specific:

- If no external side effect was authorized, rerun the interrupted stage attempt.
- If a call has a stable provider idempotency key, reconcile or replay that key.
- If the send boundary is unknown and cannot be queried, settle conservatively and enter a typed pause/failure path.
- If artifact bytes were published, verify the canonical Blob and attach or release the existing writer token; never publish over conflicting bytes.
- If Docker work was started, use the persisted `SandboxExecution`, deterministic resource identities, and watchdog evidence to stop and reconcile it before rerunning the stage.
- If the prior stage result and all domain ledgers were committed, the projection is already authoritative and the next stage begins normally.

There is no general recovery-intent state machine. Recovery commands are idempotent named operations over the small set of domain ledgers above.

## 10. Docker Safety Boundary

The Slice 0 detached watchdog and its safety properties remain mandatory. Before any Docker Create, the Runner persists the `SandboxExecution`, complete planned resource set, engine identity, deterministic names/labels, and watchdog control digest. It uses the existing pre-create ACK protocol and never exposes Docker to target code.

On owner death, watchdog EOF, or deadline, the watchdog stops/kills the planned resources. On the next CLI startup/resume, a narrow sandbox reconciler scans only exact persisted identities and completes cleanup. It cannot schedule stages, continue an old export, publish artifacts, or mutate unrelated runs.

This reconciler is intentionally not a generic janitor. It has only Inspect/Stop/Kill/Wait/Remove and evidence-settlement capabilities for known sandbox resources.

## 11. Active-Time Budget

If active wall time remains a required budget, store `active_elapsed_ns`, `active_started_at`, and `last_accounting_heartbeat_at` on the run. The execution process updates the accounting heartbeat at a modest interval solely for metering; it does not establish ownership.

Clean pause/finish adds the interval exactly. After a crash, recovery conservatively charges only through `last_accounting_heartbeat_at + one heartbeat interval`, capped by the configured deadline. Paused and offline time after that bound is not charged. There is no lease-based compensation algorithm.

## 12. Artifact and Cache Simplification

Blob publication, verified reads, writer tokens, pins, and occurrences remain because they protect immutable evidence and package integrity. Their APIs stay private and run-scoped.

GC is an explicit maintenance operation, not a concurrent background workflow. Every stateful workflow command holds a shared global artifact-usage lock; GC takes the corresponding exclusive maintenance lock, rechecks SQLite references, moves bytes through a private trash directory, and records the outcome. A normal run therefore never races an uncoordinated background GC, so the workflow does not need general cache-pin dispatch calls or recovery ownership epochs.

Cache hits still preserve source-call and artifact provenance. They are budgeted as logical cache results and verified before the current stage commits them.

## 13. CLI Behavior

The existing CLI remains the only user interface:

```text
cpgen generate --request request.yaml
cpgen run list [--state STATE]
cpgen run show <run-id>
cpgen run events <run-id> [--after-version N]
cpgen run resume <run-id>
cpgen run cancel <run-id> --reason "..."
cpgen review show|revise|retry|waive|reject ...
```

There is no daemon start/stop command and no workflow-service endpoint. Stateful commands open/migrate the local store, acquire required locks, run narrow sandbox reconciliation, execute the command, and exit.

## 14. Testing Strategy

Slice 1 must prove the reduced design rather than the discarded general engine:

- transition tables for the seven Run states and fixed stage states;
- typed pipeline compile-time tests and source-boundary tests;
- two-process tests proving the OS run lock admits one executor and automatically releases on process death;
- expected-version SQLite conflict and atomic projection+event tests;
- stage restart at every durable boundary;
- bounded retry, stable idempotency key, `UNKNOWN`, budget, and cancellation tests;
- review decision lifecycle and resume tests;
- Blob traversal/corruption/dedup/crash tests;
- real Docker kill tests while target runs, after target stops before export, and during cleanup;
- watchdog death/EOF and narrow startup reconciliation tests;
- full `go test`, `go vet`, `go test -race`, Go 1.25.0 compatibility (after the 2026-09-08 library amendment), and Linux cross-build gates.

Tests for lease expiry, fencing epoch, observation floors, generic recovery intents, dynamic DAG scheduling, or workflow visibility are explicitly out of scope.

## 15. Future Temporal Migration Seam

The design avoids a premature Temporal dependency while preserving a clean migration seam:

- typed stage inputs/outputs are serializable versioned domain values;
- external side effects are isolated behind idempotent application services;
- stage logic does not depend on SQLite or process-lock APIs;
- run/stage projections are read models, not control-flow APIs exposed to agents;
- Docker and artifact ledgers remain valid even if a future Temporal Activity invokes them.

If the product later requires remote workers, many concurrent long-lived runs, automated timers while no CLI is active, cross-host recovery, or operational workflow search, create a new ADR and POC. Temporal would then own durable Workflow/Activity execution, retry/timers, signals/updates, worker tasks, history, and visibility. CPGen would still own Docker safety, Judge, budgets, artifacts, revisions, and package integrity.

## 16. Required Documentation Changes

After this written design is approved, the implementation plan must first update the existing architecture contract:

1. Rewrite ADR-0001 as typed CPGen pipeline/activity contracts.
2. Simplify ADR-0002 by removing execution modes, leases, probing tickets, and recovery intents.
3. Revise `ARCHITECTURE.md`, storage/workflow/testing/CLI designs, the Phase 1 MVP spec, implementation plan, README, and TODO to match this decision.
4. Replace the discarded Slice 1 plan with a smaller plan covering local run/stage persistence, process locking, domain ledgers, sandbox reconciliation, Fake pipeline/CLI, and crash tests.
5. Preserve all completed Slice 0 commits and evidence.

No Slice 1 implementation begins until these documents agree on the lightweight boundary.
