# Slice 2 private LLM response replay

Date: 2026-09-08. Branch: `codex/phase2`. Base commit: `50deb38599b1d825ea15b7bcf1140882b32ba7e8`.

Status: **PASS for LLM-03b**, including full repository gates. This extends the [durable dispatch checkpoint](slice2-durable-llm-dispatch.md) in the existing working tree. It is not real-provider CLI or complete Slice 2 acceptance.

## Implementation

- `NewReplayableLLMCalls` reserves private response storage before provider dispatch. A deterministic, separate `LLM_GENERATE` logical operation contains only `LOCAL_ARTIFACT_WRITE` slots. Provider retries retain their original physical identities and call/token/cost accounting. Migration 19 permits this artifact kind without admitting Docker or similarity effects into LLM calls.
- Each successful response becomes a bounded `cpgen.llm-response/v1` receipt under `private/llm/`, using the existing declaration, writer, seal, publication, pin and occurrence protocol. The receipt binds the request, provider physical call, structured output and usage. It excludes credential values and prompt variables; metadata is allowlisted. SQLite stores references, not response bodies.
- Recovery reads the existing writer and verified Blob, validates the complete request binding and reruns the provider's local strict schema validator. SEALED receipts can finish publication; FINALIZED receipts can finish provider accounting. An unsealed interrupted send remains UNKNOWN and never receives new send authority.
- A publication error after durable sealing preserves the pending call and its reservations for local reconciliation. This is a private coordinator error path, not a provider retry. Missing or corrupt completed responses fail closed.
- `GenerateResponse.RawBlob` returns pending occurrence evidence for the existing atomic stage finish. Failed stage commits leave the receipt available. Unused or abandoned unsealed slots release their byte reservations; finalized receipts remain pinned and reserved until occurrence attachment.
- The ledger-only `NewLLMCalls` constructor remains available for existing dispatch tests and still fails closed on successful replay. Production assembly must select the replayable constructor and hold both the run lock and shared artifact-maintenance lock. The CLI remains Fake-only.

## Defects exposed by failure-first tests

1. No replayable constructor existed. The first compilation of `TestLLMReplay` failed on `NewReplayableLLMCalls`.
2. Unused response slots held byte budget after successful stage attachment. Added explicit release of unwritten artifact reservations.
3. Migration 6's writer state trigger permitted early release, while its CHECK constraints required a seal timestamp even for abandoned PREPARED/OPEN writers. A 429 response therefore interrupted transport retry. Migration 20 rebuilds the writer table, preserving references and actual timestamps, and permits those early releases.
4. Dropping/recreating a referenced SQLite table left its deferred violation counter populated. The migration now checks the actual foreign-key graph before clearing the temporary counter. A populated M19 upgrade test checks retained occurrences, writer identities, foreign keys and restored connection settings. Historical migrations remain unchanged. SQLite's documented table-rebuild procedure and [foreign-key behavior](https://www.sqlite.org/foreignkeys.html) informed these checks.
5. A sealed publication error was being converted into terminal UNKNOWN, losing usable local recovery evidence. A bounded receipt-reconciliation path now preserves that evidence through repeated failures.
6. The new byte-release method initially relied on its caller to wait for a terminal parent. A negative test exposed that gap; SQLite now requires a terminal LLM parent in the same run/stage/attempt before releasing unsealed slots.

## Focused verification

- `TestLLMReplay*`: real SQLite, private filesystem and local HTTP. Successful replay after reopening storage; stage commit failure/retry; missing/corrupt files; request/privacy bindings; artifact/provider budget rejection; pending cancellation; 429 transport retry; repeated local publication failure.
- `TestLLMReplayProcessCrashBoundaries`: a real child process exits before seal, after seal, after finalization, after MarkSent, after physical completion and after logical completion. Two recovery passes keep the HTTP count at one. Only sealed/finalized receipts recover a value; unsealed bytes do not.
- `TestArtifactDeclarationReplayIsExactAndReaderDoesNotGrantWriter`: identical declaration replay succeeds, changed bindings fail, and read-only inspection creates no token.
- `TestArtifactEarlyReleaseMigrationPreservesReferencedWriters`: actual M19 schema with PREPARED/OPEN writers and a finalized, attached occurrence upgrades without reference loss. Early-release behavior fails before the migration and passes afterward, without inventing a seal.

## Repository gates

Go 1.26.5, Windows amd64. Tests use normal host permissions because the restricted environment denied Go cache and temporary-directory access.

| Gate | Result |
| --- | --- |
| Final `go test ./...` | PASS: 22 tested packages, 3 without tests |
| Final `go vet ./...` | PASS |
| `go test -race ./...` | PASS; SQLite 242.5 s, application 302.2 s |
| Final `go test -race ./internal/application -run TestLLMReplay -count=1` | PASS: 95.7 s, including the final parent-release guard |
| Linux amd64, CGO disabled, `go build ./...` | PASS |
| Architecture consistency script | PASS: 26 normative files |
| Current-tree formatting | PASS: 243 Go files; final changed-file recheck clean |
| Tracked and new-file whitespace checks | PASS: 27 new text files checked separately |

A diagnostic race run with an intentionally shortened 30-second timeout timed out while constructing the next SQLite fixture after all six crash boundaries had passed. Its stack was executing SQLite schema work. The normal full race run and final focused race run both passed; the short diagnostic timeout is not recorded as a passing gate. No code change was made to hide that diagnostic result. No commit was created.

## Remaining work

- Integrate JSON repair and cache/current-run provenance, then real stages and the fixed application graph. Stage-level restart bridging is still a separate workflow task; these tests recover calls within their original stage attempt.
- No paid provider, public similarity service or Docker smoke was run for this change. No package export or READY claim is made.
