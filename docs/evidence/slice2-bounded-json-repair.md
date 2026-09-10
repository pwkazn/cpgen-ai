# Slice 2 bounded JSON-format repair

Date: 2026-09-08. Branch: `codex/phase2`. Base commit: `50deb38599b1d825ea15b7bcf1140882b32ba7e8`.

Status: **PASS for LLM-04**, including full repository gates. This extends the [private response replay checkpoint](slice2-private-llm-replay.md). It does not enable real-provider CLI execution or complete Slice 2.

## Implementation

- `PhysicalLLMResult.FormatRepair` carries only allowlisted local format-error codes. The strict schema registry distinguishes domain-semantic rejection from typed JSON decoding failures. Provider bodies, field paths, SDK errors and rejected output fragments never become repair input.
- Eligible failures publish bounded, private `cpgen.llm-validation/v1` receipts through the existing durable writer protocol. Receipts retain accounting and a request/physical-call binding, without raw invalid output. `ReadFormatRepair` verifies both the Blob and its correspondence to the terminal physical failure.
- `NewStructuredLLMCalls` permits zero or one repair. It preflights the original and repair prompt/schema bindings and incorporates the policy, exact prompt version and implementation revision into the original call's immutable policy digest before any dispatch. Changing the policy on resume fails without another request.
- A repair is a separate deterministic logical call. Its transport attempts retain the existing retry policy, budgets and unknown-send handling. There is no recursive repair. The result retains both logical traces and both private artifacts and separately aggregates settled usage, including the original failure.
- `llm.max_format_repairs` defaults to zero, accepts only integer 0 or 1 and participates in the effective policy digest when enabled. The zero default preserves older snapshots. `BuildFormatRepairPolicy` selects compiled Idea/Statement repair prompts with the original output validators.

## Failure-first and focused verification

The initial wrapper tests failed to compile because the structured service and policy did not exist. Configuration tests then rejected the new field and failed on missing policy mapping before implementation. An initial repair idempotency prefix failed the domain ID contract during preflight with zero HTTP requests; the corrected deterministic identifier passes.

Tests use real SQLite, private filesystem storage and local HTTP only:

- One repaired success, a second rejection, disabled repair, changed policy, absent prompt and unbounded allowance. Repeating the whole operation never adds HTTP requests.
- The repair body contains original input and safe error codes; invalid response text and private field names are absent from diagnostics and validation receipts.
- Authentication, malformed envelope, truncation, filtering and domain rejection do not enable repair. A repair send with a disconnected response remains UNKNOWN after recovery and is not repeated.
- Budget rejection before the repair sends nothing. Cancellation between calls preserves paid usage and evidence; explicit same-attempt resume can start only the unstarted repair.
- Reopening SQLite preserves both results. An injected stage-version conflict leaves both artifacts available; the successful atomic retry creates two occurrences, releases all reservations and adds no HTTP calls.
- A real subprocess exits after the validation receipt seals, between calls, before the repair receipt seals, after it seals and after physical completion. Two recoveries keep total HTTP calls at two. Unsealed repair bytes remain UNKNOWN; sealed receipts restore the validated result and aggregate usage.

## Repository gates

Go 1.26.5, Windows amd64; host test execution is required for Go cache and temporary-directory access in this environment.

| Gate | Result |
| --- | --- |
| `go test ./...` | PASS: 22 tested packages, 3 without tests |
| `go vet ./...` | PASS |
| `go test -race ./...` | PASS: application 338.6 s, SQLite 221.5 s |
| Linux amd64, CGO disabled, `go build ./...` | PASS |
| Architecture consistency script | PASS: 26 normative files |
| Formatting | PASS: 249 Go files |
| Tracked and new-file whitespace checks | PASS: 35 new files checked separately |

No paid provider or Docker smoke is part of this test set. No commit has been created.

## Remaining integration

Cache provenance, graph assembly and real stage/CLI composition remain LLM-05, WF-01–WF-05 and LLM-06. These tests recover calls within their original attempt; workflow-level interrupted-attempt reconciliation is a separate integration boundary. Business-content repair and package readiness remain governed by their existing workflow budgets and gates.
