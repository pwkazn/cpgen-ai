# Slice 2 compiled application graph

Date: 2026-09-08. Branch: `codex/phase2`. Base commit: `50deb38599b1d825ea15b7bcf1140882b32ba7e8`.

Status: PASS for WF-01. The CLI continues to use deterministic Slice 1 stages. Real Slice 2 dependencies and verified stage-output recovery remain WF-02–WF-05 / LLM-06.

## Implementation

- `compiledRunGraph` assembles only the two compiled Slice 1 and Slice 2 stage sequences with LangGraphGo in `internal/application`. One conditional edge selects one next node. It installs no library retries, state merger, tracing, callbacks, checkpoint store or library resume configuration.
- `LocalRunService` now executes its existing typed Slice 1 stage methods through the graph. Existing run locks, active accounting, cancel pollers, dependency revalidation, review application, reconciliation and checked stage commits stay in the foreground lifecycle.
- Graph state contains a private invocation frame with the current run projection and the application-owned stage callback. Provider prompts, model responses and typed content are not graph checkpoints. Stage values still flow through the existing concrete workflow methods.
- Each node checks cancellation before invoking the stage. Only a successful checked commit with a newer run version and the exact compiled successor can advance. Review, block, failure and cancellation end invocation at the current stage. Neither compiled slice permits READY.
- Resume checks workflow/schema/digest, current stage/ordinal and the complete persisted stage sequence before recovery, review application or stage work. `RuntimeStore.StageSequence` reads ordered selectors and validates stage-to-run revision bindings. Unknown, reordered, extended or incompatible definitions are rejected before mutation.
- The graph retains the last checked projection when the library returns an empty result on an ordinary error. A failed `FinishStage` likewise returns the last known run projection, so the caller can identify the current attempt without treating an empty snapshot as progress.
- Interrupted stages may use CREATED at any compatible current stage. Resume starts there; it never infers first-stage execution from CREATED alone.

## Failure-first evidence

1. The first compiled graph tests failed because the constructor was absent.
2. Integration tests then showed that unknown workflow revisions and wrong workflow digests could execute the existing Fake stages. A wrong initial stage could mutate the run before failing.
3. A simulated stage commit failure returned a zero run snapshot. The corrected service and graph retain the current stage and error without retry.
4. Tests for a reordered later stage or an extra stage showed that checking only the current selector was insufficient. Full persisted-sequence validation now rejects both before starting the first attempt.

## Focused verification

- Both compiled sequences execute serially from every current-stage entry and stop at explicit review boundaries.
- Interrupted CREATED and BLOCKED entries resume the current stage.
- Commit failures run once, preserve the last known projection and prevent downstream execution.
- Cancellation before invocation performs no stage work; cancellation after a committed stage prevents the next node.
- Invalid stage jumps, loops, unchanged versions, changed run/input/config bindings and premature READY are rejected.
- Concurrent graph invocations retain separate progress frames.
- SQLite integration resumes after a failed Exercise commit without repeating the committed Prepare stage.
- Existing Slice 1 process, lifecycle, lock and crash regressions pass the focused run. These tests use deterministic fixtures, not real provider dispatch or new Docker acceptance.

## Repository gates

- Full `go test ./...` and `go vet ./...`: PASS (22 tested packages and three without tests).
- Full `go test -race ./...`: PASS; application 426.517 s, SQLite 236.146 s, integration 83.835 s.
- Linux amd64 `CGO_ENABLED=0 go build ./...`: PASS.
- Architecture: PASS for 26 normative documents. Formatting: PASS for 258 Go files at the graph checkpoint. Tracked patch and 47 new-file whitespace checks: PASS.

No paid provider call, new real-Docker smoke or commit was made. Subsequent content-draft and real-stage work has separate verification.
