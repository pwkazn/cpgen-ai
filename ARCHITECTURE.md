# CP Problem Generator AI Architecture

Status: Current

## 1. Product boundary

CPGen turns a structured competitive-programming request into an auditable problem package. Generative models propose candidates; deterministic code, compilation, execution, judging, similarity policy, and package gates decide whether those candidates are acceptable.

Current MVP scope (user revision, 2026-09-09): a valid Similarity ACCEPT continues through Solution, Data, Docker/Judge, Quality and Package; non-accepted business results enter human review. Automatic Idea mutation and business repair are deferred for redesign after the usable forward loop. Existing transport recovery, bounded JSON-format repair and deterministic package/quality gates remain required. The [current delivery plan](docs/superpowers/plans/2026-09-09-mvp-generation-loop.md) supersedes mutation-first ordering in older documents.

Phase 1 is a modular local application:

- one host and one private project workspace;
- one foreground executor per run;
- a deterministic per-run process lock;
- a fixed pipeline compiled into the Go binary;
- SQLite for current projections and audit records;
- private content-addressed artifact storage;
- direct Docker execution guarded by the detached watchdog;
- no workflow-hosting service, daemon, task queue, remote worker, or arbitrary runtime graph.

Different runs may execute concurrently in separate CLI processes. A single run never has two mutating executors.

## 2. Architecture principles

1. Structured, versioned domain models are the source of truth.
2. Typed stage inputs and outputs are checked at compile time.
3. RunView is immutable and stage code receives only minimum metered ports.
4. External I/O never occurs while a SQLite write transaction is open.
5. Every external effect has a stable logical identity, a physical CallTrace, and conservative budget settlement.
6. Blobs are immutable; occurrences bind bytes to run, revision, role, and producer evidence.
7. Untrusted programs run only through the Docker runner and watchdog safety boundary.
8. Human review is an immutable decision record, not an ad-hoc state mutation.
9. READY means the same run atomically references a verified package occurrence and final quality report.
10. The local coordinator remains product-specific and small.

## 3. System context

The user interacts through the cpgen CLI. Read commands use local storage assembly without stage executors, provider transports or Docker preflight. Package export acquires the shared run lock and then the shared artifact lock, reconstructs committed proof using the run's frozen settings, and verifies the archive. New runs retain a verified toolchain lock snapshot in their frozen configuration. Older runs without a snapshot still require the original local lock file and its matching digest.

Execution commands load configuration, open and migrate SQLite, acquire the run-specific execution lock and shared artifact-usage lock, reconcile unfinished sandbox resources when required, perform one command, and exit. Explicit artifact maintenance acquires the exclusive global artifact lock, takes no per-run lock, and never executes a run stage. It is currently an application API, not an exposed CLI subcommand.

External dependencies are limited to explicitly configured model and similarity providers plus the local Docker Engine. Adapters normalize provider results into typed domain outcomes. Ordinary tests use deterministic Fake adapters; real-service smoke tests are opt-in.

## 4. Components

### 4.1 CLI

The CLI validates configuration and requests, invokes application services, streams stable progress events, renders human or JSON output, and maps typed errors to stable exit codes. It never edits database rows directly.

### 4.2 Application coordinator

The application uses a fixed loop, one run coordinator and explicit business stages:

- Bootstrap/Application constructs storage, providers and Docker, and owns resource closure. Read commands use BootstrapLocal without execution resources.
- LocalRunService owns run locks, attempts, start/finish, cancellation, review and recovery. Its stageControl helper owns joined accounting/cancellation pollers. Runtime state is not copied into separate lifecycle, termination or recovery objects.
- `fixedStages` contains typed input/result dispatch and the explicit recovery switch. Business executors use their own Reader; they do not contain upstream executors. Each Reader's methods and proof verification live together.
- Read-only model and sandbox policies remain separate from transports. Artifact publication takes the existing admitted attempt and run version, then uses the unchanged ledger internally.

