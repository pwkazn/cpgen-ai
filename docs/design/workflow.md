# Fixed Workflow, Revisions, and Budgets

Status: Current under ADR-0006

## 1. Scope

This design defines the concrete typed CPGen pipeline, durable stage boundaries, stage-local retry, manual resume, review, cancellation, and downstream invalidation. The coordinator is a foreground CLI component for one host.

The forward MVP continues from committed Similarity ACCEPT through Solution/Data/Docker/Judge/Quality/Package. The 2026-09-15 scope adds bounded content regeneration in `mvp.idea.statement.similarity.solution.data.judge.package.v2`; V1 and historical checkpoints retain their existing stopping behavior. Transport retry, JSON-format repair, dependency recovery and explicit human review remain separate. General idea mutation remains deferred.

## 2. Identities and revisions

A run persists:

- RunID and validated request identity;
- WorkflowRevision and SchemaVersion;
- canonical request and configuration digests;
- current run version, state, stage name, and stage ordinal;
- active-time accounting and final package pointer;
- timestamps from the injected canonical clock.

Every stage record persists its typed input digest, optional output digest, attempt count, current attempt ID, state, last typed error, logical idempotency key, and committed evidence references.

The compiled constructor is authoritative. Persisted names and ordinals are compatibility selectors and audit fields, not a user-defined graph. Immutable `workflow.Definition` is the single source for fixed stage order, package completion and attempt preservation. `NewGenerationRunService` uses the flat `GenerationRunConfig` with explicit owner resources; historical Go constructor helpers exist only in tests. Resource/admission/frozen-policy checks run at composition, while run/attempt/evidence checks remain at execution boundaries.

## 3. Concrete typed pipeline

The generation constructor assembles these business stages:

1. Idea
2. Statement
3. Similarity
4. Solution
5. Data
6. Judge
7. Quality
8. Package

The current `GenerationRevision` assembles the complete ordinary-problem pipeline, including separate verification and decision boundaries. `revisions.go` isolates the unchanged persisted revision strings and historical stopping points. All revisions use the application scheduler; there is no separate historical pipeline executor. The default deterministic Fake pipeline and its capability configuration live in `internal/adapter/fake`.

The local fixed loop in `internal/application` runs one stage boundary at a time. It validates identity, stage order and committed version before selecting the next stage, checks cancellation before invocation and returns on a pause or terminal result. For V2 it also accepts the explicit compiled content-retry routes; the durable boundary owns eligibility and the persisted retry limit. The scheduler performs no independent checkpoint writes. A failed boundary may return an updated same-stage projection when BeginStage or accounting already committed; foreign or regressed projections are rejected.

Progress is reconstructed from the compatible compiled workflow revision, stage/input/config/schema bindings and verified stored outputs. SQLite remains authoritative; no graph.json store or unchecked automatic checkpoint callback is used. The scheduler shares no invocation state between runs. `LocalRunService` directly manages attempts, result commits, review and recovery under the run lock; its stageControl helper joins pollers. `fixedStages` adapts typed business inputs/results and selects recovery through an explicit switch. Its recovery switch performs stage admission and verifies cleanup proof, then delegates retained physical-call settlement to `internal/adapter/sandbox`. Model and Similarity retry/receipt/cache protocols live in `internal/execution`; they do not import application or advance workflow stages. There is no recovery registration table or separate lifecycle/termination object. Bootstrap/Application owns and closes execution resources before storage. The existing stage-sequence, error, cancellation, concurrent-run and subprocess recovery tests remain the behavioral contract after removing the graph library.

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

V2 automatically regenerates content at most **twice per run**, shared across stages and process restarts. The example configuration selects V2; existing frozen V1 runs remain V1. This initial policy regenerates from the original typed input; it does not yet feed compiler diagnostics or prior drafts back into the prompt.

| Failure | Regenerate from |
| --- | --- |
| No feasible ideas; draft/input binding rejected | Current draft stage |
| Verified JSON-format rejection after the configured format allowance | Current draft stage |
| Solution or brute compile failure | Solution |
| Solution sample failure (the sample itself may be wrong) | Statement, then Similarity and all later stages |
| Generator/validator compile, execution, validation or reproducibility failure | Data |
| Judge reference/brute failure or differential mismatch | Solution, then Data and all later stages |

Similarity decisions, fixed-checker Quality failures, provider HTTP rejection, unknown send boundaries, unsupported diagnostics and exhausted budgets do not authorize content regeneration. They retain the existing review/block/error behavior.

`FinishContentRetry` completes the failed attempt, inserts immutable `content_retries` evidence and invalidates the target's entire downstream suffix in the same SQLite transaction. It preserves attempt ordinals, earlier artifacts, request/configuration bindings and all budget accounts. A replay cannot spend another allowance. Regenerated draft attempts bypass cache lookup; already dispatched calls retain normal durable replay. Every downstream verification must pass again before READY. When the two allowances are spent, the same transaction finishes at the ordinary non-waivable NEEDS_REVIEW gate. Pending cancellation prevents regeneration.

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

Stage publication takes the existing admitted attempt and version and artifact declaration plus bytes; the publication adapter owns physical call, reservation and writer identities. Committed proof readers use read-only response policy and sandbox planning inputs without transports or lifecycle resources. No database revision, schema, canonical encoding or audit identity changes accompany these capability boundaries.

Stage code cannot mutate counters. Parallel-safe account updates use database constraints and expected account versions. Cache results still create logical call evidence and retain the source call and artifact provenance.

Adding an ordinary business stage requires its typed implementation, committed evidence reader, a new compatible workflow definition and fixed execution/commit/recovery adapters. Existing persisted definitions must retain their sequences. Timer, cancellation, budget, writer and generic terminal-cleanup state machines do not gain business-stage branches; only a new effect protocol would require a separately reviewed adapter change.

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
