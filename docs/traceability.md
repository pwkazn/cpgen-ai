# Requirements and Design Traceability

Status: Current under ADR-0006

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
| Model calls with structured output | LLM design | Fake adapter schema, retry, and privacy tests |
| Similarity evidence and policy | Similarity design | threshold, cache, dependency, and review tests |
| Direct Docker Judge execution | ADR-0004; sandbox design | Slice 0 plus real-engine tests |
| CLI-loss target safety | ADR-0005; sandbox design | watchdog deadline/EOF and kill tests |
| Immutable artifacts and provenance | storage design | Blob, writer-token, occurrence, corruption tests |
| Deterministic package and READY | package design | structural/semantic gates and same-run constraint |

## Non-functional requirements

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
| 1 | lightweight local workflow, persistence, ledgers, Fake pipeline, CLI | Slice 1 checkpoint and architecture check |
| 2 | request, idea, statement, model, similarity | provider and content evidence |
| 3 | solution and Docker Judge | compile/run/checker evidence |
| 4 | data and quality | differential and gate evidence |
| 5 | package and E2E | verification receipt and reproducible package |

## Architecture boundary checks

scripts/check-slice1-architecture.ps1 discovers current ADRs, designs, Phase 1 specifications, architecture, plan, traceability, README files, and TODO. It enforces accepted lightweight terminology and rejects active documentation that reintroduces the superseded general-runtime mechanisms. Rejected alternatives remain documented only in the two current decision records or explicitly delimited historical ADR notes.
