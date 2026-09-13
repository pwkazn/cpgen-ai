# Task 7 fix round 4 report

Commit: the focused commit that includes this report (reported by the
implementer after commit).

## Scope

This round closes the remaining Task 7 re-review blockers while preserving
the M1-M16 migration files byte-for-byte. The new M17 migration is forward
only and keeps legal legacy volume rows compatible.

## Fixes

- Extended watchdog observation and cleanup ownership to Docker volumes,
  including exact labels, driver, and options; cleanup proof is persisted
  through the existing lifecycle path and planned/creating resources are
  settled when dispatch cannot begin.
- Bound deterministic watchdog control directories and nonces to the full
  sealed execution identity, safely retired stale control artifacts, and
  retained owner-only permissions and fsync behavior on Unix and Windows.
- Added the prepare/commit/start detached-watchdog sequence so lifecycle
  evidence is durable before a detached process starts. Persisted watchdog
  references, file digests, token digests, control envelopes, and lifecycle
  deadlines are verified during reconciliation and replay.
- Made the reconciler's default execution lock process-shared, and moved
  owner-only directory setup behind portable platform implementations so both
  Linux and Windows builds remain supported.
- Added M17 compatibility handling for M14 volume rows with NULL physical
  call IDs without changing historical migration bytes.

## Verification

- `gofmt` on all changed Go files
- `go test ./... -count=1`
- focused watchdog, Docker sandbox, SQLite, and port tests
- race-enabled focused tests
- `go test -tags=cpgen_slice0_probe ...`
- `go vet ./...`
- Linux and Windows `go test -c` cross-builds for the Docker adapter
- `git diff --check`