The package boundary follows ownership of work:

- `internal/application` owns business stages, run coordination and composition. Stage reports live with their validators; draft configuration and stage execution are not split into one-use files.
- `internal/execution` owns metered LLM/Similarity calls, bounded retry, receipts, cache reuse and the run-bound call ledger. It has no application import or workflow progression. Its store contract reads the current run and attempt but cannot create or finish a workflow stage; cache writes are supplied separately where needed.
- `internal/adapter/sandbox` owns Docker sessions, artifact publication and settlement of retained sandbox calls. Application admission and cleanup-proof checks precede that settlement. It depends on the durable call protocol, never on stage executor internals.
- `internal/artifact` owns prepared writer sessions, bounded verified reads and explicit garbage collection. Callers use it directly; application contains no compatibility alias or forwarding constructor.

`GenerationRunConfig` supplies owner resources once. Composition validates shared storage, admission, clock, locks and frozen policy before directly assembling stage structs. Historical constructor helpers exist only in tests; immutable `workflow.Definition` preserves persisted revisions and stage sequences without upgrading old runs.

Committed readers continue checking run/attempt provenance, publication state and Blob digests. Package completion uses the separate `FinalizeVerifiedPackage` transaction; READY cannot precede the verified occurrence. RunView, BudgetSnapshot and CallTrace stay derived views. The [rework record](docs/evidence/architecture-follow-up-2026-09-14.md) documents removed abstractions and validation.

It does not implement generic scheduling, replay, timers, or distributed ownership.

Under the 2026-09-13 ADR-0006 amendment, a local fixed loop in `internal/application` replaces the LangGraphGo wrapper. SQLite remains authoritative: the loop must check the stage/evidence commit before advancing, and recovery reads the existing projection and verified occurrences. Automatic graph checkpoints and a parallel graph.json store are excluded. LangChainGo stays in `internal/agent`; provider library types never enter domain, port or stage code. Explicit configuration selects the MVP; the default Fake and historical preview revisions retain their existing boundaries.

### 4.3 Typed stages

The concrete constructor assembles versioned Idea, Statement, Similarity, Solution, Data, Judge, Quality, and Package stages. Slice 1 supplies a deterministic Fake pipeline with the same contracts.

Each stage receives a copied input value, immutable RunView, canonical clock values, and only its authorized ports. It cannot receive a repository, raw Docker client, mutable run state, unrestricted artifact writer, or lock manager.

### 4.4 Persistence and artifact storage

SQLite stores short transactional projections and CPGen-specific ledgers. The private artifact store writes immutable SHA-256 addressed Blobs through declaration, writer-token, verification, pin, and occurrence protocols. Package staging uses private directories and atomic publication.

### 4.5 Docker runner and watchdog

The trusted host runner persists SandboxExecution and its complete planned resources before Docker create. Deterministic names, labels, plan digest, engine identity, and watchdog control evidence authorize only exact resources.

The detached watchdog receives the sealed plan before start and stops planned targets on deadline or process loss. A later command runs a narrow reconciler that can inspect, stop, kill, wait, remove, and settle only those persisted resources. It cannot continue stage work or publish artifacts.

## 5. Domain contracts

Key immutable values include:

- RunID, StageName, AttemptID, LogicalOperationID, CallID, SandboxExecutionID;
- WorkflowRevision, SchemaVersion, ConfigDigest, InputDigest, OutputDigest;
- RunView and typed stage input/output values;
- BudgetAccount, reservation, MeteredOutcome, and CallTrace;
- BlobRef, ArtifactDeclaration, WriterToken, ArtifactOccurrence, and provenance;
- SandboxPlan, resource identity, watchdog evidence, ProcessOutcome, and Judge verdict;
- ReviewDecision, waiver binding, package occurrence, verification receipt, and quality report.

All identifiers are validated before they are used in paths, labels, or queries. Canonical encodings and digests are versioned.

## 6. Fixed workflow

