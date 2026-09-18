# CP Problem Generator AI TODO

Status: Current under ADR-0006

Slice 1 checkpoint: **complete**. Slice 2 foundations, durable typed providers, stage-gap recovery, terminal reconciliation and explicit live preview composition pass their gates on `codex/phase2`. The preview preserves original attempts across process crashes and stops at non-waivable review; omitted workflow selection retains Fake behavior. The ordinary C++ forward generation loop passes real Docker and independent CLI acceptance with local provider fixtures. Mutation is deferred for redesign and is not a prerequisite.

## Current priority — architecture rework after user rejection

The first R1–R4 implementation passed behavioral tests but added too many objects and configuration layers. Its [rework record](docs/evidence/architecture-follow-up-2026-09-14.md) is the current implementation reference.

- [x] Remove wrapper lifecycle/recovery objects, the recovery registration map, mirror evidence interfaces and one-use stage configurations.
- [x] Use one coordinator for runtime state, direct stage readers and flat resource composition; consolidate files by responsibility.
- [x] Preserve the fixed loop, offline reads, exact evidence checks, original identities and atomic READY.
- [x] Complete rework full race, Go 1.25 and real Docker recovery/cancellation/export checks; full normal tests, Linux build, vet and architecture checks also pass.
- [x] Freeze the validated toolchain lock in new runs' effective configuration; resume and offline export consume the bound snapshot, while legacy runs retain validated path fallback.

## Completed ordinary-problem loop — 2026-09-09 user revision

Follow the [MVP generation-loop plan](docs/superpowers/plans/2026-09-09-mvp-generation-loop.md). ACCEPT continues to Solution; REJECT, review-band and insufficient business evidence enter human review. Existing dependency-failure recovery and bounded JSON-format repair remain separate.

- [x] **LOOP-01 / SOL-01:** wire verified ACCEPT to Solution in a compatible revision, generate Reference/Brute and explanation, compile in Docker and verify samples. All non-accepted business decisions stop for review without new generation or mutation claims. Public configuration and independent CLI recovery are verified; this slice ends at `solution_checkpoint`, not READY.
- [x] **DATA-01:** the internal MVP generates bounded plans, compiles generator/validator, proves seed-based reproduction, validates samples/generated inputs and produces reference answers through Judge. Public full-loop integration passes under PKG-01.
- [x] **JUDGE-01:** internal Data/Judge/Quality now verify small-case differential, formal-data resources and the fixed checker in actual Docker, retaining committed proof and failure routes. Full CLI/package revalidation passes under PKG-01.
- [x] **PKG-01:** Current artifact assembly, canonical ZIP, atomic VERIFIED/READY, transaction-process crash recovery, independent CLI export and fresh execution from the ZIP pass with real Docker. The full MVP selector is available for ordinary problems. See [package evidence](docs/evidence/mvp-package-commit-foundation.md).

The historical preview keeps its review boundary. [solution.example.yaml](config/solution.example.yaml) enables the forward Solution slice with a pinned local Docker engine and toolchain. The internal MVP now has [real Data execution](docs/evidence/mvp-data-foundation.md), [Judge checks](docs/evidence/mvp-judge-foundation.md) and [Quality plus package format v2](docs/evidence/mvp-quality-package-foundation.md). The [full MVP configuration](config/mvp.example.yaml) now passes ordinary C++ end-to-end acceptance with local provider fixtures and real Docker. External service availability, SPJ, generic untrusted import execution and Go end-to-end acceptance remain separate follow-up work.

