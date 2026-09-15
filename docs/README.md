# CP Problem Generator Documentation Index

The [2026-09-13 architecture simplification plan](superpowers/plans/2026-09-13-architecture-simplification.md) tracks separation of read/export and execution services, explicit stage dependencies, local fixed scheduling, and consolidated historical compatibility. Its implementation and verification records distinguish completed changes from outstanding environment checks. The local scheduler replaces the earlier LangGraphGo wrapper while retaining the persisted revision and transition contracts.

The [2026-09-14 architecture follow-up](design/architecture-follow-up-2026-09-14.md) preserves the first proposal, whose implementation was rejected for adding too many abstractions. The [rework record](evidence/architecture-follow-up-2026-09-14.md) covers the simplified coordinator, direct readers, removed configuration layers and current verification. The toolchain snapshot follow-up uses the existing effective configuration; a separate publication ledger remains an independent design decision.

## Document hierarchy

1. ARCHITECTURE.md defines the current system boundary.
2. docs/adr records accepted decisions and their supersession.
3. docs/design contains component and contract details.
4. docs/implementation-plan.md defines slice delivery order.
5. docs/traceability.md maps requirements to design and evidence.
6. docs/evidence records completed verification.
7. docs/superpowers/specs contains approved implementation designs.
8. docs/superpowers/plans contains executable engineering plans.

When documents conflict, the newest accepted ADR and its named authoritative design win. ADR-0006 defines Slice 1 lightweight local workflow.

The user revised the MVP priority on 2026-09-09: [complete a usable generation loop first](superpowers/plans/2026-09-09-mvp-generation-loop.md). Similarity ACCEPT continues to Solution/Data/Docker/Judge/Quality/Package; non-accepted business results enter human review. Mutation is deferred for redesign. This scope amendment takes precedence over mutation-first ordering in older plans and component designs.

The [Solution slice](evidence/mvp-solution-foundation.md), [Data execution](evidence/mvp-data-foundation.md), [Judge answers and differential checks](evidence/mvp-judge-foundation.md), and [Quality with package format v2](evidence/mvp-quality-package-foundation.md) now feed [package assembly, atomic READY and CLI export](evidence/mvp-package-commit-foundation.md). The full MVP configuration passes real Docker and independent CLI acceptance for an ordinary C++ problem, including a process exit inside the package transaction and fresh execution from the exported ZIP. A subsequent [APINode live-provider test](evidence/apinode-live-mvp-2026-09-10.md) passes real model generation through Docker, export and independent revalidation. Similarity remains a local fixture; actual originality has not been checked.

New Solution/MVP runs persist the validated canonical toolchain lock with their effective configuration, so CLI resume and offline export can use that bound snapshot after the configured lock path is removed. Historical runs without a snapshot continue to validate and read the original lock path; a missing legacy lock remains a closed failure.

The [pre-execution sandbox recovery fix](evidence/sandbox-unsent-recovery-2026-09-15.md) permits manual resume after the first `solution_verify` create fails before an execution record exists, using an atomic no-send check and a new verification attempt. Original calls and accounting remain intact; sent/unknown work and existing execution records retain receipt recovery.

The historical 2026-09-08 [library integration checkpoint](evidence/slice2-library-integration.md) records the former in-process library amendment and provider boundary tests. Later checkpoints below record preview CLI and durable graph integration; the 2026-09-13 ADR-0006 amendment supersedes the graph-library implementation choice.

The subsequent [provider configuration and durable dispatch checkpoint](evidence/slice2-durable-llm-dispatch.md) records LLM-02 and LLM-03a with independent subagent acceptance. The [private result replay checkpoint](evidence/slice2-private-llm-replay.md), [bounded JSON repair checkpoint](evidence/slice2-bounded-json-repair.md) and [private cache checkpoint](evidence/slice2-private-llm-cache.md) complete LLM-03b through LLM-05 with full gates. The [compiled application graph](evidence/slice2-compiled-graph.md) completes WF-01 with full gates. The [development log](development-log.md) records the active work order.

The [strict content drafts](evidence/slice2-content-drafts.md), [active-time LLM ledger bridge](evidence/slice2-active-llm-ledger.md), [typed committed-stage input recovery](evidence/slice2-committed-generation-inputs.md) and [durable content executor](evidence/slice2-generation-executor.md) complete LLM-06a, WF-04a, WF-03a and WF-02a. The [durable Similarity evidence checkpoint](evidence/slice2-durable-similarity.md) covers physical dispatch, private replay and committed reads. [Typed Similarity execution](evidence/slice2-typed-similarity.md) binds the verified Statement chain to per-attempt evidence and a separate checkpoint; full gates pass. These components now drive the accepted preview service below.

