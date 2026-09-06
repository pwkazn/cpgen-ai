# CP Problem Generator AI TODO

Status: Current under ADR-0006

Slice 1 checkpoint: **complete**. Next: Slice 2 — idea, statement, model, and similarity.

## Rules

- Follow the accepted ADRs and the lightweight local workflow design.
- Preserve completed Slice 0 code and evidence.
- Use test-driven development and capture the expected failure before implementation.
- Keep external I/O outside SQLite write transactions.
- Keep READY unavailable until same-run package verification.
- Run focused tests, full gates, and patch checks before each checkpoint.

## Milestones

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

- [ ] Define strict GenerationRequest, Idea, and Statement values.
- [ ] Add prompt registry, versions, and strict structured output.
- [ ] Add MeteredLLM with budgets, CallTrace, privacy, cache, and bounded retry.
- [ ] Add MeteredSimilarity, evidence cache, decision policy, and review band.
- [ ] Revalidate an unavailable dependency in a fresh same-stage attempt.
- [ ] Add deterministic Fake E2E and opt-in provider smoke tests.

## Slice 3: solution and Docker Judge

- [ ] Generate and validate reference and candidate solutions.
- [ ] Integrate compile and run through the retained Docker boundary.
- [ ] Add checker and SPJ protocols.
- [ ] Persist Judge, target measurement, CallTrace, and artifact evidence.
- [ ] Add bounded repair and review routing.
- [ ] Pass compatible-host Docker safety gates.

## Slice 4: data and quality

- [ ] Generate and validate tests.
- [ ] Produce expected outputs through trusted oracle paths.
- [ ] Add differential, mutation, boundary, and resource-limit validation.
- [ ] Persist reproducible failure and minimization evidence.
- [ ] Produce the final quality report.

## Slice 5: package and E2E

- [ ] Build the canonical internal package through declared occurrences.
- [ ] Run structural and semantic package gates.
- [ ] Create verification receipts and final quality binding.
- [ ] Commit VERIFIED package occurrence and READY atomically.
- [ ] Add exporter and untrusted package import verification.
- [ ] Pass clean-workspace deterministic E2E.

## Later phases

- [ ] Improve diversity, quality ranking, and explanation tooling.
- [ ] Evaluate expanded deployment requirements through new ADRs.
- [ ] Add operational interfaces only after the local MVP evidence justifies them.

## Common commands

~~~powershell
go test ./...
go vet ./...
go test -race ./...
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
git diff --check
~~~
