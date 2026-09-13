# Requirements and Design Traceability

Status: Current under ADR-0006

Slice 1 checkpoint: **complete**. Slice 2's explicitly selected Idea/Statement/Similarity preview passes full lifecycle gates and ends at non-waivable review. The user now prioritizes ACCEPT → Solution/Data/Docker/Judge/Quality/Package and non-accepted business results → review. Automatic mutation is deferred for redesign. See [Slice 1 evidence](evidence/slice1-verification.md), [preview evidence](evidence/slice2-live-preview.md) and the [current implementation plan](superpowers/plans/2026-09-09-mvp-generation-loop.md).

## MVP functional requirements

| Requirement | Decision/design | Verification |
|---|---|---|
| Structured request to verified package | ARCHITECTURE sections 1, 6, 12; Phase 1 design | fixed-pipeline E2E and package-gate tests |
| Typed stage boundaries | ADR-0001; workflow design | compile-time and source-boundary tests |
| Single-host foreground execution | ADR-0006; CLI design | real subprocess command tests |
| One mutating executor per run | ADR-0006; workflow/storage design | same-run lock race and process-death release |
| Durable pause and manual resume | ADR-0002; workflow design | crash at every stage boundary |
| Human review | ADR-0002; workflow/storage/CLI | decision lifecycle and stale-binding tests |
| Responsive cancellation | ADR-0002; workflow/CLI/sandbox | concurrent cancel and target-stop proof |
| Model calls with structured output | LLM design | strict content drafts, private response/diagnostic recovery, bounded repair and local domain binding; [draft evidence](evidence/slice2-content-drafts.md) |
| LangChainGo provider boundary | ADR-0006 amendment; LLM design | canonical HTTP parity, strict validation, durable physical ledger, exact private replay, usage, cancellation and redaction; [dispatch evidence](evidence/slice2-durable-llm-dispatch.md) and [active ledger evidence](evidence/slice2-active-llm-ledger.md) |
| In-process LangGraphGo execution | ADR-0006 amendment; workflow design | typed serial routing, checked production commits, same-attempt restart and cancellation; [graph evidence](evidence/slice2-compiled-graph.md) and [preview evidence](evidence/slice2-live-preview.md) |
| Private model response recovery | LLM design; artifact and budget protocols | internal/application/llm_replay_test.go and llm_replay_process_test.go: verified replay, no resend, budget/cancel/commit failure; artifact_reader_test.go: populated migration and exact reader bindings; full checkpoint gates pass |
| Bounded JSON-format repair | LLM design; strict provider configuration | physical_repair_test.go: eligible sanitized diagnostics; llm_validation_test.go: private failure receipts; llm_structured_test.go and llm_structured_process_test.go: one immutable repair, separate traces/usage, budget/cancel/crash recovery and atomic attachment; full checkpoint gates pass |
| Similarity evidence and policy | Similarity design | durable physical/private evidence, typed committed input, fresh dependency checks and all decision/quota plans; [typed evidence](evidence/slice2-typed-similarity.md) and [read-only route plan](evidence/slice2-similarity-route-plan.md); actual business route remains open |
| Accepted Similarity to usable package | Current generation-loop plan LOOP-01/SOL-01 through PKG-01 | Pending: ACCEPT continues to real Solution/data/Judge/package; REJECT/review/insufficient business evidence stops for human review with zero mutation calls; complete exported-package E2E |
| Direct Docker Judge execution | ADR-0004; sandbox design | Slice 0 plus real-engine tests |
| CLI-loss target safety | ADR-0005; sandbox design | watchdog deadline/EOF and kill tests |
| Immutable artifacts and provenance | storage design | Blob/writer/pin and corruption tests; declared and cached metadata binding, settled-write attachment without double charge; [artifact evidence](evidence/slice2-artifact-mutation-records.md) |
| Deterministic package and READY | package design | structural/semantic gates and same-run constraint |

## Non-functional requirements

Mutation core/intent, result ledger, candidate retention, atomic mutation completion and typed initial batch publication keep their [historical evidence](superpowers/plans/2026-09-09-slice2-business-routing.md). Their unfinished source readers/authorization/loop are deferred research, not MVP functional requirements.

| Requirement | Design mechanism | Verification |
|---|---|---|
| No external I/O in write transactions | workflow and storage protocols | transaction instrumentation |
| Conservative budget accounting | budget and call ledgers | concurrent reservation and unknown-boundary tests |
| Stable idempotency | stage attempt and call identities | duplicate command and restart tests |
| Exact Docker cleanup scope | SandboxExecution resource plan | unrelated-resource refusal tests |
| Process crash recovery | OS lock plus domain ledgers | forced subprocess termination suite |
| Security and privacy | strict configuration, private paths, adapter redaction | path, secret, HTTP, and package-safe tests |
| Reproducibility | revisions, canonical digests, image/toolchain identity | deterministic fixtures and package bytes |
| Auditability | run events, attempts, CallTrace, occurrences, receipts | cross-ledger integrity tests |
| Local concurrency | per-run locks plus expected versions | same-run conflict and different-run concurrency |
| Go release quality | repository gates | test, vet, race, cross-build, patch check |

## Slice checkpoints

| Slice | Scope | Evidence |
|---|---|---|
| 0 | direct Docker execution, watchdog, Judge foundation | docs/evidence/slice0-verification.md |
| 1 | lightweight local workflow, persistence, ledgers, Fake pipeline, CLI | [Slice 1 verification evidence](evidence/slice1-verification.md) and architecture check |
| 2 | request, idea, statement, model, similarity | [accepted live preview](evidence/slice2-live-preview.md), [remaining routing plan](superpowers/plans/2026-09-09-slice2-business-routing.md); no READY |
| 3 | solution and Docker Judge | compile/run/checker evidence |
| 4 | data and quality | differential and gate evidence |
| 5 | package and E2E | verification receipt and reproducible package |

## Architecture boundary checks

scripts/check-slice1-architecture.ps1 discovers current ADRs, designs, Phase 1 specifications, architecture, plan, traceability, README files, and TODO. It enforces accepted lightweight terminology and rejects active documentation that reintroduces the superseded general-runtime mechanisms. Rejected alternatives remain documented only in the two current decision records or explicitly delimited historical ADR notes.

The library amendment adds required in-process/authoritative-storage documentation checks. internal/workflow/source_boundary_test.go enforces provider/scheduler import locations across production Go source, including nested directories. CI runs both checks, full race tests and vet, verifies module tidiness and cross-builds Windows and Linux without enabling paid-provider smoke tests.
