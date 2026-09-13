# Slice 2 strict content drafts

Date: 2026-09-08. Branch: `codex/phase2`. Base commit: `50deb38599b1d825ea15b7bcf1140882b32ba7e8`.

Status: PASS for LLM-06a. This is a provider-content prerequisite, not real CLI stage acceptance.

## Implementation

- `IdeaDraftV1` and `StatementDraftV1` are strict, versioned content proposals. They have no provider-controlled domain IDs, hashes, resource limits, revisions, selection bindings or mutation lineage. Exact-field JSON decoding and semantic validation reject forged or malformed fields.
- `IdeaDraftInputV1` binds a validated request snapshot, an admitted count of 2-8 candidates and locally derived seed axes for each slot. Binding a draft creates the initial `IdeaBatch` with existing deterministic constructors, frozen budgets and request constraints. Provider proposals must match the admitted count.
- `StatementDraftInputV1` contains the resolved and validated snapshot/batch/selection/input chain. Binding a draft creates `ProblemSpec` locally with the trusted revision, identities, constraints, algorithm and complexity. Sample input/output bytes are preserved exactly.
- Built-in `idea.draft` and `statement.draft` prompts and their bounded format-repair prompts select these schemas. Historical full-domain prompts and validators retain their original identities for receipt compatibility. `BuildLLMDraftPrompt` exposes only the two compiled draft choices.
- Slice 2 RunView now uses the persisted request schema (`cpgen.request/v1`), while its typed Idea input keeps the distinct snapshot schema. Both ordinary execution and revalidation reject an incompatible run schema.

## Failure-first and focused verification

- Initial tests failed because draft constructors and decoders were absent.
- Registry tests failed before draft prompt/schema and repair registration was added.
- The persisted RunView schema test reproduced the request-versus-snapshot mismatch before it was corrected.
- Tests reject identity/limit overrides, duplicate and unknown fields, case aliases, missing content, invalid feasibility states, candidate-count changes and altered seed axes.
- Local binding is deterministic, copies mutable collections and preserves sample bytes. A substituted selection chain cannot produce a statement.
- Local HTTP tests use the production built-in draft registries through `NewReplayableLLMCalls` and `NewStructuredLLMCalls`. Each stage receives one invalid-format response followed by one valid draft. Both calls, private artifacts and all six input/eight output usage tokens are retained. Reconstructing the structured wrapper and replaying keeps the HTTP count at two, then the same draft binds into the validated domain chain.

## Remaining scope

Model feasibility assertions remain proposals; they do not constitute Judge, resource or originality evidence. Mutation and solution repair need their own authorized policies. The real factory, committed-stage content reader, interrupted-call recovery and final quality/package gates remain open. No paid provider request, Docker smoke or commit is included.

## Repository gates

- Full `go test ./...` and `go vet ./...`: PASS (22 tested packages and three without tests).
- Full `go test -race ./...`: PASS; application 428.018 s, SQLite 224.597 s, integration 86.116 s.
- Linux amd64 `CGO_ENABLED=0 go build ./...`: PASS.
- Architecture check: PASS for 26 normative documents. Formatting, tracked patch and new-file whitespace checks: PASS.

These gates cover the completed draft implementation. The subsequently added failure-first heartbeat/ledger regression belongs to WF-04a and is not claimed as passing in this checkpoint.