The [stage-gap recovery correction](evidence/slice2-stage-gap-recovery.md) addresses cancellation and stale predecessor identity after a stage has committed and before its successor begins.

The [provider reconciliation component](evidence/slice2-provider-reconciliation.md) adds receipt-only terminal cleanup and prevents stage release before provider settlement; full gates pass.

The [explicit live preview](evidence/slice2-live-preview.md) connects frozen configuration, production Bootstrap, typed graph commits, same-attempt process recovery and provider cancellation/budget cleanup. Normal tests/vet, production and supplementary race verification, Linux build and architecture/format/patch checks pass. The default remains Fake, and the preview ends at non-waivable review. Business routing and later quality/package gates remain open.

The [committed Similarity route plan](evidence/slice2-similarity-route-plan.md) completes read-only decision/quota inspection and corrects omitted logical/package budget limits. Full gates pass. Its mutation-aware routing and the old [business routing plan](superpowers/plans/2026-09-09-slice2-business-routing.md) are retained as deferred research; the forward MVP uses committed decisions directly without mutation quota or authorization.

The [mutation core/intent contracts](evidence/slice2-mutation-contracts.md), [artifact and result-ledger hardening](evidence/slice2-artifact-mutation-records.md), [durable candidate collection](evidence/slice2-idea-candidates.md), [atomic mutation completion and result recovery](evidence/slice2-atomic-mutation-stage.md) and [separate mutation provider contract](evidence/slice2-mutation-prompt.md) retain their completed gates. Further mutation development is paused; these checkpoints do not impose prerequisites on Solution or packaging.

The [application fixture preparation change](evidence/application-fixture-preparation.md) passes complete gates and reduces repeated test setup work while preserving database isolation. The [initial typed batch publisher](evidence/slice2-idea-batch-output.md) also passes complete gates, including real process recovery and review cleanup. It does not alter the preview graph.

## ADR

| ADR | Decision |
|---|---|
| 0001 | Static typed CPGen pipeline and activity contracts |
| 0002 | Seven run states, review, retry, cancellation, and local restart |
| 0003 | Judge outcomes and deterministic precedence |
| 0004 | Direct target execution in an isolated Docker cgroup |
| 0005 | Docker execution lifecycle, watchdog, and cross-stop transfer |
| 0006 | Lightweight local workflow boundary |

## Detailed design

| File | Scope |
|---|---|
| workflow.md | compiled stages, retry, resume, review, cancellation |
| storage.md | SQLite projections and CPGen domain ledgers |
| testing.md | deterministic, subprocess, crash, and Docker acceptance |
| cli.md | commands, exit codes, restart behavior |
| configuration.md | local runtime, storage, provider, and sandbox values |
| llm.md | prompts, strict structured output, accounting, privacy |
| similarity.md | adapter, evidence, cache, and decision policy |
| sandbox.md | direct Docker protocol, watchdog, exact reconciliation |
| judge.md | checker and verdict contracts |
| package.md | internal package, gates, verification, export |
| data-pipeline.md | data generation and differential validation |
| idea-statement.md | request, idea, and statement models |

## Current baseline

Slice 0 is completed and its evidence remains authoritative. Slice 1 uses one foreground local CLI executor for a run, an OS-backed run lock, compiled typed stages, short SQLite transactions, CPGen-specific ledgers, and the retained Docker watchdog.

The accepted design is docs/superpowers/specs/2026-08-31-lightweight-local-workflow-design.md. The implementation plan is docs/superpowers/plans/2026-08-31-slice1-lightweight-local-workflow.md.

## Vocabulary

- Run: one immutable request and its durable current projection.
- Stage: one named compiled CPGen transformation.
- Stage attempt: one physical execution of the current stage.
- RunView: immutable values and read-only remaining budgets visible to a stage.
- CallTrace: physical external-call evidence.
- Blob: immutable digest-addressed bytes.
- ArtifactOccurrence: run-scoped provenance for a Blob.
- SandboxExecution: persisted Docker logical operation and complete resource plan.
- ReviewDecision: immutable human action applied by a later resume.
- PackageOccurrence: a run relation to a staged or verified package.