The authoritative stage order and attempt recovery policy are compiled in `internal/workflow/definition.go`. Definition accessors copy stage slices; callers cannot mutate the order. Persisted stage name, ordinal, workflow revision, and schema version select a compatible binary definition; database rows do not define graph edges.

The closed run state set is:

- CREATED
- RUNNING
- BLOCKED
- NEEDS_REVIEW
- READY
- FAILED
- CANCELLED

A stage uses PENDING, RUNNING, SUCCEEDED, BLOCKED, NEEDS_REVIEW, FAILED, or CANCELLED. Its append-only attempt ends as SUCCEEDED, BLOCKED, NEEDS_REVIEW, FAILED, CANCELLED, or INTERRUPTED.

The MVP reaches READY only through the verified Package transaction. Historical preview and checkpoint revisions do not produce READY.

## 7. Per-run locking and transaction protocol

The lock path is derived from validated RunID beneath the private runtime directory. The OS releases the lock when a CLI process exits, including abnormal termination. Read-only show and events commands do not need it. Cancel may insert an idempotent control request while another process owns it.

SQLite still uses expected run-version comparisons. For one stage attempt:

1. acquire the run lock;
2. open and migrate SQLite;
3. reconcile unfinished sandbox work for the run;
4. load immutable request, configuration, projection, and compiled revision;
5. in a short transaction, create or replay the stage attempt and reservations;
6. perform network, Docker, hashing, filesystem, and verified Blob reads outside write transactions;
7. validate returned evidence;
8. in a short transaction, settle budgets and effects, attach occurrences, finish the attempt, update projections, and append events;
9. continue to the next compiled stage or exit.

No hashing, fsync, rename, provider call, Docker call, watchdog IPC, or blocking wait occurs inside a write transaction.

## 8. Persistence model

Workflow projection tables are:

- runs
- stage_records
- stage_attempts
- run_events
- control_requests
- review_decisions

CPGen domain ledgers are separate:

- budget_accounts and call_records;
- artifact declarations, writer tokens, Blobs, pins, and occurrences;
- cache entries with source-call and artifact references;
- mutation claims and provenance;
- sandbox_executions and sandbox_resources;
- packages, package_occurrences, verification receipts, and quality reports.

Run events provide append-only audit but are not replayed to reconstruct control flow. Relational constraints prevent cross-run provenance, budget, artifact, and package corruption.

## 9. Retry, pause, review, and cancellation

Retry is bounded inside the active domain stage. Each physical attempt has a new ordinal and call record; the logical idempotency key remains stable. An unknown send boundary must reconcile the original provider or Docker identity when possible. Otherwise the system charges conservatively and returns a typed pause or failure.

BLOCKED stores stage input digest, dependency identity, policy digest, error evidence, and retry-after time. Manual resume starts a fresh attempt of the same stage. That attempt revalidates the dependency through its ordinary metered port before doing normal work. Historical health data is diagnostic only.

Review commands create one PENDING decision. Manual resume validates and applies a matching REVISE, RETRY, WAIVE, or REJECT decision in a short transaction.

Cancel inserts one idempotent control request. The active command polls it, cancels the root context, stops new authorization, and settles in-flight effects. CANCELLED is committed only after every untrusted target is proven stopped.

## 10. Restart and crash recovery

A process may exit while the run projection and current attempt still say RUNNING. The next manual resume obtains the released OS lock and follows stage-specific rules:

- rerun when no external effect was authorized;
- replay or reconcile the same stable provider identity;
- settle unknown boundaries conservatively;
- verify any already-published Blob before attaching or releasing its writer token;
- use persisted SandboxExecution and exact resource identities to clean Docker work;
- begin the next stage normally if result and ledgers already committed.

Recovery consists of idempotent named operations over domain ledgers. It never scans unrelated runs or invents a generic workflow action.

