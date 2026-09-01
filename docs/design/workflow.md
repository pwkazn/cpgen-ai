# Fixed Workflow, Revisions, and Budgets

Status: Current under ADR-0006

## 1. Scope

This design defines the concrete typed CPGen pipeline, durable stage boundaries, stage-local retry, manual resume, review, cancellation, and downstream invalidation. The coordinator is a foreground CLI component for one host.

## 2. Identities and revisions

A run persists:

- RunID and validated request identity;
- WorkflowRevision and SchemaVersion;
- canonical request and configuration digests;
- current run version, state, stage name, and stage ordinal;
- active-time accounting and final package pointer;
- timestamps from the injected canonical clock.

Every stage record persists its typed input digest, optional output digest, attempt count, current attempt ID, state, last typed error, logical idempotency key, and committed evidence references.

The compiled constructor is authoritative. Persisted names and ordinals are compatibility selectors and audit fields, not a user-defined graph.

## 3. Concrete typed pipeline

The Phase 1 constructor assembles these versioned stages:

1. Idea
2. Statement
3. Similarity
4. Solution
5. Data
6. Judge
7. Quality
8. Package

Slice 1 uses a deterministic Fake constructor with a small subset of stages to prove persistence and restart. Later slices replace or extend that constructor with real stage implementations.

A stage is parameterized by concrete Go input and output values. It receives an immutable RunView, a copied input, and a narrow set of metered ports. It never receives persistence, the process lock, raw Docker, unrestricted Blob writing, or mutable coordinator state.

## 4. State model

Run states are CREATED, RUNNING, BLOCKED, NEEDS_REVIEW, READY, FAILED, and CANCELLED.

Stage states are PENDING, RUNNING, SUCCEEDED, BLOCKED, NEEDS_REVIEW, FAILED, and CANCELLED.

Stage-attempt states are RUNNING, SUCCEEDED, BLOCKED, NEEDS_REVIEW, FAILED, CANCELLED, and INTERRUPTED.

READY cannot be committed until the verified package occurrence and final quality report for the same run are bound atomically.

## 5. Coordinator protocol

A stateful execution command:

1. acquires the deterministic per-run process lock;
2. opens and migrates SQLite;
3. reconciles unfinished exact sandbox resources for the run;
4. validates request, configuration, workflow revision, and current projection;
5. starts or replays the current stage attempt in a short transaction;
6. reserves required budgets and effect records;
7. invokes the stage outside all SQLite write transactions;
8. verifies returned evidence and artifacts;
9. settles ledgers, finishes the attempt, updates stage and run projection, and appends ordered events in a short transaction;
10. advances to the next compiled stage or exits at a pause or terminal state.

Expected run version is checked on every transition. Replaying a committed transition returns its stored result and never duplicates events or accounting.

## 6. Retry

Retry is a bounded loop within the current stage and its versioned policy. Each physical attempt has a new ordinal and persisted call record. The logical operation identity and idempotency key remain stable across attempts.

Retry stops on success, a blocking condition, human review, permanent failure, user cancellation, or exhausted stage budget. A request with an unknown external send boundary is never resent under a new key. The adapter reconciles the original identity where supported; otherwise it settles reservations conservatively and returns a typed pause or failure.

Backoff is applied only while the foreground command is running. There is no background timer. A future retry time is persisted as part of a blocked checkpoint and requires manual resume.

## 7. Blocking and manual resume

A BLOCKED checkpoint contains:

- stage name and typed input digest;
- dependency identity and policy digest;
- canonical error evidence;
- retry-after timestamp;
- relevant budget and revision bindings.

Manual resume reacquires the run lock and creates a fresh attempt of the same stage. Its first authorized dependency operation uses the ordinary metered port and current policy to revalidate the exact checkpoint dependency. Cached historical health is diagnostic only and cannot establish recovery by itself.

If the dependency remains unavailable, the new attempt ends BLOCKED and updates evidence. If it is healthy, the same attempt may continue ordinary stage work. No separate run mode or special scheduler path is introduced.

## 8. Process restart

An abnormal exit may leave run, stage, and current attempt recorded as RUNNING. The OS releases the run lock. On manual resume, the coordinator first reconciles the attempt:

- if no irreversible effect was authorized, mark it INTERRUPTED and rerun;
- if a provider supports the stable idempotency key, query or replay that identity;
- if dispatch status is unknown and cannot be queried, settle conservatively and pause or fail;
- if Blob bytes were published, verify them and attach or release the writer token;
- if Docker work started, settle the persisted SandboxExecution before rerunning;
- if the completed result and ledgers already committed, advance from the authoritative projection.

Only the current stage is considered. Recovery never chooses an arbitrary graph node or changes unrelated runs.

## 9. Review

ReviewDecision kinds are REVISE, RETRY, WAIVE, and REJECT. States are PENDING, APPLIED, REJECTED, and STALE.

Review commands insert an immutable PENDING decision with run version, workflow revision, stage input, evidence, policy, requested edits, and waiver scope. Manual resume validates exactly one decision:

- REVISE creates a new revision, invalidates downstream outputs, and restarts at the earliest affected compiled stage;
- RETRY creates a fresh attempt of the reviewed stage;
- WAIVE records bounded policy evidence and continues only where the policy permits;
- REJECT ends the run as FAILED with review evidence.

A stale or ambiguous decision is never applied.

## 10. Cancellation

The cancel command inserts one idempotent control request even when another process owns the run lock. The foreground coordinator polls that table and cancels its root context.

After cancellation is observed:

- no new call, artifact writer, or sandbox start may be authorized;
- already-authorized operations are settled;
- each untrusted target is stopped and proven stopped;
- cleanup evidence and budget outcomes are recorded;
- pending review decisions become stale;
- the run commits CANCELLED in one short transaction.

If no executor is active, cancel may acquire the run lock and perform the same exact-resource reconciliation before committing.

## 11. Downstream invalidation

Every stage declares the input components and prior-stage outputs that form its input digest. A revision change computes the earliest affected stage from the compiled definition. Current outputs at that stage and later stages become non-current, but immutable prior attempts, events, Blobs, occurrences, call traces, and review decisions remain auditable.

An unchanged digest may reuse a verified committed stage output only when schema, workflow revision compatibility, policy, provenance, and budget rules permit it.

## 12. Budgets

The coordinator exposes each stage a RunView containing read-only remaining limits. Metered ports own reservations and settlement for model calls, similarity calls, Docker runs, artifact bytes, tokens, cost, and active time.

Stage code cannot mutate counters. Parallel-safe account updates use database constraints and expected account versions. Cache results still create logical call evidence and retain the source call and artifact provenance.

## 13. Acceptance

Tests must prove:

- only the compiled stage order can execute;
- RunView and inputs cannot be mutated through aliases;
- the run lock excludes a second executor for the same run;
- every durable boundary can restart without duplicate effects;
- retry keeps logical identity and gets new physical records;
- blocked resume performs a current-policy dependency check;
- review and cancellation follow the rules above;
- external I/O never overlaps a SQLite write transaction;
- READY is impossible before same-run package verification.