Historical integration checkpoint 2026-09-08: [verification evidence](docs/evidence/slice2-library-integration.md), [plan.md — LLM adapter](plan.md#32-llm-适配器实现方案) and [workflow scheduler](plan.md#42-workflow-调度器实现方案). That checkpoint admitted the in-process libraries while preserving the accepted execution/storage contract. Its completion limits describe that date; current execution status is given above. The 2026-09-13 amendment replaces LangGraph assembly with a checked local stage loop.

## Rules

- Follow the accepted ADRs and the lightweight local workflow design.
- Preserve completed Slice 0 code and evidence.
- Use test-driven development and capture the expected failure before implementation.
- Keep external I/O outside SQLite write transactions.
- Keep READY unavailable until same-run package verification.
- Preserve the complete Idea → Statement → Similarity → Solution → Data → Judge → Quality → Package schema. Demo READY is not full workflow acceptance.
- Run focused tests, full gates, and patch checks before each checkpoint.

## Milestones

These original milestones include broader features such as SPJ, minimization and general import execution. Their remaining items do not reopen the completed ordinary-problem P0 loop above.

- [x] Slice 0: execution foundation
- [x] Slice 1 lightweight local workflow
- [ ] Slice 2: idea, statement, model, similarity
- [ ] Slice 3: solution and Docker Judge
- [ ] Slice 4: data and quality
- [ ] Slice 5: package and E2E

## Slice 0: completed

- [x] Create Go 1.24 project structure.
- [x] Add strict domain IDs, enums, quantities, and digests.
- [x] Define Judge outcomes and deterministic precedence.
- [x] Run target containers directly with explicit argv.
- [x] Enforce Docker capability, mount, command, and image rules.
- [x] Record budgets, CallTrace, output, timing, and resource evidence.
- [x] Add deterministic resource names, labels, and engine identity.
- [x] Add detached watchdog deadline and control-channel EOF behavior.
- [x] Record verification in docs/evidence/slice0-verification.md.

## Slice 1: lightweight local core

### Architecture contract

- [x] Add executable documentation consistency checking.
- [x] Accept ADR-0006 and the lightweight design.
- [x] Reconcile architecture, ADRs, detailed designs, plan, traceability, README, and TODO.

### Lifecycle and locking

- [x] Add closed run, stage, attempt, and review values.
- [x] Add cross-platform OS-backed locks derived from validated RunID.
- [x] Test same-run exclusion, different-run concurrency, and release after process death.

### SQLite projection

- [x] Add ordered migrations and migration checksums.
- [x] Persist runs, stage records, attempts, events, control requests, and review decisions.
- [x] Implement expected-version transitions and atomic projection plus event.
- [x] Add active-time accounting timestamps for metering only.

### Calls and budgets

- [x] Persist logical operations, physical call records, reservations, settlement, and CallTrace.
- [x] Enforce call, token, cost, similarity, sandbox, artifact, and active-time limits.
- [x] Keep logical idempotency stable across physical retry.
- [x] Settle unknown send boundaries conservatively.
- [x] Test concurrent reservation limits.

### Blob and occurrences

- [x] Add private SHA-256 Blob publication and verified reads.
- [x] Add artifact declarations, writer tokens, pins, and run-scoped occurrences.
- [x] Test traversal, symlink escape, corruption, deduplication, and crash boundaries.
- [x] Commit occurrence attachment with stage results.

### Cache and maintenance

- [x] Add canonical cache keys, source-call references, Blob references, and current-run uses.
- [x] Retain mutation and provenance accounting.
- [x] Add explicit garbage collection under the exclusive artifact lock.
- [x] Test cache provenance and GC exclusion.

### Docker persistence and reconciliation

- [x] Persist SandboxExecution and the complete resource plan before Docker create.
- [x] Bind authorization to run, attempt, sandbox execution, logical operation, scope, plan, and engine identities.
- [x] Retain deterministic labels and detached watchdog behavior.
- [x] Add narrow exact-resource inspect, stop, kill, wait, remove, and settlement.
- [x] Prove unrelated resources are never touched.

### Fixed typed Fake pipeline

- [x] Assemble the concrete typed constructor.
- [x] Pass immutable RunView and minimum metered ports.
- [x] Implement bounded stage retry and stable identity.
- [x] Implement BLOCKED current-stage resume with fresh dependency revalidation.
- [x] Implement review application, cancellation, and current-stage restart.
- [x] Keep external work outside write transactions.

### CLI and configuration

- [x] Add strict local runtime, storage, lock, accounting, provider, and sandbox configuration.
- [x] Implement generate and run list/show/events/resume/cancel.
- [x] Implement review show/revise/retry/waive/reject.
- [x] Preserve stable JSON envelopes and exit codes.
- [x] Document immediate process-lock conflicts and restart semantics.

### Crash and boundary proof

- [x] Inject process death at every durable stage boundary.
- [x] Prove no duplicate effects, budget overspend, event duplication, or corrupt artifacts.
- [x] Kill the CLI while a target runs, after target stop, and during cleanup.
- [x] Prove restart does not continue an incomplete old export.
- [x] Run full tests, vet, race, Linux cross-build, architecture check, and patch check.
- [x] Record the Slice 1 checkpoint.

### Slice 1 exit criteria

- [x] One local executor can create, pause, resume, review, cancel, and inspect a run.
- [x] A competing same-run process cannot start stage work.
- [x] Process death releases the lock and manual resume reconciles the current stage.
- [x] SQLite and all domain ledgers remain consistent at crash boundaries.
- [x] CANCELLED waits for proof that untrusted targets stopped.
- [x] The deterministic Fake pipeline covers all pause and failure paths.
- [x] Completed Slice 0 tests remain green.

## Slice 2: idea, statement, model, similarity

- [x] Define strict GenerationRequest, Idea, and Statement values.
- [x] Add prompt registry, versions, and strict structured output.
- [x] Add provider-neutral model contracts and the OpenAI-compatible HTTP implementation in `internal/agent/openai.go`.
- [x] Add typed Idea / Statement / Similarity stage boundaries and dependency revalidation in the Slice 2 pipeline.
- [ ] Complete real-provider wiring through persistent budgets, CallTrace, privacy, cache, and bounded retry; see LLM-01–LLM-06 below.
- [ ] Add MeteredSimilarity, evidence cache, decision policy, and review band.
- [x] **SIM-01 — Durable Similarity evidence.** Single physical exchanges, ledger retries/costs, bounded private publication, exact receipt replay and read-only committed-stage reconstruction pass full tests/vet, race, Linux build and architecture/format/patch gates. See [evidence](docs/evidence/slice2-durable-similarity.md).
- [x] **SIM-02 — Compose typed Similarity execution.** Strict semantic input binds the committed Statement chain and execution policy before an attempt; wire identity also binds the attempt. Active-time admission, private occurrence collection, committed decision reconstruction and a new four-stage checkpoint revision pass full tests/vet, race, Linux build and architecture/format/patch checks. See [evidence](docs/evidence/slice2-typed-similarity.md).
- [ ] **SIM-03 / LOOP-01 — Apply the simple committed decision split.** Valid ACCEPT proceeds to Solution; REJECT, review band and insufficient business evidence enter NEEDS_REVIEW. Preserve evidence before pausing, keep dependency failures on existing recovery paths, and do not claim mutation quota or dispatch mutation calls.
- [x] **SIM-03a — Plan from committed evidence and authoritative quota.** A read-only, strictly bound plan covers all five routing outcomes and the shared CONTENT/METADATA allowance. Positive mutation/package limits now survive RunView projection. Focused and full normal/vet/race, Linux and architecture/format/patch gates pass; see [route-plan evidence](docs/evidence/slice2-similarity-route-plan.md).
- [x] Wire fresh dependency revalidation into the real CLI/application preview path; WF-04d covers persisted BLOCKED admission. Future executing routes require their own admission tests.
- [ ] Pass deterministic and opt-in provider E2E through the integrated application path.

### Verified experiments (not production integration)

- [x] Validate the LangChainGo adapter on `codex/cpgen-json-demo` and `codex/langgraph-trial`.
- [x] Validate LangGraphGo `v0.8.5` with LangChainGo `v0.1.14`: fixed graph, conditional repair, pause/resume, checked JSON checkpoints, and same-task exclusion.
- [x] Run DeepSeek `deepseek-v4-flash` generation; replay the saved outputs through real Docker, check all 12 expected answers, and build the demo ZIP without new API calls.
- [ ] Publish integration evidence on the formal branch. The trial uses similarity fixtures and simplified verification; it does not satisfy full Judge/Quality/PackageGate acceptance.

### Integration prerequisites

- [x] **INT-01 — Align the runtime decision.** Amend ADR-0006 and the affected architecture, workflow, LLM, configuration, implementation-plan, traceability, and boundary checks for an in-process LangGraphGo dependency. Retain one foreground executor per run, per-run process lock, fixed pipeline, and no workflow-hosting service.
- [x] **INT-02 — Pin compatible dependencies.** Pin LangChainGo `v0.1.14` / LangGraphGo `v0.8.5`; upgrade the minimum to Go 1.25.0 and update CI/build instructions. Preserve historical Slice 0/1 evidence.
- [x] **INT-03 — Port selected behavior and regressions.** Port provider adaptation through the formal port/strict-schema contract and add application-layer typed graph compatibility probes for serial routing, commit-error propagation, cancellation and no implicit retry. Production graph assembly and durable restart bridging remain WF-01–WF-05.

### LangChainGo adapter implementation

- [x] **LLM-01 — Implement the adapter boundary.** Add `internal/agent/langchain.go` with `port.MeteredLLM`, typed requests/results, prompt registry and schema digest contracts. Verify canonical HTTP and outcome parity with the existing adapter; application wiring remains pending.
- [x] **LLM-02 — Configure the first provider.** Support configurable base URL, model, credential environment reference, timeout, output-token limit and response cap. Credential-free example: `config/deepseek.example.yaml`; strict configuration and `BuildLLMConfig` bind built-in registries and one adapter attempt. This accepts provider settings but does not enable real CLI dispatch.
- [x] **LLM-03 — Connect physical calls to the durable ledger.** Adapt requests through the existing application CallCoordinator and authorization/reservation/settlement protocol. Disable hidden library retries; prove every physical request has its own persisted call and usage evidence, including failed requests and missing usage. LLM-03a/03b establish the call-level bridge; real stage/CLI assembly remains LLM-06.
- [x] **LLM-03a — Single physical dispatch and settlement.** Add PhysicalLLM and LLMCalls with bounded reservations, database physical identity/CallTrace, failed-call usage, Retry-After and cancellation settlement. Recovered uncertain sends never dispatch again. Evidence: [durable dispatch checkpoint](docs/evidence/slice2-durable-llm-dispatch.md).
- [x] **LLM-03b — Private durable result replay.** `NewReplayableLLMCalls` provides bounded private receipts, verified reads, request/schema binding, occurrence attachment and crash recovery. Full tests, vet, race, Linux build and architecture/patch checks pass. The ledger-only constructor retains `ErrLLMReplayUnavailable`. See [replay evidence](docs/evidence/slice2-private-llm-replay.md).
- [x] **LLM-04 — Preserve validation and repair semantics.** Strict validation, private code-only diagnostics and `NewStructuredLLMCalls` permit zero or one configured JSON-format repair, separate from business repair budgets. Stable transport identity, unknown sends, cancellation, budget rejection and process crashes pass full gates. See [bounded repair evidence](docs/evidence/slice2-bounded-json-repair.md).
- [x] **LLM-05 — Preserve cache, errors and privacy.** Private same-run reuse verifies committed original/repaired responses before recording current-attempt provenance, with zero new provider usage. Local artifact completion and stage attachment are atomic; migration 21 admits earlier-attempt cache sources while preserving same-run scope. Error/privacy, crash and full gates pass. See [cache evidence](docs/evidence/slice2-private-llm-cache.md).
- [ ] **LLM-06 — Complete external integration evidence.** Real Idea / Statement factory wiring, local HTTP contracts and exact ledger/request counts pass the preview gates; Fake remains the default. The separately enabled DeepSeek smoke with explicit bounds remains open.

- [x] **LLM-06a — Admit content drafts and derive trusted identities.** Strict Idea/Statement drafts bind locally into IdeaBatch/ProblemSpec with deterministic hashes, seed axes and frozen chain/resource fields. Historical prompts remain available; durable HTTP repair/replay and full gates pass. See [draft evidence](docs/evidence/slice2-content-drafts.md).
- [x] **WF-03a — Reconstruct typed current-stage inputs.** Separate run request schema from input snapshot schema; recover the exact submitted request and effective seed (including blank-brief random admission), resolve committed provider receipts through current successful stage attempts and verified occurrences, rebuild deterministic domain outputs and check both the stored output and downstream selection digest before work. Full gates pass; see [committed-input evidence](docs/evidence/slice2-committed-generation-inputs.md).

### LangGraphGo scheduler implementation (spans Slices 2–5)

- [x] **WF-01 — Add application-level graph assembly.** Fixed application graphs drive the existing typed Slice 1 lifecycle and define the Slice 2 sequence. Persistence compatibility, serial routing, checked commits, control outcomes and current-stage restart pass full gates. Real stage assembly remains WF-02–WF-05. See [graph evidence](docs/evidence/slice2-compiled-graph.md).
- [x] **WF-02 — Replace Fake-only application dispatch.** Explicit frozen configuration and production Bootstrap/RunService composition execute Idea / Statement / Similarity and end at an unfinished non-waivable preview boundary. Normal tests/vet, production race and supplementary lifecycle races, Linux build and architecture/format/patch checks pass. See [preview evidence](docs/evidence/slice2-live-preview.md).
- [x] **WF-02a — Compose durable typed content execution.** Bind Idea/Statement calls to the current active attempt, reconstruct frozen input, collect private receipts beside the typed outcome and index cache sources only after stage commit. Focused tests and full gates pass. See [executor evidence](docs/evidence/slice2-generation-executor.md).
- [x] **WF-03 — Bridge graph progress to authoritative persistence.** Verified input reconstruction and atomic output/occurrence/next-input commits drive the real preview graph; optional cache indexing follows commit. Process crashes and lost/rejected commit returns preserve original attempts and three exact occurrences. Full gates pass. SQLite remains the only authoritative progress store.
- [ ] **WF-04 — Integrate recovery and control.** Preserve same-run process locks, interrupted-attempt reconciliation, fresh dependency checks on BLOCKED resume, ReviewDecision application, persisted cancellation, and Docker target-stop proof. Infrastructure failures must not consume content-repair calls.
- [x] **WF-04a — Bind call mutations to the active run projection.** Preserve original logical-open and completion commands for restart and refresh optimistic versions for ledger-only transitions within the exact current stage attempt. Long-call settlement, cache replay, forced database races, cancellation, changed attempts and unknown sends pass focused tests and full gates. See [active ledger evidence](docs/evidence/slice2-active-llm-ledger.md).
- [x] **WF-04b — Handle the gap between committed stages.** Cancellation and Resume distinguish a PENDING successor with no attempt from a live RUNNING attempt, and ignore cached predecessor identities. Failure-first regressions and full gates pass. See [gap recovery evidence](docs/evidence/slice2-stage-gap-recovery.md).
- [x] **WF-04c — Reconcile providers without authorizing new work.** Receipt/settlement interfaces, LLM/Similarity/structured cleanup, empty OPEN-call settlement and stage-release guards pass full normal tests/vet, race and cross-platform checks. Cleanup starts no transport retry or unopened format repair, and preserves unreadable receipts. See [evidence](docs/evidence/slice2-provider-reconciliation.md).
- [x] **WF-04d — Integrate preview provider lifecycle.** Same-attempt process restart, receipt reconciliation before cancellation/budget exhaustion, persisted fresh-dependency admission after BLOCKED, immutable-config refusal and active-owner cancellation pass normal and race verification. Reproduced heartbeat version and production UTC timestamp errors are corrected. See [preview evidence](docs/evidence/slice2-live-preview.md).
- [ ] **WF-05 — Bind the forward revision and review outcomes.** Enforce stage/input/config/schema bindings and immutable history for the new positive path. Resume old runs through compatible definitions. Automatic Idea mutation and solution-repair loops are deferred.
- [x] **WF-06 — Complete the original business graph.** Follow [the stage mapping](plan.md#41-完整-workflow-schema-的阶段映射): solution compile/sample checks; TestPlan/Generator/Validator and SampleGate; small differential tests; formal inputs/answers/ResourceGate; Quality; PackageGate and final READY transaction. Add real stages with Slices 3–5 instead of placeholder success nodes.
- [x] **WF-07 — Verify the usable forward lifecycle.** Test ACCEPT-to-package, non-accepted-to-review, dependency blocking, cancellation, process crashes, commit failures and concurrent resume. Prove committed stages do not call the model again, no implicit mutation occurs, and missing quality/package evidence cannot produce READY.

### Integration order and acceptance

- [x] Continue from the accepted INT/LLM/preview foundation with LOOP-01/SOL-01 → DATA-01 → JUDGE-01 → PKG-01. Deferred mutation tasks do not gate this sequence.
- [x] Extend the graph through Slices 3–5, completing WF-06/WF-07 and their domain gates together.
- [ ] Run focused contracts, full tests, vet, required race/cross-build and architecture checks; use real Docker and explicitly enabled provider/similarity smoke for integration evidence.
- [ ] Record exact revisions, provider configuration without secrets, physical call counts, resume behavior and gate results. Mark complete only for the formal workflow actually tested; label fixture/replayed evidence explicitly.

## Slice 3: solution and Docker Judge

- [x] Generate and validate reference and candidate solutions.
- [x] Integrate compile and run through the retained Docker boundary.
- [x] Add and execute the fixed exact-token checker, including AC/WA canaries.
- [ ] Add SPJ support (outside the ordinary-problem MVP).
- [x] Persist Judge, target measurement, CallTrace, and artifact evidence.
- [x] Route invalid solution content and failed correctness checks to review/failure with evidence; automatic business repair is deferred.
- [ ] Pass compatible-host Docker safety gates.

## Slice 4: data and quality

- [x] Generate and validate tests.
- [x] Produce expected outputs through trusted oracle paths.
- [x] Add differential, boundary and resource-limit validation.
- [ ] Reconsider mutation-based validation after mutation redesign.
- [ ] Persist reproducible failure and minimization evidence.
- [x] Produce the final quality report.

## Slice 5: package and E2E

- [x] Build the canonical internal package through declared occurrences.
- [x] Run structural and semantic package gates.
- [x] Create verification receipts and final quality binding.
- [x] Commit VERIFIED package occurrence and READY atomically.
- [x] Export canonical packages and reject malformed or substituted archives on read; recompile and revalidate an exported ordinary C++ package in a fresh test workspace.
- [ ] Add a general untrusted-package import execution workflow.
- [x] Pass clean-workspace deterministic E2E.

## Deferred mutation research

The user paused this work on 2026-09-09. Completed checkpoints below remain historical evidence; unchecked items are deferred for redesign and are not prerequisites for the usable generation loop. BR-01b is no longer the next task. See the [current plan](docs/superpowers/plans/2026-09-09-mvp-generation-loop.md).

- [x] **MUT-01a — Define deterministic mutation core, intent and draft lineage.** Separate pre-claim identity from the bound durable grant, preserve exact request/seed/source fields, derive distinct child ordinals and reject crossed proofs. Pure and actual SQLite-grant tests and complete gates pass; see [contract evidence](docs/evidence/slice2-mutation-contracts.md). This does not authorize a production route.
- [x] **MUT-01b — Verify mutation result ledger and settled output attachment.** Actual output publication/settlement, unique immutable evidence and exact grant-limit checks pass complete gates. Migration 24 also binds occurrence metadata to immutable declarations/cache entries. Real record commit crashes and exact replay pass; see [artifact and result-ledger evidence](docs/evidence/slice2-artifact-mutation-records.md).
- [x] **MUT-01c — Commit mutation output, record and stage atomically.** The composite command preserves lifecycle guards, binds complete attempt evidence and advances with its output record in one transaction. A read-only reader reconstructs exact historical result metadata. Rollback/cancel/process/replay, mixed failed/successful evidence, corruption and complete gates pass. See [atomic completion evidence](docs/evidence/slice2-atomic-mutation-stage.md).
- [ ] **SIM-03b / WF-05a — Apply checked routing atomically.** Introduce a compatible revision, exact replayable authorization command, source-proof retention, shared claim and downstream invalidation in one transaction. Preserve pending cancellation and active-attempt checks. Follow the [business routing plan](docs/superpowers/plans/2026-09-09-slice2-business-routing.md).
- [ ] **MUT-02 / WF-07a — Execute and recover the bounded loop.** Preserve no-feasible source evidence, execute the authorized draft, retain operations/settlements/output records, reselect and regenerate downstream stages, and prove rejection/recheck/cancel/crash paths before enabling the new revision.
- [x] **MUT-02a — Separate candidate collection from selection.** A committed full batch can be reconstructed even when all recorded candidates are rejected; it cannot become Statement input. The original preview still reviews at Idea without new work on Resume. Focused and complete gates pass; see [candidate evidence](docs/evidence/slice2-idea-candidates.md).
- [x] **MUT-02b — Bind a separate mutation provider contract.** The compiled mutation prompt and schema cannot reuse initial draft bindings. Local durable format repair/replay retains one logical mutation claim. Focused and complete gates pass; see [prompt evidence](docs/evidence/slice2-mutation-prompt.md).
- [x] **BR-01a — Publish a typed initial IdeaBatch.** The independent publisher retains provider EVIDENCE and canonical OUTPUT with exact physical-byte accounting. Successful and review completion plus sealed/finalized process recovery pass focused and complete gates. See [output evidence](docs/evidence/slice2-idea-batch-output.md). Current typed/history readers and mutation publication remain separate follow-ups.
- [ ] **BR-01b — Deferred typed-batch reader research.** The former proposal verifies current typed output and provider evidence together; see the [historical sequence](docs/superpowers/plans/2026-09-09-slice2-business-routing.md#next-executable-sequence). It is not needed before Solution and will be reconsidered only if a redesigned mutation path needs it.

## Later phases

- [ ] Improve diversity, quality ranking, and explanation tooling.
- [ ] Evaluate expanded deployment requirements through new ADRs.
- [ ] Add operational interfaces only after the local MVP evidence justifies them.

## Common commands

~~~powershell
go test ./...
go vet ./...
go test -race -timeout 30m ./...
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
git diff --check
~~~
