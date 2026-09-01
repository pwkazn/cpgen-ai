# CP Problem Generator AI TODO

Status: Current under ADR-0006

## Rules

- Follow the accepted ADRs and the lightweight local workflow design.
- Preserve completed Slice 0 code and evidence.
- Use test-driven development and capture the expected failure before implementation.
- Keep external I/O outside SQLite write transactions.
- Keep READY unavailable until same-run package verification.
- Run focused tests, full gates, and patch checks before each checkpoint.

## Milestones

- [x] Slice 0: execution foundation
- [ ] Slice 1 lightweight local workflow
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

- [ ] Add executable documentation consistency checking.
- [ ] Accept ADR-0006 and the lightweight design.
- [ ] Reconcile architecture, ADRs, detailed designs, plan, traceability, README, and TODO.

### Lifecycle and locking

- [ ] Add closed run, stage, attempt, and review values.
- [ ] Add cross-platform OS-backed locks derived from validated RunID.
- [ ] Test same-run exclusion, different-run concurrency, and release after process death.

### SQLite projection

- [ ] Add ordered migrations and migration checksums.
- [ ] Persist runs, stage records, attempts, events, control requests, and review decisions.
- [ ] Implement expected-version transitions and atomic projection plus event.
- [ ] Add active-time accounting timestamps for metering only.

### Calls and budgets

- [ ] Persist logical operations, physical call records, reservations, settlement, and CallTrace.
- [ ] Enforce call, token, cost, similarity, sandbox, artifact, and active-time limits.
- [ ] Keep logical idempotency stable across physical retry.
- [ ] Settle unknown send boundaries conservatively.
- [ ] Test concurrent reservation limits.

### Blob and occurrences

- [ ] Add private SHA-256 Blob publication and verified reads.
- [ ] Add artifact declarations, writer tokens, pins, and run-scoped occurrences.
- [ ] Test traversal, symlink escape, corruption, deduplication, and crash boundaries.
- [ ] Commit occurrence attachment with stage results.

### Cache and maintenance

- [ ] Add canonical cache keys, source-call references, Blob references, and current-run uses.
- [ ] Retain mutation and provenance accounting.
- [ ] Add explicit garbage collection under the exclusive artifact lock.
- [ ] Test cache provenance and GC exclusion.

### Docker persistence and reconciliation

- [ ] Persist SandboxExecution and the complete resource plan before Docker create.
- [ ] Bind authorization to run, attempt, sandbox execution, logical operation, scope, plan, and engine identities.
- [ ] Retain deterministic labels and detached watchdog behavior.
- [ ] Add narrow exact-resource inspect, stop, kill, wait, remove, and settlement.
- [ ] Prove unrelated resources are never touched.

### Fixed typed Fake pipeline

- [ ] Assemble the concrete typed constructor.
- [ ] Pass immutable RunView and minimum metered ports.
- [ ] Implement bounded stage retry and stable identity.
- [ ] Implement BLOCKED current-stage resume with fresh dependency revalidation.
- [ ] Implement review application, cancellation, and current-stage restart.
- [ ] Keep external work outside write transactions.

### CLI and configuration

- [ ] Add strict local runtime, storage, lock, accounting, provider, and sandbox configuration.
- [ ] Implement generate and run list/show/events/resume/cancel.
- [ ] Implement review show/revise/retry/waive/reject.
- [ ] Preserve stable JSON envelopes and exit codes.
- [ ] Document immediate process-lock conflicts and restart semantics.

### Crash and boundary proof

- [ ] Inject process death at every durable stage boundary.
- [ ] Prove no duplicate effects, budget overspend, event duplication, or corrupt artifacts.
- [ ] Kill the CLI while a target runs, after target stop, and during cleanup.
- [ ] Prove restart does not continue an incomplete old export.
- [ ] Run full tests, vet, race, Linux cross-build, architecture check, and patch check.
- [ ] Record the Slice 1 checkpoint.

### Slice 1 exit criteria

- [ ] One local executor can create, pause, resume, review, cancel, and inspect a run.
- [ ] A competing same-run process cannot start stage work.
- [ ] Process death releases the lock and manual resume reconciles the current stage.
- [ ] SQLite and all domain ledgers remain consistent at crash boundaries.
- [ ] CANCELLED waits for proof that untrusted targets stopped.
- [ ] The deterministic Fake pipeline covers all pause and failure paths.
- [ ] Completed Slice 0 tests remain green.

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
