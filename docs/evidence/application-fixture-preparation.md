# Application fixture preparation cost

Status: Accepted, 2026-09-09 08:15 UTC+8

The expanded unoptimized application race package passed in 1199.150 seconds. A later overlapping sweep hit the 20-minute cumulative package ceiling while progressing through ordinary commit-error tests; it is recorded as failed, not accepted. Current full-suite instructions allow 30 minutes while retaining all operation-specific deadlines.

A CPU profile of `TestGenerationExecutorCommitsRepairedTypedChainAndReplaysWithoutHTTP` attributed 2.30 of 5.81 sampled seconds (39.59%) to SQLite migration. Running the same case five times with race instrumentation took 29.291 seconds before the fixture change and 16.725 seconds afterward, a 42.9% reduction in this local sample. This is test preparation evidence, not a production performance benchmark.

`openFreshApplicationSQLite` now creates an empty fully migrated database once per test process, closes its final connection, keeps immutable main-file bytes in memory and creates an independent copy for each business fixture. The copy requires O_EXCL and is then opened through the real store's schema/connection checks. The template contains no runs or business records. Existing-database/reopen paths, production Bootstrap and all SQLite migration tests remain unchanged.

The isolation regression creates the same RunID in two independent copies, commits cancellation in only one, rejects an attempted overwrite, then verifies ordinary database reopening retains that cancellation. Focused ordinary and race checks pass.

Complete verification passed on Windows/amd64 with Go 1.26.5: `go test ./...` (application 30.888 s; SQLite 26.309 s), `go vet ./...` and `go test -race -timeout 30m ./...` (application 327.817 s; SQLite 447.355 s; CLI 19.581 s). The application package previously took 1132.195 s in the accepted atomic-completion/mutation-prompt sweep. These are local suite observations with different scheduling and supplementary coverage, not a controlled production benchmark. Linux command compilation and architecture/format/patch checks also passed. This checkpoint precedes the typed IdeaBatch output publisher, which receives separate acceptance.

The profile and test binary are local ignored diagnostics under `.tmp/`; they contain fixture activity and are not release artifacts.
