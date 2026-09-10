# Slice 2 atomic mutation completion and result recovery

Status: Accepted

Checkpoint: 2026-09-09 08:06 UTC+8. MUT-01c supplies a storage completion primitive and a read-only result reader. It neither creates mutation claims nor changes the preview graph.

## Atomic completion

`FinishMutationStageCommand` binds an existing grant, the successful stage command, complete current-attempt operations/reservations, and the finalized OUTPUT writer. The writer identifies an occurrence before its database-generated occurrence ID exists. The typed stage's semantic output digest and serialized Blob digest are separately bound; these hashes need not be equal.

`FinishMutationStage` reuses the ordinary stage transaction's version, active-time, attempt, timestamp and pending-cancel checks. Within that transaction it attaches occurrences, resolves the exact new output, inserts the immutable mutation result and advances to the fixed successor. Unique evidence plus authoritative attempt counts prevents omitted operations or reservations; existing ledger constraints require terminal operations and settled/released reservations. The selected output requires both successful physical and logical completion. Earlier failed operations can remain in the evidence set. Cache reuse has no mutation policy yet and is rejected by this command.

The whole composite command is the event's idempotency identity. Exact replay returns the original result after later stage advancement without rewinding it. A changed mutation record or ordinary FinishStage command cannot reuse that identity. The generic RecordMutation API retains its historical command format and can exactly replay the record produced atomically.

## Read-only recovery

`ReadMutationRecord` reconstructs an exact receipt in one SQLite read snapshot. It matches the supplied grant against the stored claim, checks every stored result identity field, rebuilds ordered operation/reservation references, resolves the output reference and validates the original command hash. No committed result means not found. Invalid, incomplete or drifted records return no partial receipt.

Historical metadata remains readable after advancement and database reopening. This reader does not read Blob bytes, authorize dispatch or make invalidated history current generation input.

## Verification in this checkpoint

- Atomic output/record/progress, exact replay after successor start, cancellation, omitted evidence and rollback at record insertion, successor update and final COMMIT pass.
- Real processes exit before and after COMMIT; fresh-process recovery twice proves output, record and stage are either all absent or all committed, with unchanged settled-byte accounting.
- Failure-first tests reproduced a failed output operation being recorded as successful, and a reader overlooking damaged source identity. Both are corrected; diagnostic artifact retention remains separately supported.
- Result-reader tests cover exact generic replay, database reopening, independent returned slices, crossed run/grant, missing child records, broken order, damaged source and command hash.
- Complete normal tests/vet pass: application 60.393 s, SQLite 32.653 s, integration 8.964 s. Complete race passes with a 30-minute cumulative package limit: application 1132.195 s, SQLite 470.252 s, integration 97.732 s. The earlier 20-minute sweep timed out and is not counted as acceptance.
- A supplementary mixed-operation test retains a failed model request plus successful output with five reservations in non-ID order. Exact record recovery/replay passes normal (1.235 s) and race (6.977 s).
- Linux amd64 CGO-disabled command build, 26-file architecture check, formatting and patch whitespace pass. A later test-only empty-schema fixture optimization receives [separate verification](application-fixture-preparation.md).
