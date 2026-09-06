# Task 9 fix round 3

## Scope

- Verify the run exists before reading a pending review. This preserves the
  existing `OK`/`null` response for an existing run without a pending review,
  while mapping a well-formed but nonexistent run ID to typed `not_found`.
- Add a CLI regression test covering the required exit code 3 and JSON error
  envelope.

## Verification

- `gofmt -w internal/cli/run.go internal/cli/run_test.go` — PASS.
- `go test ./internal/cli ./internal/application ./cmd/cpgen -count=1` — PASS.
- `go test ./... -count=1` — PASS.
- `go vet ./...` — PASS.
- `pwsh -NoProfile -File scripts/check-slice1-architecture.ps1` — PASS.
- `git diff --check` — PASS.

Commit: this commit (`fix: return not-found for missing reviews`)
