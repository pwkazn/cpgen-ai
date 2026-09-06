# Task 7 fix round 1

Implemented the critical lifecycle, cleanup-proof, replay-identity, and sealed-identity fixes requested by the Task 7 review.

- Docker container creation now performs durable `BeginDispatch`, external create, `MarkSent`, and `CompletePhysical`; create errors settle the same physical call as `UNKNOWN` and the sandbox resource as `UNKNOWN`.
- Cleanup terminal states are reachable only through named persisted stop-proof, removal-evidence, or no-create interruption commands. SQLite migration M15 adds append-only evidence columns/triggers and reconciliation evidence.
- Lifecycle command IDs and timestamps are derived from sealed execution/resource identity; watchdog detached control references and probe SandboxExecutionIDs are stable per logical operation.
- Production sandbox/probe/watchdog identities and labels no longer contain `OwnerID` or `LeaseEpoch`; resource and execution identity/labels are immutable after persistence.

Evidence:

- `go test ./...`
- `go test -tags cpgen_slice0_probe ./internal/adapter/sandbox/docker ./internal/probe ./internal/port ./internal/watchdog`
