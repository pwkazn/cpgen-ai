# Slice 2 committed generation inputs

Date: 2026-09-09 UTC+8. Branch: `codex/phase2`. Base commit: `50deb38599b1d825ea15b7bcf1140882b32ba7e8`.

Status: WF-03a PASS at 01:12 UTC+8. Real application stage selection remains open.

## Implementation

- `ReadGenerationSnapshot` restores the canonical submitted generation request and the creation-time effective seed in one database read. It verifies the request hash, exact canonical bytes, admitted schema/mode and explicit-seed binding. It preserves submitted whitespace, Unicode, nil/empty collections and signed 64-bit seeds without normalization or floating-point conversion.
- Random/v1 requests with an empty brief now roundtrip through RunRequest and CreateRun. Other modes and unsupported schemas retain their brief requirement. Slice 1 separately validates its typed input before creating a run, so unsupported empty input cannot leave a RUNNING record behind.
- `ReadCommittedLLMStage` requires a currently SUCCEEDED stage and its matching latest successful attempt, input, output and workflow/schema bindings. It reads the stage and occurrences in a read-only SQLite transaction. A historical successful attempt alone cannot resurrect an invalidated stage.
- Each private receipt occurrence is checked against its retained finalized writer, READY Blob, immutable provenance, terminal local producing call and terminal provider call. Cache uses also require the current cache-hit trace and reuse record. Returned metadata distinguishes current occurrence/call identity from the original producer and contains no writer or dispatch capability.
- `StructuredLLMCalls.ReadCommitted` restores an attached original or one bounded repair through the existing verified Blob and physical-receipt checks. It performs no logical opening, provider planning mutation, dispatch or settlement. Original diagnostic reads require terminal local producing calls before entering replay.
- `GenerationReader` reconstructs the original provider plan from frozen draft input, prompt/schema, sampling, output limit and provider/repair policy plus the source attempt's immutable call identities. Only an exact plan binding whose original-or-repair result matches the committed provider source can supply content. Private bytes are verified before strict draft decoding.
- IdeaBatch and ProblemSpec are rebuilt with existing deterministic constructors and checked against their committed semantic output digests. The deterministic selection also must match the Statement input digest committed by Idea. Statement reconstruction validates the full request/batch/selection chain and trusted revision.

## Failure-first and focused verification

- A valid random request originally failed RunRequest conversion with `run request has an empty required field`. Domain roundtrip and storage tests now preserve its exact submitted JSON.
- The generation snapshot, stage artifact and committed-response reader tests initially failed to compile before their interfaces were implemented.
- After broadening domain admission, a failure-first Slice 1 test found that unsupported empty typed input created a RUNNING run before failing. Preflight now rejects it before persistence.
- Snapshot tests close/reopen SQLite, preserve positive and negative seeds beyond 2^53, prove reads start no attempts/calls and reject altered request bytes, incompatible schemas, explicit seed drift and legacy non-generation modes.
- Stage-reader tests reject running stages, unattached receipts, substituted output digests and invalidated prior success. Private original/repair calls remain distinct, and cache source/current-attempt identities stay separate. Committed reuse survives later cache-index invalidation.
- Local HTTP tests commit real built-in Idea and Statement drafts with one format repair each. Both semantic outputs, selection and downstream input restore without additional HTTP, including after closing/reopening SQLite and rebuilding reader services.
- Changed candidate count, selection policy, provider policy, output token limit or statement revision cannot reinterpret committed output. Missing private bytes and substituted input/output/source proof are rejected without generating replacement content.
- A real ReviewRevise transaction invalidates Idea, Statement and Similarity. Reads immediately reject old output. A fresh Idea attempt then commits a same-run cache reuse at zero additional HTTP cost; the reader restores that current result through its original repaired response, while the invalidated old Statement remains unavailable. The mutable cache index is invalidated before the successful read.

## Repository gates

Related domain, port, SQLite, application, workflow and CLI package tests pass. Repository verification:

- `go test ./...` and `go vet ./...`: PASS, 22 tested packages and three without tests.
- `go test -race ./...`: PASS. The first run passed application (563.344 s), SQLite (314.197 s), integration (85.654 s) and all other packages except one Windows pipe test. Normal and race processes used the same machine-wide pipe name. Both pipe-opening tests now generate independent nonces; the same-nonce refusal assertion is unchanged. Two concurrent processes each passed 50 repetitions, and the full race rerun passed with unchanged packages cached.
- Linux `CGO_ENABLED=0` build, architecture consistency (26 documents), formatting (275 Go files), tracked and new-file whitespace checks: PASS.

## Remaining scope

This reader reconstructs the initial Idea batch and a trusted Statement revision. New mutation lineage and content repair remain separately authorized business transitions. Model feasibility claims do not replace Judge, resource, originality or quality evidence. WF-02/WF-04/WF-05 must compose these readers with actual stage execution, active-time accounting and recovery before real CLI acceptance. No paid provider request, Docker smoke, commit or READY path is included.
