# CP Problem Generator AI

CPGen is a local, auditable system for generating, validating, judging, and packaging competitive-programming problems.

## Current status

- Slice 0 is completed: strict domain values, Judge foundation, direct Docker execution, detached watchdog, and verification evidence.
- Slice 1 lightweight local workflow is complete, including persistence, ledgers, the fixed typed Fake pipeline, CLI, and crash-recovery evidence.
- Slice 2 foundations, durable typed providers and the live preview lifecycle pass their checkpoint gates, including process-crash and race verification. An explicit configuration selects Idea/Statement/Similarity ending in non-waivable review; the default remains Fake. See [preview configuration](config/README.md), [integration evidence](docs/evidence/slice2-live-preview.md) and the [development log](docs/development-log.md).
- The [MVP configuration](config/mvp.example.yaml) enables verified Similarity ACCEPT → Solution → Data → Docker/Judge → Quality → Package → READY; non-accepted business results enter human review. Real Docker and independent CLI tests pass for an ordinary C++ problem, including transaction-crash recovery, ZIP export and recompilation/revalidation from the exported package. Provider evidence uses local HTTP/TLS fixtures; paid models and external Similarity services were not called. See [usage](config/README.md) and [package evidence](docs/evidence/mvp-package-commit-foundation.md). The older Solution selector still stops at `solution_checkpoint`. Automatic mutation remains deferred under the [active plan](docs/superpowers/plans/2026-09-09-mvp-generation-loop.md).
- The current branch preserves Slice 0 and Slice 1 code and evidence while keeping the workflow local and foreground-only.

## Runtime boundary

Phase 1 uses one foreground Go CLI process for each active run on one host. A deterministic OS-backed run lock prevents two processes from executing the same run, while different runs may proceed concurrently.

The compiled Go binary owns a concrete typed stage sequence. SQLite stores current run and stage projections plus CPGen ledgers for budgets, calls, artifacts, cache, sandbox resources, reviews, and packages. External work occurs outside database write transactions.

ADR-0006 permits LangGraphGo v0.8.5 only for in-process fixed graph assembly in the application layer and LangChainGo v0.1.14 inside the provider adapter. SQLite remains authoritative for progress; library types do not enter domain/port/stage contracts.

The detached Docker watchdog and exact resource identities remain mandatory. Restart is manual and reconciles only the current domain stage and its persisted effects.

## MVP flow

~~~text
Request
  -> Idea
  -> Statement
  -> Similarity
  -> Solution
  -> Data
  -> Judge
  -> Quality
  -> Package gates
  -> READY
~~~

Generative models propose candidates. Deterministic validators, compilers, target execution, Judge rules, similarity policy, and package gates decide acceptance.

## Documents

- ARCHITECTURE.md — current system contract
- docs/adr/0006-lightweight-local-workflow.md — accepted lightweight decision
- docs/superpowers/specs/2026-08-31-lightweight-local-workflow-design.md — authoritative design
- docs/superpowers/plans/2026-08-31-slice1-lightweight-local-workflow.md — Slice 1 implementation plan
- docs/README.md — documentation index
- docs/evidence/slice0-verification.md — completed Slice 0 evidence
- docs/evidence/slice1-verification.md — completed Slice 1 checkpoint evidence

## Development gates

Install Go 1.25.0 or later. Dependencies are pinned in go.mod/go.sum; CI tests Go 1.25.0 and the current stable release. Build the host commands with `go build ./cmd/...`; Linux cross-build uses `GOOS=linux CGO_ENABLED=0 go build ./cmd/...` (set these as environment variables in PowerShell).

~~~powershell
go test ./...
go vet ./...
go test -race -timeout 30m ./...
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
git diff --check
~~~

Docker-required safety tests run only where the documented host profile is available; incompatible hosts must return a typed result rather than weakening isolation.
