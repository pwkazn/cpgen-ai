# ADR-0001: Static Typed CPGen Pipeline and Activity Contracts

Status: Accepted (amended by ADR-0006)

Date: 2026-08-30

## Context

CPGen needs an auditable path from a generation request to a verified contest package. Adjacent stages must exchange versioned domain values without giving stage code control over persistence, Docker, or unrestricted artifact publication.

## Decision

Phase 1 uses a statically assembled typed Go pipeline. The binary contains a concrete constructor for the ordered Idea, Statement, Similarity, Solution, Data, Judge, Quality, and Package stages. Slice 1 begins with a deterministic Fake pipeline that exercises the same coordinator and persistence boundaries.

Each stage contract has:

- a concrete, versioned input type and output type;
- an immutable RunView plus value or copied inputs;
- only the metered ports authorized for that stage;
- explicit typed outcomes for success, blocking, review, permanent failure, and cancellation;
- deterministic input and output digests used for restart and downstream invalidation.

The application coordinator owns stage selection, persistence, transitions, retry entry, and result commit. Stage code cannot receive SQLite handles, repositories, the run process lock, raw Docker clients, unrestricted Blob writers, or mutable run state. A port may expose only the narrow capability required by the stage and must return CallTrace and budget evidence when it crosses an accounted boundary.

Workflow revision and schema version select a compatible compiled definition. Persisted names and ordinals are audit and compatibility data; they do not define an arbitrary graph. Runtime registration and map[string]any cannot bypass compile-time contracts.

## Consequences

- Interface drift is caught at compile time.
- Tests can copy RunView values and use deterministic Fake ports.
- External I/O remains outside SQLite write transactions.
- A later durable-workflow product can wrap the same serializable inputs, outputs, and idempotent application services without changing domain stage logic.
- Adding or reordering a stage requires a workflow-revision change and explicit downstream invalidation rules.

## Prohibited shortcuts

- heterogeneous stage registries or reflection-selected control flow;
- mutable shared context passed between stages;
- repositories or infrastructure clients passed directly to stage code;
- unversioned payloads or untyped maps at stage boundaries;
- publishing artifacts without the metered artifact port and occurrence binding.

## Superseded design

<!-- Superseded design: begin -->
The original ADR text described a reusable generic Step[I,O] runtime as the workflow center. ADR-0006 supersedes that implication: typed contracts remain, while the concrete CPGen coordinator owns the fixed stage sequence.
<!-- Superseded design: end -->
