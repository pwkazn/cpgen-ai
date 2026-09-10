# Phase 1 MVP Implementation Design

Status: Accepted

Date: 2026-08-31

## 1. Goal and authoritative sources

Phase 1 builds an auditable local CPGen path from GenerationRequest to a verified package. The runtime contract is one foreground executor per run, a per-run process lock, a fixed pipeline, and no workflow-hosting service.

Authoritative sources are ADR-0006, the lightweight local workflow design, ARCHITECTURE.md, current ADRs, and the detailed designs. The completed Slice 0 evidence remains valid and is not rewritten by Slice 1.

## 2. Scope

Scope amendment from the user, 2026-09-09: prioritize Similarity ACCEPT → Solution → Data → Docker/Judge → Quality → Package, and non-accepted business results → human review. Automatic mutation and business repair are deferred for redesign after a usable loop. Existing mutation-ledger/provenance descriptions remain historical contracts, not MVP delivery prerequisites. Follow the [current execution plan](../plans/2026-09-09-mvp-generation-loop.md).

The MVP includes:

- strict request, configuration, and domain values;
- typed Idea, Statement, Similarity, Solution, Data, Judge, Quality, and Package stages;
- foreground CLI execution on one host;
- SQLite current projections and append-only audit events;
- CPGen ledgers for budgets, calls, artifacts, cache, sandbox, review, and packages;
- direct Docker target execution with detached watchdog;
- provider-neutral model and similarity adapters;
- immutable Blob storage and run-scoped occurrences;
- human review, bounded stage retry, manual resume, and cancellation;
- deterministic package gates and atomic READY binding.

Not in scope are remote workers, a daemon, a task queue, arbitrary runtime graphs, cross-host checkpointing, web/API control, or unattended timers.

## 3. Component boundary

### CLI

Validates local configuration and requests, invokes application services, renders stable output, and maps typed outcomes to exit codes.

### Local coordinator

Acquires the OS-backed run lock, selects the current compiled stage, records attempts and reservations, performs external work outside write transactions, commits verified outcomes, and exits at a pause or terminal state.

### Typed stages

Receive immutable RunView, copied typed input, and minimum metered ports. They never receive persistence, locks, raw Docker, unrestricted artifact writing, or mutable run state.

### Adapters

SQLite, filesystem, model, similarity, Docker, clock, and ID adapters implement narrow ports. Deterministic Fakes are the default test implementations.

### Docker safety boundary

The Runner persists SandboxExecution and its complete resource plan before Docker create. Deterministic identity and the detached watchdog ensure targets stop after CLI loss. Later commands reconcile only exact persisted resources.

## 4. Data flow

1. Parse and strictly validate GenerationRequest and ApplicationConfig.
2. Canonicalize redacted configuration and request digests.
3. Create run, compiled stage projections, and first event atomically.
4. Acquire the run lock and select the current stage.
5. Create or replay a stage attempt and stable logical idempotency key.
6. Reserve budgets and effect records.
7. Invoke the typed stage and external adapters outside database write transactions.
8. Validate responses, Judge evidence, and artifact bytes.
9. Settle CallTrace, budgets, occurrences, and stage result atomically where they share SQLite.
10. Continue the fixed stage sequence or exit at BLOCKED, NEEDS_REVIEW, READY, FAILED, or CANCELLED.
11. At Package, verify the staged tree and atomically bind the same-run verified occurrence and final quality report before READY.

## 5. State and restart

Run states are CREATED, RUNNING, BLOCKED, NEEDS_REVIEW, READY, FAILED, and CANCELLED.

A stage attempt is append-only audit. After process death, the OS releases the lock. Manual resume reconciles the current stage from its persisted ledgers:

- rerun when no irreversible effect was authorized;
- query or replay the same provider idempotency identity;
- settle an unknown send boundary conservatively;
- verify published Blob bytes and writer tokens;
- stop and clean exact Docker resources;
- advance normally if the completed result already committed.

BLOCKED resume creates a fresh same-stage attempt and first revalidates the checkpoint dependency through its normal metered port and current policy. Historical health data alone cannot resume work.

Cancel is an idempotent control request. CANCELLED cannot commit until every untrusted target is proven stopped.

## 6. Persistence

Projection tables are runs, stage_records, stage_attempts, run_events, control_requests, and review_decisions.

Domain ledgers include:

- budget_accounts, logical operations, call records, and reservations;
- declarations, writer tokens, Blobs, pins, and occurrences;
- cache entries, source calls, Blob references, and current-run uses;
- mutation and provenance records;
- sandbox executions, resources, and evidence;
- packages, package occurrences, verification receipts, and quality reports.

Every state-changing operation checks expected version and uses a short transaction. Events are audit, not a control-flow replay source.

## 7. Slice delivery

### Slice 0: execution foundation — completed

Delivered strict domain values, Judge foundation, direct Docker execution, host capability results, CallTrace and budget evidence, deterministic resource identity, detached watchdog, and verification evidence.

### Slice 1: lightweight local core

Deliver architecture reconciliation, lifecycle values, cross-platform run locks, SQLite projections, call and budget ledgers, Blob and occurrence storage, cache and explicit maintenance, Docker identity persistence and narrow reconciliation, fixed typed Fake pipeline, CLI commands, and crash tests.

### Slice 2: idea, statement, model, similarity

Deliver request and content contracts, prompt registry, strict structured output, provider adapters, similarity evidence and policy, cache, privacy, and review routing.

### Slice 3: solution and Judge

Deliver solution generation, compile/run integration, checker and SPJ support, target measurements, and persisted Judge evidence.

### Slice 4: data and quality

Deliver data generation, validation, expected outputs, differential checks, mutation evidence, resource limits, and final quality reports.

### Slice 5: package and E2E

Deliver canonical internal packages, structural and semantic gates, verification receipts, atomic READY binding, export/import verification, and complete E2E evidence.

## 8. Error handling

Typed errors classify invalid input, unavailable dependency, incompatible host, retryable transport, unknown send status, budget exhaustion, review requirement, cancellation, permanent stage failure, and internal corruption.

Retry is bounded within a foreground stage. Backoff does not create an unattended timer. Unknown boundaries retain the original identity or pause conservatively. Artifact and Docker cleanup remain idempotent after cancellation.

## 9. Testing

Required proof includes:

- transition tables and compiled typed boundaries;
- two-process same-run exclusion and process-death release;
- atomic projection plus event and expected-version conflict;
- crash at every durable stage boundary;
- stable logical identity and physical CallTrace;
- budget concurrency and conservative settlement;
- Blob security, corruption, deduplication, and GC exclusion;
- cache source and current-run provenance;
- live Docker kill, watchdog EOF, and exact-resource reconciliation;
- review and cancellation lifecycle;
- package gate completeness and same-run READY;
- full Go tests, vet, race tests, Go 1.25.0 compatibility under the 2026-09-08 ADR-0006 amendment, Linux cross-build, and architecture consistency.

## 10. Acceptance

Phase 1 is complete only when a clean workspace can generate or deterministically simulate a request through the compiled pipeline, survive injected process death, resume the current stage without duplicate effects, stop untrusted targets safely, produce a verified package, and reproduce the result from persisted evidence.
