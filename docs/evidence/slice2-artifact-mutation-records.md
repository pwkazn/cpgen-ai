# Slice 2 artifact attachment and mutation result ledger

Status: Accepted

Checkpoint: 2026-09-09 07:41 UTC+8. MUT-01b hardens the existing generic ledger; it does not authorize or execute a business mutation route.

## Retained output and metadata

An actual local output fixture reproduced failure when attaching a writer whose physical bytes had already been settled. Attachment now permits exact reuse of that settlement only with the finalized writer, active pin, authoritative byte count, successful physical write and matching receipt digest. The logical call must be terminal. Attachment does not charge bytes or increment its budget account again. Reserved writes retain the original settlement path; released reservations and mismatched receipts are rejected.

Successful diagnostic writes remain retainable when the containing business call failed. This attachment rule does not establish successful business output; later mutation finalization independently checks the selected output operation.

Failure-first tests also reproduced three substituted new-write provenance fields and five substituted cache metadata fields. Forward migration 24 requires new occurrences to retain immutable declaration or cache-entry metadata, including role, media type, logical path and provenance. Earlier migration bytes and existing rows are preserved. Rejected attachment rolls back the occurrence and permits the original command to commit correctly.

## Immutable result evidence

`MutationRecordRequest` rejects duplicate logical operations, duplicate reservations and reservations with no recorded operation. Evidence counts are bounded. `RecordMutation` also verifies the grant's immutable limit; rehashing a changed limit cannot turn it into the original claim. A record requires an actually committed OUTPUT occurrence in the operation scope.

Real subprocess tests terminate before and after record commit, then recover twice in fresh processes. Each retains one original claim, one result record and exact operation/reservation/output references without changing stage progress or charged bytes. This generic method still runs after ordinary attachment. The subsequent atomic-stage checkpoint closes that separate transaction boundary.

## Verification

- Full normal tests and vet pass; application 53.436 s, SQLite reused the passing 23.778 s checkpoint, integration 5.437 s.
- Full race passes: application 1199.150 s, SQLite 369.501 s, integration 90.216 s. This sweep includes migration 24, generic record process tests and candidate collection, but predates the later atomic-stage and result-reader APIs.
- Linux amd64 CGO-disabled command build, the 26-file architecture check, formatting and patch whitespace pass.
- All providers are local fixtures. This checkpoint has no paid-provider, external Similarity or Docker acceptance claim.
