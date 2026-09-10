# BR-01a: initial IdeaBatch typed output publication

Status: Accepted, 2026-09-09 08:50 UTC+8

## Implemented boundary

`GenerationExecutor.CollectIdeaCandidatesWithOutput` composes the existing durable initial candidate collector with a separately budgeted local publisher. It preserves the raw original/repair responses as EVIDENCE and appends canonical IdeaBatch bytes as OUTPUT. The semantic batch digest and canonical Blob hash are deliberately separate identities. The publication request binds the exact snapshot, batch, Blob, successful current-attempt provider operation and provider request digest.

The local operation is an application-owned `LLM_GENERATE` / `private-blob` child with the closed `idea-batch-output:` identity family. It uses the existing writer, reservation, seal, finalization and receipt protocols. The SQLite parent guard recognizes this family, and ordinary stage attachment completes its physical/logical receipt in the same transaction as byte settlement and occurrence attachment. Existing response constructors still default to EVIDENCE. No migration or historical workflow definition changes.

## Verified behavior

- Original and repaired responses produce two and three occurrences respectively, with exactly one typed OUTPUT. Canonical bytes verify through the Blob store and strict IdeaBatch decoder.
- Same-attempt collection reuses the same writer and provider result. A failure after durable sealing is recoverable by a new executor without another model request.
- Real child processes exit after typed-output seal or finalization. New processes recover and commit, then reopen again: exactly one model HTTP request, one OUTPUT, three terminal operations and unchanged settled physical bytes remain. The second resume adds no work.
- All-rejected candidate batches retain complete deterministic no-feasible evidence and the typed OUTPUT. The primitive does not select them for Statement.
- A real reservation occupying the remaining artifact allowance produces `idea_output_budget_exhausted` review, no publishable occurrence set and no additional provider call.
- The historical provider-only generation reader rejects these additional output occurrences. This explicitly preserves its old contract until a dedicated typed reader exists.
- A failure-first review regression reproduced an omitted output family in the discarded-publication SQL: one logical operation and one byte reservation stayed live after review. The corrected release transaction closes the child and charges its published bytes while attaching no occurrence; exact review replay changes neither accounting nor HTTP count.

The initial process fixtures exposed a missing serialized stage field and a restarted fake clock preceding durable timestamps. The fixtures now use their own exact input and advance time monotonically between processes. Those failed runs are not acceptance evidence.

## Validation

Windows/amd64, Go 1.26.5, local HTTP fixtures only:

- `go test ./internal/application -run '^TestIdeaBatchOutput' -count=1`: passed, 1.544 s, including the review-cleanup correction.
- `go test ./...` and `go vet ./...`: passed with the correction; application 51.289 s, SQLite 43.984 s, integration 8.528 s. The earlier pre-correction sweep also passed but is not the final acceptance result.
- `GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/...`: passed.
- Architecture check: 26 normative files; gofmt: 333 Go files; `git diff --check`: passed.
- `go test -race -timeout 30m ./...`: passed with the correction; application 392.462 s, SQLite 505.544 s, CLI 31.687 s, integration 124.771 s and workflow 1.814 s. This is the final full sweep; the earlier pre-correction sweep is not used to accept the release path.

## Remaining scope

Neither historical preview revision calls this method. It publishes only the initial batch; an authorized mutation-input collector, current typed-output reader, retained historical-source reader and checked authorization transaction remain open. Cache-derived output requires an explicit provenance policy and is currently refused when no successful provider parent exists in the current attempt. Optional provider-only cache indexing is disabled on this result.

Crash recovery is proven after seal and finalization. A writer interrupted before seal remains subject to the existing conservative artifact protocol; automatic continuation of an unsealed writer with the same identity is not claimed. The complete BR-01 acceptance must cover the remaining prepare/write/attach boundaries when composed with the new graph.

No actual provider, Similarity service or Docker smoke was run for this checkpoint. This does not complete automatic mutation, Slice 2 business routing or the MVP, and cannot produce READY.
