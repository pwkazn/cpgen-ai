# Task 9 fix round 2

## Scope

- Pin `go.yaml.in/yaml/v3` to the required `v3.0.3` release and record its
  verified module and `go.mod` checksums.
- Restrict `ErrCleanupPending` classification to a matching `RunID` with
  explicit pending execution/resource rows or complete manual-cleanup
  blocker evidence. Reconciliation failures and incomplete zero-value
  reports now retain a generic operation error.
- Add application regression tests for zero-value reports, mismatched runs,
  missing rows, explicit pending rows, and manual blockers.

## Verification

- `go mod download -json go.yaml.in/yaml/v3@v3.0.3` with
  `GOPROXY=https://goproxy.cn,direct` — PASS; module and checksums resolved.
- `go mod tidy` — PASS.
- `go mod verify` — PASS.
- `go test ./internal/application ./internal/cli ./cmd/cpgen -count=1` — PASS.
- `go test -race ./internal/config ./internal/application ./internal/cli ./cmd/cpgen -run 'Test(Config|Bootstrap|CLI|Generate|RunCommand|ReviewCommand|CleanupEvidence|Reconcile)' -count=1` — PASS.
- `go test -race ./... -count=1` — PASS.
- `go vet ./...` — PASS.
- `pwsh -NoProfile -File scripts/check-slice1-architecture.ps1` — PASS.
- `git diff --check` — PASS.
- `go build ./cmd/cpgen ./cmd/diffcheck` — not runnable because this
  checkout has no `cmd/diffcheck` directory; `cmd/cpgen` remains covered by
  the focused and full tests.

Commit: this commit (`fix: tighten local workflow CLI errors`)
