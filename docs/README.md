# CP Problem Generator Documentation Index

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
