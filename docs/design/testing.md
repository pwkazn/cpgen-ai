# Testing and Acceptance Design

Status: Current under ADR-0006

## 1. Test layers

1. Domain unit tests for strict values, transitions, policy, and digests.
2. Application tests with real coordinator and store plus deterministic Fake ports.
3. Adapter contract tests for SQLite, filesystem, HTTP, and Docker boundaries.
4. Subprocess and crash-injection tests for locks and restart.
5. Opt-in integration tests for Docker and configured external services.
6. End-to-end CLI and package verification tests.

Ordinary tests are offline, deterministic, and parallel-safe. Time, randomness, IDs, and provider responses are injected.

Application business fixtures may copy an empty fully migrated database created once by the real migrator in the current test process. Each copy uses an exclusive new path and still passes through normal OpenWithClock validation. Runs, transactions, connections and mutable rows are never shared. Database reopening, production Bootstrap and SQLite migration/upgrade tests continue to use their original real initialization paths. Verify control-state isolation and refusal to overwrite existing databases when changing this fixture helper.

## 2. Slice 0 evidence retained

Completed Slice 0 tests remain release evidence for:

- strict domain values and Judge outcome precedence;
- direct target execution without a shell wrapper;
- command and mount allowlists;
- timeout, kill escalation, and target-stop proof;
- detached watchdog deadline and parent-channel EOF behavior;
- deterministic Docker names, labels, plan identity, and engine identity;
- CallTrace, budget, stdout/stderr, and resource evidence;
- typed incompatible-host behavior when required measurement capability is absent.

Task 1 changes no Slice 0 source or evidence.

## 3. Fixed workflow tests

Table-driven tests cover the seven run states, stage states, attempt states, review kinds, and review lifecycle.

Compile-time and source-boundary tests prove:

- the concrete constructor has typed adjacent inputs and outputs;
- RunView and inputs are copied or immutable;
- stage code has no persistence, lock, raw Docker, unrestricted writer, or mutable run access;
- no runtime graph or untyped registration can select control flow.

Coordinator tests verify deterministic stage order, input/output digests, event order, downstream invalidation, and READY being unavailable before verified package binding.

## 4. Process-lock tests

Use two real subprocesses and a temporary private runtime directory:

- two executors racing for the same RunID admit exactly one;
- the loser receives the documented conflict without starting a stage;
- different RunIDs can run concurrently;
- read-only show and events work while execution owns the lock;
- cancel can insert its idempotent request;
- forced process termination releases the OS lock;
- a later resume obtains the released lock and reconciles the current stage;
- invalid RunID values cannot influence the lock path.

Tests must synchronize on observable readiness, not sleeps.

## 5. SQLite and stage-boundary crash tests

For each durable boundary, inject process termination before and after commit:

- run creation and first event;
- attempt creation and budget reservation;
- call authorization;
- external call return;
- budget settlement;
- Blob publication and token finalization;
- occurrence attachment;
- stage projection and event;
- sandbox resource creation and cleanup;
- package verification and READY binding.

Restart must produce one authoritative projection, no duplicate event version, no budget overspend, no orphaned active writer token, and no repeated irreversible effect under a new identity.

Transaction instrumentation fails the test if network, Docker, hashing, fsync, rename, verified Blob read, watchdog IPC, or blocking wait occurs during a SQLite write transaction.

## 6. Retry, blocking, and idempotency

Tests prove:

- bounded retry creates new physical records and preserves logical identity;
- success, block, review, failure, cancellation, and budget exhaustion stop retry;
- an unknown send boundary reconciles the original identity or charges conservatively;
- BLOCKED resume creates a fresh same-stage attempt;
- the first dependency action uses the checkpoint identity and current policy;
- old health data cannot by itself resume the stage;
- failure returns to BLOCKED with new evidence;
- success may continue normal stage work.

## 7. Review and cancellation

Review tests cover immutable PENDING creation, exact decision matching, stale rejection, revision invalidation, bounded waiver scope, and REJECT termination.

Cancellation tests cover concurrent request insertion, root-context cancellation, refusal of new authorization, settlement of existing reservations, review staleness, and idempotent repeated cancel.

A cancellation test with a live Docker target must prove the run does not commit CANCELLED until the target is stopped and stop evidence is durable.

## 8. Budget and CallTrace

Concurrent reservation tests verify every configured dimension, exact limit enforcement, monotone settlement, and replay. Provider and Docker calls must persist complete CallTrace evidence. Cache results must retain source-call and artifact provenance.

Active-time tests inject clock movement, clean pause, crash after accounting heartbeat, conservative one-interval charging, and deadline caps.

## 9. Blob, cache, and package tests

Blob tests include traversal, symlink escape, partial write, crash before and after rename, digest collision simulation, corruption on verified read, deduplication, token reuse, pin lifecycle, and explicit GC lock exclusion.

Cache tests include canonical keys, policy mismatch, expiry, corrupt source Blob, complete current-run provenance, and stale health data.

Package tests include path normalization, duplicate logical paths, manifest canonicalization, structural gates, semantic gates, verification receipts, publication crash points, and the same-run READY constraint.

## 10. Narrow Docker recovery

Use the real Docker Engine when the profile is available. Required scenarios:

- CLI killed while target runs;
- target stops before output transfer;
- CLI killed during cleanup;
- watchdog control EOF;
- watchdog process death detected by the runner;
- late create discovered by deterministic name and labels;
- unrelated container with similar metadata is never touched;
- repeated reconciliation is idempotent;
- later resume does not continue an incomplete prior export.

The reconciler may only inspect, stop, kill, wait, remove, and settle exact persisted resources. Test instrumentation fails if it schedules a stage or publishes an artifact.

## 11. Similarity, model, Judge, and package gates

Model adapters are tested with strict structured fixtures, bounded stage-local retry, privacy redaction, canonical request digest, idempotency, and conservative unknown-boundary handling.

Similarity tests cover HTTPS and allowlists, redirects, response limits, evidence cache provenance, policy thresholds, review bands, and fresh dependency checks on resume.

Judge tests preserve precedence, checker protocol, resource evidence, differential validation, and deterministic ordering. Package gates must reject any missing or inconsistent required evidence.

## 12. Commands and release gates

Required gates:

~~~powershell
go test ./...
go vet ./...
go test -race ./...
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
git diff --check
~~~

Linux cross-build and Docker-required tests run in their documented environments. Real provider smoke tests require explicit configuration and never run in ordinary CI.

## 13. Slice acceptance

Slice 1 is accepted only when the process lock, projections, domain ledgers, Fake pipeline, CLI commands, restart boundaries, cancellation safety, and narrow Docker reconciliation pass together while all completed Slice 0 tests remain green.
