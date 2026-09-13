# ADR-0006: Lightweight Local Workflow

Status: Accepted

Date: 2026-08-31

Amended: 2026-09-08 (in-process library integration)

Amended: 2026-09-13 (local fixed scheduling)

## Context

Phase 1 needs a recoverable product workflow, but its deployment boundary is one host and one local project workspace. The completed Slice 0 Docker watchdog, measurement, budget, and artifact evidence already provide the difficult external-effect safety boundary. Building a hosted general workflow runtime would add mechanisms the product does not require.

## Decision

Decision: one foreground Go CLI executor per run on one host; OS process lock; fixed typed pipeline; SQLite projection plus CPGen domain ledgers; no hosted runtime.

The normative contract uses the exact phrases: one foreground executor per run, per-run process lock, fixed pipeline, and no workflow-hosting service.

The coordinator opens the local store, acquires the deterministic run lock, selects the current compiled stage, records a stage attempt, invokes external work outside write transactions, atomically commits projection and evidence, and exits at a pause or terminal state. Different runs may use different CLI processes concurrently.

The seven run states are CREATED, RUNNING, BLOCKED, NEEDS_REVIEW, READY, FAILED, and CANCELLED. Restart is manual and reruns or reconciles the current domain stage. Retry is bounded inside that stage.

## Supersedes

Supersedes: ADR-0001 only where it implied a generic Step runtime; ADR-0002 execution modes/leases/probe/recovery machinery; the old Slice 1 durable-engine scope.

This decision also rejects a daemon, task queue, arbitrary runtime graph, execution lease, lease epoch, owner epoch, fencing token, PROBING or QUIESCING run mode, observation ticket or floor, generic recovery intent, startup janitor, and distributed ownership. Temporal, AutoGen, CrewAI and hosted LangGraph services remain unnecessary; future cross-host requirements require a separate ADR and proof of need.

## Provider library and local scheduling

The 2026-09-13 architecture simplification replaces the LangGraphGo wrapper admitted on 2026-09-08 with a local fixed loop in `internal/application`. The wrapper had no ownership of checkpoints, retries or recovery; those already belonged to CPGen. The loop selects the current persisted stage, invokes one typed stage boundary and validates its committed transition before continuing. It introduces no new workflow revision or stored identity. Historical library evidence remains historical.

LangChainGo `v0.1.14` remains pinned for provider adaptation in `internal/agent`. The minimum Go version is 1.25.0. Library types stay out of `internal/domain`, `internal/port` and `internal/workflow`. SQLite remains authoritative: progress is reconstructed from projections and verified Blob occurrences. No second graph.json store, automatic library checkpoint persistence, callbacks containing private state, library-managed retries, or user-configurable graph is admitted.

Transport retries use the existing CallCoordinator with one physical dispatch per authorization. JSON-format repair is a separate bounded operation (at most one when enabled); automatic business repair and Idea mutation remain deferred. Scheduler completion never grants READY: all eight business stages and the same-run package transaction remain required. Explicit configuration selects the completed ordinary-problem MVP; historical Fake and preview revisions retain their existing boundaries.

## Retains

Retains: typed inputs/outputs, immutable RunView, restricted ports, ReviewDecision, Docker watchdog/resource identities, budgets, CallTrace, Blob/occurrence, package gates.

In particular:

- the Slice 0 detached watchdog and exact Docker authorization identities remain mandatory;
- immutable content-addressed Blobs, verified reads, writer tokens, pins, and occurrences remain private and run-scoped;
- LLM, Similarity, Docker, artifact, and active-time accounting remains conservative;
- unknown external boundaries reconcile their original identity or pause conservatively;
- READY remains unavailable until a same-run verified package occurrence and final quality report commit atomically.

## Consequences

The design is smaller, testable with local subprocesses, and aligned with actual Phase 1 operations. SQLite is a current projection and audit store, not a replay-driven scheduler. If future requirements include remote workers, automated timers without a CLI process, cross-host recovery, or operational workflow search, a new ADR may adopt a hosted runtime while preserving the typed activity and domain-ledger boundaries.
