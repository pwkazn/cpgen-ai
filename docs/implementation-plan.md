# MVP Implementation Plan

Status: Current under ADR-0006

## 1. Delivery contract

Phase 1 follows one foreground executor per run, a per-run process lock, a fixed pipeline, and no workflow-hosting service. SQLite stores current run and stage projections plus CPGen domain ledgers. All external I/O occurs outside write transactions.

Each slice must preserve completed evidence from earlier slices, use typed contracts and deterministic tests, pass repository gates, and end with a reviewable checkpoint.

## 2. Slice 0: execution foundation — completed

Delivered:

- Go 1.24 module and strict domain values;
- Judge outcome foundation and deterministic precedence;
- direct Docker target execution with explicit argv;
- capability checks and typed incompatible-host results;
- budget and CallTrace evidence;
- deterministic Docker plan, resource identity, and labels;
- detached watchdog, deadline, stop, kill, wait, and control-channel EOF behavior;
- Windows and Docker Desktop probe evidence.

Completion evidence is recorded in docs/evidence/slice0-verification.md. Slice 1 changes neither the completed code nor that evidence.

## 3. Slice 1 lightweight local workflow

Goal: deliver a recoverable single-host foreground core proportional to the product boundary.

### Task 1: architecture contract

- add ADR-0006 and accept the lightweight design;
- amend ADR-0001, ADR-0002, ADR-0004, and ADR-0005;
- reconcile architecture, detailed designs, Phase 1 scope, traceability, README, and TODO;
- add and pass the executable architecture consistency check.

### Task 2: lifecycle values and process locks

- define strict RunState, StageState, StageAttemptState, ReviewDecisionKind, and ReviewDecisionState;
- implement cross-platform OS-backed locks derived from validated RunID;
- prove same-run exclusion, different-run concurrency, and release after process death.

### Task 3: SQLite projections

- add ordered migrations for runs, stage_records, stage_attempts, run_events, control_requests, and review_decisions;
- implement expected-version transitions and atomic projection plus event;
- add active-time accounting timestamps used only for metering;
- prove review and cancellation constraints.

### Task 4: call and budget ledgers

- persist logical operations, physical call records, reservations, settlement, and CallTrace;
- enforce all configured limits transactionally;
- preserve stable logical idempotency and conservative unknown-boundary handling;
- prove concurrent reservations cannot overspend.

### Task 5: Blob CAS and occurrences

- implement private content-addressed Blob publication, verified reads, declarations, writer tokens, pins, and occurrences;
- bind every occurrence to run, stage attempt, role, revision, and source evidence;
- prove traversal resistance, corruption detection, deduplication, crash safety, and atomic occurrence attachment.

### Task 6: cache, mutation, and maintenance

- persist cache source calls and artifact references;
- retain mutation and provenance accounting;
- make garbage collection an explicit command under the exclusive artifact lock;
- prove cache hits create complete current-run provenance and cannot race ordinary workflow use.

### Task 7: Docker identity persistence and reconciliation

- persist SandboxExecution and complete resource plans before Docker create;
- authorize exact RunID, AttemptID, SandboxExecutionID, logical operation, scope, plan, and engine identities;
- retain deterministic labels and the detached watchdog;
- implement a narrow reconciler limited to exact inspect, stop, kill, wait, remove, and settlement;
- prove unrelated resources are never touched.

### Task 8: fixed typed Fake pipeline

- assemble a concrete typed constructor;
- implement the local coordinator and immutable RunView;
- run external work outside SQLite write transactions;
- implement bounded stage retry, manual resume, current-stage restart, review application, and cancellation;
- keep READY unavailable before package verification.

### Task 9: local CLI and configuration

- add strict local runtime, storage, lock, accounting, provider, and sandbox configuration;
- implement generate, run list/show/events/resume/cancel, and review commands;
- retain stable exit codes and JSON envelopes;
- define immediate same-run lock conflict and process-restart semantics.

### Task 10: crash and Docker persistence proof

- inject process death at every durable stage boundary;
- prove no duplicate irreversible effects, budget overspend, event duplication, or artifact corruption;
- test kill while target runs, stop before export, and death during cleanup;
- confirm later reconciliation never continues an incomplete old export.

### Task 11: boundary audit and checkpoint

- run full tests, vet, race tests, architecture check, Linux cross-build, and Docker-required gates where supported;
- scan production source for forbidden generic-runtime mechanisms;
- update traceability and evidence;
- record the Slice 1 checkpoint without changing Slice 0 history.

### Slice 1 completion criteria

- one executor can create, pause, resume, review, cancel, and inspect a run locally;
- a competing same-run process cannot execute stages;
- process death releases the OS lock and restart reconciles only the current stage;
- SQLite projections, events, budgets, calls, artifacts, cache, and sandbox resources remain consistent at crash boundaries;
- cancellation waits for proof that untrusted targets stopped;
- the Fake pipeline reaches every pause and failure path deterministically;
- all full repository gates pass.

## 4. Slice 2: idea, statement, model, and similarity

Deliver typed GenerationRequest, Idea, Statement, model adapters, prompt registry, strict structured output, similarity adapter, evidence cache, policy decisions, privacy rules, and review routing.

Completion requires deterministic Fake E2E coverage, opt-in provider smoke tests, current-policy dependency checks after blocking, complete budget and provenance records, and no package-unsafe content leakage.

## 5. Slice 3: solution and Docker Judge

Deliver solution generation, compile/run integration, reference solution verification, checker/SPJ support, and persisted Judge evidence through the Slice 0 Docker boundary.

Completion requires authoritative target measurements, exact sandbox identity, watchdog safety, deterministic Judge precedence, bounded repair, and complete artifacts and CallTrace.

## 6. Slice 4: data and quality gates

Deliver test-data generation, validator, expected outputs, oracle and differential testing, mutation checks, resource-limit evidence, and final quality reports.

Completion requires deterministic failing seeds, minimized evidence where policy requires it, strict budgets, reproducible reports, and review routing for ambiguous quality failures.

## 7. Slice 5: package and end-to-end acceptance

Deliver internal package staging, canonical manifest, structural and semantic gates, verification receipt, atomic package occurrence, READY binding, export, import verification, and full E2E fixtures.

Completion requires deterministic package bytes, path safety, crash-safe publication, same-run verified occurrence constraints, clean-workspace E2E, and acceptance evidence.

## 8. Per-change minimum gates

Every change runs focused tests first, then:

~~~powershell
go test ./...
go vet ./...
git diff --check
~~~

Boundary or concurrency changes also run race tests. Docker changes run real-engine safety tests in a compatible environment. Documentation changes run scripts/check-slice1-architecture.ps1.

## 9. Deferred work

Remote execution, many-host coordination, always-on timers, operational workflow search, arbitrary plugin graphs, and web/API control remain outside the MVP. Any such expansion requires a separate architecture decision and migration plan.
