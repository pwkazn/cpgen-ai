# Task 7 fix round 5

Base commit: `3665094`

## Changes

- Fixed M16 compatibility parking by dropping the M16 version guard together with the phase guards before updating legacy post-dispatch VOLUME rows. Added an upgrade regression that creates a minimal M14 database with a `DISPATCHING` VOLUME whose `physical_call_id` is `NULL`, upgrades through M17, and verifies the row is preserved.
- Persisted the exact Docker container or volume engine identity before settling the physical create call or notifying the watchdog. Added the explicit `DISPATCHING -> SENT` lifecycle transition needed to preserve the monotone resource state machine.
- Reconciler now settles `CREATING` rows without an engine ID as deterministic no-create `INTERRUPTED` rows. For `DISPATCHING`/`SENT` rows with no ID, it probes only the deterministic name and complete ownership labels; exact matches are durably completed, while missing or mismatched objects become manual cleanup blockers with cleanup pending.
- Windows same-nonce watchdog replay probes the named pipe with `WaitNamedPipeW` before retiring a control directory, refusing active or uncertain states so a live detached watchdog is not disrupted.
- Sealed `CleanupDeadlineUTC` in the watchdog record and mirrored it in the in-process and detached service. Reconciliation and persisted-control validation now use and validate the same absolute cleanup deadline.

## Verification

Passed:

- `go test ./internal/adapter/storage/sqlite ./internal/adapter/sandbox/docker ./internal/watchdog ./internal/port -count=1`
- `go test -tags cpgen_slice0_probe ./internal/adapter/sandbox/docker ./internal/probe ./internal/watchdog -count=1`
- `go test -race ./internal/adapter/sandbox/docker ./internal/watchdog -count=1`
- `go test ./... -count=1`
- `go vet ./...`
- `go build ./cmd/cpgen`
- `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./...`
- `GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./...`
- `GOOS=windows GOARCH=amd64 go test ./internal/adapter/sandbox/docker ./internal/watchdog -run '^$'`
- `git diff --check`

The repository currently has no `cmd/diffcheck` directory, so the AGENTS.md example command `go build ./cmd/cpgen ./cmd/diffcheck` cannot resolve that target; `go build ./cmd/cpgen` succeeds.
