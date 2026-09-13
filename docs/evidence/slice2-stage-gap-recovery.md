# Cancellation and recovery between committed stages

Date: 2026-09-09 UTC+8. Branch: `codex/phase2`.

Status: WF-04b PASS, 04:22 UTC+8. Normal full tests/vet and the complete race sweep through SIM-02 pass, along with Linux compilation, architecture, formatting and patch checks.

A successful stage commits its successor as PENDING while the run remains RUNNING. If the process stops at that boundary, the successor has no current attempt. The previous cancellation code returned `running cancellation lacks current attempt`; the existing terminal projection also rejected every RUNNING run. A still-live service additionally retained its previous attempt in memory, and Resume tried to interrupt that old identity under the successor's name.

`FinalizeCancel` now admits only the exact additional case: a RUNNING run with closed active-time accounting, a PENDING current stage and no current attempt. It still requires a pending control request, expected version and resource reconciliation, and still rejects a live RUNNING attempt. The transaction cancels the pending stage without creating or rewriting attempt history.

Run-service cancellation and recovery now resolve the latest persisted attempt for the current stage, clear stale predecessor identities and act only on a RUNNING attempt. Missing attempts have a consistent `ErrNotFound` classification while retaining the original SQL cause; other read failures are returned.

The failure-first regressions reproduce direct cancellation and restart with a pending cancel at the commit gap, then cover the same owner retaining its predecessor identity. After the correction, neither path starts the successor, repeats the predecessor or changes committed history. Ordinary Resume after the committed boundary continues from the successor. Existing tests still reject direct terminal finalization of a live attempt.

Focused normal checks pass. The focused race run covers run-graph cancellation/recovery and SQLite finalization (application 57.800 s; SQLite 21.554 s). The additional ordinary-resume case passes in the normal full suite. Complete normal tests/vet pass after the correction (application 39.083 s; SQLite 25.255 s; integration 6.292 s).

The complete race sweep passes with SIM-02 (application 980.753 s, SQLite 400.332 s, integration 120.262 s), including the ordinary-resume regression. This correction is independent of live provider selection. Terminal cleanup of interrupted LLM/Similarity calls still needs to reconcile original provider receipts before releasing stage resources; it remains part of WF-04c.