For the first `solution_verify` create failing before any SandboxExecution exists, manual resume may interrupt the old attempt and begin an ordinary new verification attempt. SQLite checks this in the interruption transaction: all calls are terminal sandbox/local-publication calls, every Docker physical call is `ABORTED_NO_DISPATCH` without a dispatch-start timestamp, and reservations are settled. Existing execution records (including CLEANED), sent or unknown calls, and unsettled evidence retain the original recovery path. Original call identities, evidence and consumed budgets remain intact; the new attempt uses the unchanged committed solution input and normal admission, resource authorization and READY gates. See the [recovery evidence](docs/evidence/sandbox-unsent-recovery-2026-09-15.md).

## 11. Budget and provenance

Budget dimensions include model calls, tokens and cost, similarity calls, sandbox runs, artifact bytes, stage attempts, and optional active wall time. Reservation precedes irreversible work; final settlement is monotone and auditable.

Active-time accounting may persist active_elapsed_ns, active_started_at, and last_accounting_heartbeat_at. That heartbeat is metering only. After a crash, the charge is conservatively bounded by the last accounting timestamp plus one configured interval and the run deadline.

Every physical external call records provider, request digest, policy, timing, result classification, idempotency identity, and CallTrace. Cache hits preserve source-call and artifact provenance and are budgeted according to their logical effect.

## 12. Artifacts, cache, and packages

Artifacts are declared before writing. Bytes are streamed to a private temporary file while hashing and size limits are enforced, then atomically promoted to their canonical Blob identity. Verified reads rehash before use at high-trust boundaries.

An occurrence links a finalized writer token and Blob to its producing run, stage attempt, role, revision, and source call. Cache hits create new run-scoped provenance instead of returning a naked Blob reference.

Garbage collection is an explicit maintenance command. Stateful workflow commands hold a shared artifact lock; collection takes the exclusive form, rechecks database references, and moves bytes through private trash.

Package creation stages only declared files. Structural and semantic gates run before publication. READY and the verified package occurrence are committed in one transaction.

## 13. Security

- private runtime, database, lock, artifact, and staging paths;
- strict path normalization with no user-selected lock path;
- no secrets in configuration snapshots, logs, artifacts, Docker labels, or packages;
- provider allowlists, HTTPS, redirects disabled by default, response caps, and timeouts;
- Docker profiles with non-root identity, dropped capabilities, read-only root, explicit mounts, pids/memory/CPU limits, and no socket exposure;
- exact resource identity checks before cleanup;
- deterministic errors that do not leak sensitive content.

## 14. Testing and release gates

Slice 1 proves:

- all run-state transitions and fixed-stage transitions;
- compile-time typed boundaries and copied RunView values;
- two-process lock exclusion and automatic release after process death;
- expected-version conflicts and atomic projection plus event commits;
- restart at every durable stage boundary;
- bounded retry, stable identities, unknown-boundary handling, budgets, review, and cancellation;
- Blob traversal, corruption, deduplication, and crash behavior;
- watchdog death, deadline, and exact-resource reconciliation;
- no external I/O during SQLite write transactions.

Release gates include full Go tests, vet, race tests, Go 1.25.0 compatibility, Linux cross-build, Docker-required safety tests where available, and the architecture consistency script. go.mod pins LangChainGo v0.1.14; fixed scheduling has no graph-library dependency. CI tests the minimum Go version and the current stable toolchain.

## 15. Delivery slices

- Slice 0: completed execution probe, direct Docker runner, watchdog, Judge foundation, and evidence.
- Slice 1 lightweight local workflow: local lifecycle values, per-run lock, SQLite projections, domain ledgers, Blob storage, Fake pipeline, CLI, and crash tests.
- Slice 2: request, idea, statement, model integration, and similarity.
- Slice 3: solution generation and Docker Judge integration.
- Slice 4: data generation, differential validation, and quality gates.
- Slice 5: package assembly, export, verification, and end-to-end acceptance.

The architecture deliberately keeps the Slice 0 safety evidence intact while making Slice 1 proportional to the single-host foreground product.
