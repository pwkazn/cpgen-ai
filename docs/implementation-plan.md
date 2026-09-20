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

**Historical priority, 2026-09-09 (ordinary-problem loop completed):** complete the usable forward generation loop. Valid Similarity ACCEPT proceeds to Solution; non-accepted business results enter human review. Implement LOOP-01/SOL-01 → DATA-01 → JUDGE-01 → PKG-01 according to the [current executable plan](superpowers/plans/2026-09-09-mvp-generation-loop.md). Automatic mutation and its retained-source/authorization prerequisites are deferred for redesign and do not gate MVP delivery. Existing Quality and PackageGate checks remain required.

Deliver typed GenerationRequest, Idea, Statement, model adapters, prompt registry, strict structured output, similarity adapter, evidence cache, policy decisions, privacy rules, and review routing.

Completion requires deterministic Fake E2E coverage, opt-in provider smoke tests, current-policy dependency checks after blocking, complete budget and provenance records, and no package-unsafe content leakage.

The initial [architecture simplification plan](superpowers/plans/2026-09-13-architecture-simplification.md) is followed by [the initial R1–R4 proposal](design/architecture-follow-up-2026-09-14.md), which was reworked after user rejection; current changes and verification are in its [rework record](evidence/architecture-follow-up-2026-09-14.md). The ordinary-problem loop is implemented; its broader historical slice milestones below do not reopen that delivery.

### Historical library integration checkpoints (2026-09-08)

These record the original integration. The 2026-09-13 ADR amendment replaces LangGraphGo with a checked local loop and retains LangChainGo; current dependency requirements are in go.mod.

1. INT-01/INT-02: amend ADR-0006 and current designs; pin LangChainGo v0.1.14 and LangGraphGo v0.8.5 with Go 1.25.0, minimum/current CI, import boundaries and upstream contract probes. Preserve historical Slice 0/1 evidence.
2. LLM-01: port the provider adapter through the existing MeteredLLM contract, compare canonical requests and typed outcomes to the HTTP adapter, and retain local strict schemas and endpoint policy.
3. LLM-02 through LLM-06: complete provider configuration, durable physical dispatch and replay, one bounded JSON repair and cache/privacy; wire real stages through the application factory with local HTTP lifecycle evidence. Record opt-in external smoke separately before claiming external-service acceptance.
4. WF-01 through WF-05: assemble fixed serial graph nodes in internal/application, commit through existing SQLite/Blob protocols, preserve recovery/review/cancel and revision compatibility, and stop at the implemented slice boundary. SQLite remains authoritative; no graph.json or library checkpoint persistence is introduced.
5. Complete WF-06/WF-07 with Slices 3–5 and the complete eight-stage business graph. No unimplemented stage or library terminal result can substitute for Judge, Quality or PackageGate.

Per-slice status is recorded by the linked verification evidence below. The [provider configuration and durable dispatch checkpoint](evidence/slice2-durable-llm-dispatch.md) adds LLM-02 and LLM-03a. LLM-03b implements private response publication, verified replay and crash recovery; [replay evidence](evidence/slice2-private-llm-replay.md) records full gates. LLM-04 adds one configured format repair with durable sanitized diagnostics and separate accounting; [repair evidence](evidence/slice2-bounded-json-repair.md) records its full gates.

LLM-05 completes [private same-run cache provenance](evidence/slice2-private-llm-cache.md), and WF-01 supplies the [compiled application graph](evidence/slice2-compiled-graph.md). LLM-06a adds [strict content drafts](evidence/slice2-content-drafts.md) whose identities and frozen resource fields are derived locally. Assembly exposed a heartbeat/version conflict in long model calls and cache completion replay; WF-04a addresses it with an [attempt-bound ledger and original command receipts](evidence/slice2-active-llm-ledger.md).

WF-03a completes [typed committed-input recovery](evidence/slice2-committed-generation-inputs.md): current stage/attempt provenance and verified private bytes reconstruct the semantic Idea/Statement chain without provider requests, including after review invalidation and cache reuse. The accepted live preview now composes per-attempt execution, atomic receipt attachment, the explicit workflow selector and recovery/control lifecycle.

Composition status and remaining boundaries:

