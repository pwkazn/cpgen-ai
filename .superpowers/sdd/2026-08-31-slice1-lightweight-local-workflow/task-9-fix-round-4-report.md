# Task 9 fix round 4

## Scope

- Preserve the SQLite reader default only when `sqlite.max_readers` is
  omitted. Explicit zero, negative, and null values now reach validation and
  produce the field-qualified `sqlite.max_readers` error.
- Verify a run exists before reading its events, preserving an empty event
  result for an existing run while mapping a well-formed nonexistent run ID
  to the typed `not_found` envelope and exit code 3.
- Add configuration and CLI regressions for both contract boundaries.

## Verification

- `gofmt -w internal/config/config.go internal/config/config_test.go internal/cli/run.go internal/cli/run_test.go` — PASS.
- `go test ./internal/config ./internal/cli -count=1` — PASS.
- `go test -race ./internal/config ./internal/application ./internal/cli ./cmd/cpgen -run 'Test(Config|Bootstrap|CLI|Generate|RunCommand|ReviewCommand)' -count=1` — PASS.
- `go test ./... -count=1` — PASS.
- `go test -race ./... -count=1` — PASS.
- `go vet ./...` — PASS.
- `go mod verify` — PASS.
- `pwsh -NoProfile -File scripts/check-slice1-architecture.ps1` — PASS.
- `git diff --check 047a3ed^ HEAD` — PASS.

Commit: this commit (`fix: tighten config and event lookup`)
