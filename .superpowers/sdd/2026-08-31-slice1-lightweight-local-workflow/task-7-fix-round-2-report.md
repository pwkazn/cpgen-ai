# Task 7 fix round 2

This round closes the durable sandbox lifecycle boundaries identified in the
re-review.

- Volume creation now claims a prepared physical call, records dispatch,
  sent, and terminal success/UNKNOWN settlement with a stable completion key.
- Container and volume failures after an external create attempt retain the
  exact returned identity, settle the same physical call as UNKNOWN when it is
  not already terminal, and persist lifecycle UNKNOWN or CLEANUP_PENDING.
- Cleanup can transition in-flight resources to CLEANUP_PENDING after
  cancellation, then records stop/removal evidence for exact-name and label
  ownership before finishing.
- Reconciliation is serialized per run and validates execution/resource
  identity, exact ownership labels, names, and persisted watchdog control
  evidence. In-process watchdog control is backed by an owner-only fsynced
  control file.
- Probe compile and each capability canary receive independent logical and
  SandboxExecution identities. Existing M1-M15 migration bytes were not
  changed.

Verification:

- `go test -race -tags=cpgen_slice0_probe ./internal/adapter/sandbox/docker ./internal/probe ./internal/port ./internal/watchdog`
- `go test -tags=cpgen_slice0_probe ./... -count=1`
- `go test ./... -count=1`
- `go vet ./...`
- `git diff --check`
