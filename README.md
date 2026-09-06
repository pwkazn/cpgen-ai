# CP Problem Generator AI

CPGen is a local, auditable system for generating, validating, judging, and packaging competitive-programming problems.

## Current status

- Slice 0 is completed: strict domain values, Judge foundation, direct Docker execution, detached watchdog, and verification evidence.
- Slice 1 lightweight local workflow is complete, including persistence, ledgers, the fixed typed Fake pipeline, CLI, and crash-recovery evidence.
- Slice 2 (idea, statement, model, and similarity) is next.
- The current branch preserves Slice 0 and Slice 1 code and evidence while keeping the workflow local and foreground-only.

## Runtime boundary

Phase 1 uses one foreground Go CLI process for each active run on one host. A deterministic OS-backed run lock prevents two processes from executing the same run, while different runs may proceed concurrently.

The compiled Go binary owns a concrete typed stage sequence. SQLite stores current run and stage projections plus CPGen ledgers for budgets, calls, artifacts, cache, sandbox resources, reviews, and packages. External work occurs outside database write transactions.

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

~~~powershell
go test ./...
go vet ./...
go test -race ./...
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
git diff --check
~~~

Docker-required safety tests run only where the documented host profile is available; incompatible hosts must return a typed result rather than weakening isolation.
