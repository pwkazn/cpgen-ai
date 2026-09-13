# ADR-0002: Run State, Review, Retry, and Local Recovery

Status: Accepted (amended by ADR-0006)

Date: 2026-08-30

## Context

Phase 1 needs durable restart and human review for a fixed local pipeline. It does not need distributed ownership or a general workflow engine.

## Decision

### Run states

The closed run state set is:

- CREATED
- RUNNING
- BLOCKED
- NEEDS_REVIEW
- READY
- FAILED
- CANCELLED

READY is unreachable until Slice 5 atomically binds the same-run verified package occurrence and final quality report.

### Stage and attempt states

A compiled stage has PENDING, RUNNING, SUCCEEDED, BLOCKED, NEEDS_REVIEW, FAILED, or CANCELLED state. Each physical execution is an append-only stage attempt with RUNNING, SUCCEEDED, BLOCKED, NEEDS_REVIEW, FAILED, CANCELLED, or INTERRUPTED state.

An abnormal process exit can leave the run and current attempt recorded as RUNNING. The next manual resume command acquires the per-run process lock, reconciles unfinished sandbox work, records the interrupted attempt, and starts or replays the current stage according to its domain evidence.

### Retry and blocked resume

Retry is bounded and owned by the current stage policy. A physical retry gets a new attempt ordinal and call record while retaining the stable logical idempotency key. Unknown external send boundaries must reconcile the original identity or settle conservatively into a typed pause or failure.

BLOCKED stores the current stage input digest, dependency identity, policy digest, error evidence, and retry-after time. Manual resume creates a fresh attempt of that same stage. Its first authorized operation revalidates the dependency through the ordinary metered port and current policy; stale capability data cannot by itself resume work.

### ReviewDecision

ReviewDecision kinds are REVISE, RETRY, WAIVE, and REJECT. Their lifecycle is PENDING, APPLIED, REJECTED, or STALE.

Review commands create an immutable PENDING decision. They do not directly mutate generated content or advance the run. Manual resume applies exactly one matching decision in a short transaction after validating run version, revision, evidence, policy, and budget bindings.

### Cancellation

The cancel command inserts one idempotent control request. A foreground executor polls it, cancels the root context, stops authorizing new work, and settles already-authorized effects.

A run cannot become CANCELLED until every untrusted sandbox target is proven stopped. If no executor holds the process lock, the cancel command may acquire it, reconcile the exact persisted sandbox resources, and commit the terminal state. Cleanup evidence may still be settled after cancellation, but no ordinary stage work may start.

## Consequences

- Seven run states are sufficient for CLI presentation and persistence.
- Restart behavior is current-stage and domain-specific.
- Review is immutable, auditable, and applied only by the coordinator.
- Cancellation remains responsive without allowing two executors for one run.
- Docker stop safety is represented by SandboxExecution state, not an extra run mode.

## Superseded design

<!-- Superseded design: begin -->
The earlier design required NORMAL/PROBING/QUIESCING modes, execution leases, fencing epochs, observation tickets, and generic recovery intents. ADR-0006 replaces those mechanisms with one OS-backed run lock, expected-version database writes, fresh same-stage attempts, and domain-specific reconciliation.
<!-- Superseded design: end -->
