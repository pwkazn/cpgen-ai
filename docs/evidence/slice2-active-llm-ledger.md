# Slice 2 active-time and LLM ledger bridge

Date: 2026-09-09 UTC+8. Branch: `codex/phase2`. Base commit: `50deb38599b1d825ea15b7bcf1140882b32ba7e8`.

Status: PASS for WF-04a, completed at 00:18 UTC+8. This is a prerequisite for real stage assembly.

## Implementation

- `RunBoundLLMLedger` binds ledger mutations to one RunID, stage and attempt. Every transition checks that this is still the current RUNNING attempt, reads the current optimistic run version and invokes the existing storage guard. At most eight database attempts retry `ErrVersionConflict`; other errors and cancellation stop immediately. No provider operation is retried by this bridge.
- M22 adds immutable original opening and completion command receipts. Each receipt is committed in the same transaction as its logical-call change, binds the existing command digest and has a 16 KiB bound. Readback verifies raw and canonical hashes, IDs and typed validity. The receipts contain command metadata, not prompts or credentials.
- Opening or completing the same logical call after a heartbeat restores its exact persisted command. Every field except the caller's optimistic version must still match. Historical calls without these receipts are adopted only when the exact historical command is supplied; their previous versions are never guessed. Ordinary storage APIs retain their strict historical behavior.
- Private response publication, physical completion and artifact-slot release refresh only their transition version. Dispatch grant identity, fresh-send authority, fixed retry plans, reservation amounts and cancellation guards remain in the existing ledger. Receipt reads do not grant provider dispatch.

## Failure-first evidence and focused tests

- A local HTTP response followed by an active-time heartbeat originally failed private publication with `version_conflict: metering expected run version does not match`. The response now settles once with its verified usage and private artifact.
- A private cache hit followed by a heartbeat originally failed repeated logical completion with `logical completion idempotency was reused with different content`. Opening and completion metadata now preserve the original command and return the same trace and reuse record without a provider call.
- The tests cover one JSON repair, a 429 transport retry, an uncertain repair send, persisted cancellation after a response, wrapper reconstruction and database close/reopen after multiple heartbeats. Replay retains exact traces, artifacts and usage, with no additional HTTP requests.
- A deterministic race fixture inserts a real database heartbeat immediately before each of opening, planning, dispatch, sent recording, physical completion, logical completion and unwritten-slot release. Every forced conflict retries its database operation while the provider HTTP count stays one.
- Changed run/stage/attempt, input/policy digest or opening timestamp is rejected. An interrupted or replaced stage attempt cannot acquire authority from a newer run version. Conflict retries stop at eight; other storage errors stop at one; a cancelled caller starts none.
- Receipt insertion failures roll back logical opening or completion. SQLite prevents receipt updates/deletes; readback also rejects a deliberately corrupted completion receipt. Historical database migration and exact original-command adoption pass.

## Repository gates

- Full `go test ./...` and `go vet ./...`: PASS (22 tested packages and three without tests).
- Full `go test -race ./...`: PASS; application 496.943 s, SQLite 258.584 s, integration 84.066 s.
- Linux amd64 `CGO_ENABLED=0 go build ./...`: PASS.
- Architecture check: PASS for 26 normative documents. Go formatting (266 files), tracked patch and new-file whitespace checks: PASS.

## Remaining scope

The real application factory has not selected this ledger for live Idea/Statement CLI stages yet. Current-stage typed input recovery and interrupted-attempt orchestration remain WF-03a/WF-04. A heartbeat-safe call bridge does not itself authorize a new attempt after an unknown external send. No paid provider request, Docker smoke or commit is included, and no new READY path is introduced.
