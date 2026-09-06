# Task 7 fix round 3 report

Commit: the focused commit that includes this report (reported by the
implementer after commit).

## Scope

This round addresses the remaining Task 7 re-review findings while preserving
the historical M1-M15 migration files byte-for-byte.

## Fixes

- Added forward-only M16 schema migration for sandbox resource scope. Resource
  rows now carry `(run_id, stage_name, attempt_id)` and `physical_call_id` is
  bound to the same physical-call scope with a composite foreign key. The
  previous non-container restriction was removed; volume resources now use the
  same physical-call ledger as containers. Post-dispatch resource phases require
  a persisted physical call identity.
- Wired volume creation through `BeginDispatch`, Docker `Create`, `MarkSent`,
  and `CompletePhysical`, with the physical call ID persisted before dispatch.
- Split cleanup authorization from the new-work RUNNING guard. Stop proof,
  interruption, cleanup-pending, and cleaned settlement remain scoped to the
  exact run/stage/attempt and are now valid after cancellation or interruption,
  without permitting new Create/Start/export work.
- Reconciler volume removal now requires the inspected engine name to match
  both the persisted engine ID and sealed deterministic resource name.
- Replaced fixed year-2100 lifecycle deadlines with the injected/real clock;
  persisted lifecycle timestamps remain the replay source for stable phase
  deadlines.

## Verification

- `go test ./internal/adapter/storage/sqlite -run 'TestSandboxExecution|TestMigration' -count=1`
- `go test ./... -count=1`

The temporary migration fixture test was removed after validating migration of
existing M1-M15 sandbox rows. Final focused, race, tagged, vet, build, and diff
checks are run before commit.
