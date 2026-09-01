# ADR-0006: Lightweight Local Workflow

Status: Accepted

Date: 2026-08-31

## Context

Phase 1 needs a recoverable product workflow, but its deployment boundary is one host and one local project workspace. The completed Slice 0 Docker watchdog, measurement, budget, and artifact evidence already provide the difficult external-effect safety boundary. Building a hosted general workflow runtime would add mechanisms the product does not require.

## Decision

Decision: one foreground Go CLI executor per run on one host; OS process lock; fixed typed pipeline; SQLite projection plus CPGen domain ledgers; no hosted runtime.

The normative contract uses the exact phrases: one foreground executor per run, per-run process lock, fixed pipeline, and no workflow-hosting service.

The coordinator opens the local store, acquires the deterministic run lock, selects the current compiled stage, records a stage attempt, invokes external work outside write transactions, atomically commits projection and evidence, and exits at a pause or terminal state. Different runs may use different CLI processes concurrently.

The seven run states are CREATED, RUNNING, BLOCKED, NEEDS_REVIEW, READY, FAILED, and CANCELLED. Restart is manual and reruns or reconciles the current domain stage. Retry is bounded inside that stage.

## Supersedes

Supersedes: ADR-0001 only where it implied a generic Step runtime; ADR-0002 execution modes/leases/probe/recovery machinery; the old Slice 1 durable-engine scope.

This decision also rejects a daemon, task queue, arbitrary runtime graph, execution lease, lease epoch, owner epoch, fencing token, PROBING or QUIESCING run mode, observation ticket or floor, generic recovery intent, startup janitor, and distributed ownership. Temporal, LangGraph, AutoGen, and CrewAI are currently unnecessary; future cross-host requirements require a separate ADR and proof of need.

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
