# Slice 2 provider configuration and durable dispatch checkpoint

Date: 2026-09-08

Scope: LLM-02 and the first part of LLM-03, built on the existing uncommitted library integration work in `codex/phase2`, base commit `50deb38599b1d825ea15b7bcf1140882b32ba7e8`. This is an incremental development checkpoint, not completed Slice 2 or real-provider CLI acceptance. Changes remain in the working tree.

## Implementation

- Optional strict `llm` configuration and an application-level LangChainGo configuration bridge; absent LLM settings preserve previous effective snapshot bytes. Credentials remain environment names, resolved only at dispatch.
- Provider-neutral `PhysicalLLM` planning and single-request execution. The existing endpoint, prompt and strict schema admission checks are shared with the standalone adapter. Library and HTTP automatic retries remain disabled.
- `application.LLMCalls` uses CallCoordinator and the existing SQLite ledger to reserve bounded physical attempts, authorize sends, settle failures/successes and return database CallTrace. Failed or missing-usage responses keep conservative accounting. Successful logical response usage includes prior failed requests.
- Only fresh dispatch authorization permits a send. Interrupted DISPATCHING/SENT calls resolve conservatively, and completed success replay stops explicitly instead of calling the provider again.
- Post-dispatch settlement survives caller cancellation within five seconds. Retry waits honor the provider's bounded Retry-After value as well as the persisted backoff.

## Failure-first checks

`go test ./internal/agent -run TestPhysicalGeneration -count=1` initially failed because PlanGenerate and GeneratePhysical did not exist. `go test ./internal/application -run TestDurableLLM -count=1` then failed because NewLLMCalls did not exist. Both focused tests passed after implementation.

## Independent acceptance

An independent subagent added `internal/agent/physical_acceptance_test.go` and `internal/application/llm_calls_acceptance_test.go`, using synthetic provider data, local HTTP and real SQLite. The first acceptance run failed and identified:

1. Missing or version-mismatched schema registrations admitted a paid send. A new exact schema binding check now runs before planning/dispatch.
2. Provider `Retry-After: 7` was truncated to the local two-second backoff default. Provider delay parsing now has a separate one-minute safety cap; the coordinator honors the later deadline.
3. A cancellation request recorded after a successful HTTP response advanced the run version and blocked MarkSent/settlement, leaving reservations open. Receipt-only operations now accept exactly the one-version advance caused by a matching pending cancellation, while preserving the running stage/attempt checks. New effect authorization remains strict.

Acceptance also exposed invalid UTF-8 envelopes being eligible for verified token usage; usage extraction now requires valid encoding. The independent subagent confirmed that case passes. After these fixes, `go test ./internal/agent ./internal/application -run 'TestPhysicalAcceptance|TestLLMCallsAcceptance' -count=1` passed.

Final independent acceptance: **PASS for this checkpoint**, on Go 1.26.5 / Windows amd64. The subagent additionally tested ordinary stale versions, replaced current attempts and new dispatch during pending cancellation; all remain rejected with unchanged call/reservation projections. No production defect remains in the tested scope.

| Gate | Result |
| --- | --- |
| Configuration/application mapping focused tests | PASS |
| Physical/provider and durable-call acceptance suites | PASS after the fixes above |
| Narrow cancellation-authority negative tests | PASS |
| `go test -json ./...` | PASS: 22 tested packages; 3 packages with no test files |
| `go vet ./...` | PASS |
| `go test -race ./internal/agent ./internal/application ./internal/port ./internal/config ./internal/adapter/storage/sqlite` with CGO enabled | PASS |
| Linux amd64, CGO disabled: `go build ./...` | PASS |
| `scripts/check-slice1-architecture.ps1` | PASS: 26 normative files |
| `gofmt -l` on current-tree tracked and non-ignored untracked Go files | PASS: 237 files |
| `git diff --check` plus no-index whitespace checks for untracked files | PASS |

The subagent used ordinary host permissions after the restricted environment denied Go cache writes. An initial recursive formatting inspection included ignored nested worktrees; the final gate explicitly selected current-tree files with `git ls-files --cached --others --exclude-standard`. Existing ignored worktrees and generated tasks were not edited. Real Docker canary was skipped and the `cpgen_slice0_probe` build-tag suite was not run; this patch changes no Docker implementation. No new commit was created.

## Remaining boundaries

- LLM-03 remains open until successful responses are published and replayed through verified private Blob artifacts with the existing occurrence/budget protocols. Current success replay returns `ErrLLMReplayUnavailable` without network I/O.
- Configuration is accepted and can construct the provider policy, but Bootstrap remains Fake-only. Real Idea/Statement/Similarity assembly, JSON repair, cache integration and the production LangGraphGo graph remain pending.
- Cost settlement uses an explicit per-request ceiling passed to NewLLMCalls; no provider price or actual billed amount is inferred.
- Tests use local HTTP fixtures and real SQLite. No paid model, public similarity or real Docker smoke is part of this checkpoint. Existing task artifacts and historical Slice 0/1 evidence are preserved.
