# Provider receipt reconciliation before terminal cleanup

Date: 2026-09-09 UTC+8. Branch: `codex/phase2`.

Status: WF-04c component implementation and complete normal tests/vet, race, Linux build, architecture, formatting and patch checks PASS (04:54 UTC+8).

## Contract

`CallReconciler` accepts only original-call reads, existing dispatch-grant reads, receipt recording and settlement. Its interface has no opening, planning or dispatch-start methods; internal compatibility adapters explicitly refuse them. Provider replay rejects any locally fresh send grant. Cleanup neither waits for transport retry nor creates a missing operation.

The original run, stage, attempt, kind, provider, request/policy digest, logical identity, retry policy, opening key and timestamp must match. Physical order and scope are checked. Only bounded LLM and Similarity provider operations are admitted.

| Existing boundary | Terminal cleanup |
| --- | --- |
| Missing operation | Return not found, create nothing |
| Empty OPEN | Record NO_DISPATCH without planning |
| Unsent PREPARED | Release reservations and close |
| Completed retryable failure | Retain failure, release unstarted retries |
| DISPATCHING/SENT with valid receipt | Restore publication, verify original response and settle |
| Uncertain send with confirmed absence of receipt | Record UNKNOWN and consume conservative bounds |
| Receipt read/validation error | Preserve state and reservations, return error |
| Terminal operation | Replay the existing outcome and trace |

`LLMCalls.Reconcile`, `SimilarityCalls.Reconcile` and `StructuredLLMCalls.Reconcile` use this boundary. Structured cleanup restores an already-opened format repair, but a diagnostic alone cannot start one. It checks the exact repair parent's existence first, preserving missing-artifact errors for repairs that already exist.

## SQLite corrections

Failure-first tests reproduced an OPEN provider call stranded by cancellation before planning. Its NO_DISPATCH finish now remains possible after pending cancellation, while proving zero physical rows and retaining expected-version/current-attempt checks. New calls, cache reuse, foreign attempts and forged dispatch remain rejected.

A second failure-first case covered an OPEN private response child. After its exact provider parent is terminal, an empty child closes without creating declarations or reservations. The general `LoadCall` contract still rejects unprepared bundles.

Cancellation and interruption previously released the only SEALED receipt while its provider parent remained unresolved. Both now preflight the closed private-response families before changing writers, pins, budgets or projections. Unresolved parents reject the transition atomically. After original receipt reconciliation, release succeeds and published bytes retain their accounting.

## Verification

- Narrow ledger and receipt facades exercise missing/OPEN/PREPARED calls, existing dispatch, known failure, unknown boundary and unreadable receipts without planning, dispatch-start or backoff.
- Local HTTP counts remain zero for unstarted work and one for original receipt recovery. Changed bindings fail before recovery; terminal replay preserves traces.
- Pending cancellation covers partially prepared local slots and SEALED/complete/terminal provider receipts for both LLM and Similarity.
- Structured cleanup retains one rejected original receipt without starting its unopened repair; an existing successful repair replays both responses. Missing existing repair evidence remains an error.
- SQLite cancellation/interruption refuses to discard an unresolved parent's SEALED writer, preserves the RUNNING version, then permits cleanup after original replay.

Complete normal tests and vet pass (application 67.329 s, SQLite 34.851 s, integration 8.231 s). Linux amd64 compilation with CGO disabled passes. Architecture consistency passes for 26 normative files; formatting passes for 297 Go files and patch whitespace is clean. Complete race verification passes with the existing 20-minute package timeout (application 836.984 s, SQLite 338.206 s, integration 105.096 s). This sweep compiled before the following explicit-workflow configuration work.

## Remaining integration

The real run service must call reconciliation before terminal cancellation, exhausted active-time handling or interruption releases provider resources. Ordinary same-attempt continuation may authorize later configured work after original receipts are restored; terminal cleanup cannot. Live selection, checkpoint routing and later verification gates remain separate work. No paid provider or Docker invocation is included.