1. WF-02a's typed executor acceptance is complete, including cache reuse at zero remaining call budget and replay across active-time versions.
2. SIM-01 and SIM-02 pass complete gates for durable Similarity dispatch, private evidence/replay, committed reads and typed composition against the verified Statement chain. The semantic Similarity input binds problem/snapshot, decision and provider policy, result limit, exact retry settings and cost ceiling before the next attempt. Wire logical identity adds run and attempt; the unknown next attempt is absent from the preceding stage's output binding. The HTTP adapter derives its Idempotency-Key from the request, so exact same-attempt replay preserves identity while a fresh BLOCKED attempt receives a new one. Future evidence-cache reuse remains separate.
3. The explicit preview selector and frozen content/provider configuration are implemented in production Bootstrap. Omitted-selector Fake snapshots are preserved; provider configuration alone does not select live execution. Full normal tests/vet, production and supplementary lifecycle race verification, Linux build and architecture/format/patch checks pass.
4. SIM-02 adds `slice2.idea.statement.similarity.checkpoint.v1` with a fourth `slice2_checkpoint` stage; the existing three-stage revision is unchanged. Successful collection returns the private occurrence separately from the policy decision, so Similarity evidence commits before checkpoint routing. SIM-03 must apply the committed Accept/Review/Reject/Blocked decision through the real lifecycle. Existing control-outcome occurrence restrictions remain intact. The checkpoint cannot produce READY or waive unimplemented later gates.
5. The real preview service preserves the exact RUNNING attempt across before-call, sealed and completed provider boundaries. Typed terminal reconciliation restores existing receipts before cancellation or exhausted-budget release. Same-attempt recovery and terminal cleanup have distinct authorization paths. Fifteen real process-crash scenarios and six failed/lost commit returns pass normal and race verification with exact attempts, occurrences and HTTP counts.
6. BLOCKED dependency work now starts inside accounting. An authoritative predecessor checkpoint survives restart before call opening and bypasses historical generation cache as proof of recovery; successful generation establishes current availability and supplies the result in the same metered operation. Caller-supplied cancellation versions remain strict, while an accounting heartbeat retries transient version conflicts on its next bounded tick.

The integration tests must stop execution after provider completion, private receipt publication and stage commit independently; resume must preserve request/seed/config identity, HTTP counts and the exact next stage at each boundary. Cancellation and exhausted active-time budgets must settle the same durable calls before final run control transitions.

Lifecycle acceptance also covers the gap after a successful stage commit and before the next attempt begins. That projection is RUNNING with a PENDING current stage and no live attempt. WF-04b adds the narrow terminal transition and authoritative attempt lookup, including the case where a live service still caches its predecessor identity. Failure-first regressions and complete gates pass. Finalization still rejects a genuinely RUNNING attempt and preserves earlier committed history.

For provider recovery, separate ordinary same-attempt continuation from terminal cleanup. Cancellation and active-budget exhaustion may replay/settle original DISPATCHING, SENT or completed receipts, but cannot plan a new transport call, format repair or cache lookup. Unstarted OPEN/PREPARED calls need an explicit no-send settlement path that remains valid after a pending cancel. Do not let stage cancellation release the only sealed response while its provider parent remains nonterminal.

WF-03a recovers the exact submitted request and effective seed, validates current successful-stage occurrence provenance, reconstructs typed outputs and compares their stored semantic digests. The [live preview composition](evidence/slice2-live-preview.md) connects those readers, atomic commits, frozen configuration, fresh dependency admission and receipt cleanup through the existing lifecycle. Its explicit checkpoint remains a non-waivable preview for every retained Similarity decision. LOOP-01 introduces a compatible forward revision using committed evidence directly: ACCEPT to Solution, other business outcomes to review. The former [mutation routing plan](superpowers/plans/2026-09-09-slice2-business-routing.md) is frozen research, not the next work sequence. The [development log](development-log.md) records the completed session and later priority revision. Preview acceptance cannot substitute for full MVP gates.

## 5. Slice 3: solution and Docker Judge

Deliver solution generation, compile/run integration, reference solution verification, checker/SPJ support, and persisted Judge evidence through the Slice 0 Docker boundary.

Completion requires authoritative target measurements, exact sandbox identity, watchdog safety, deterministic Judge precedence, and complete artifacts and CallTrace. The first vertical slice generates Reference/Brute and explanation, compiles in Docker and checks samples. Content/correctness failures retain evidence and stop for review/failure; automatic solution repair is deferred.

## 6. Slice 4: data and quality gates

Deliver reproducible test-data generation, validator, expected outputs, oracle and differential testing, resource-limit evidence, and final quality reports. Advanced mutation/adversarial testing follows the first usable loop and does not delay ordinary sample, boundary, differential or resource checks.

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

Boundary or concurrency changes also run `go test -race -timeout 30m ./...`. The explicit suite timeout accommodates instrumented SQLite migration and crash/replay coverage; the 2026-09-09 expanded application sweep passed at 1199.150 seconds, too close to the former 20-minute package limit. Operation-level HTTP, lock and helper deadlines remain bounded independently. Docker changes run real-engine safety tests in a compatible environment. Documentation changes run scripts/check-slice1-architecture.ps1.

## 9. Deferred work

Automatic Idea mutation, automatic business solution repair and the old BR-01b/BR-02–BR-05 mutation source/authorization chain are deferred under the 2026-09-09 user revision. Completed contracts and migration evidence remain historical assets, not required dependencies for the forward path. Reconsider a simpler mutation design only after a usable package-producing loop exists.

Remote execution, many-host coordination, always-on timers, operational workflow search, arbitrary plugin graphs, and web/API control remain outside the MVP. Any such expansion requires a separate architecture decision and migration plan.
