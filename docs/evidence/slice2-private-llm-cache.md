# Slice 2 private LLM cache and artifact completion

Date: 2026-09-08. Branch: `codex/phase2`. Base commit: `50deb38599b1d825ea15b7bcf1140882b32ba7e8`.

Status: PASS for LLM-05. This extends the [bounded JSON repair checkpoint](slice2-bounded-json-repair.md). Real graph and stage/CLI assembly remain separate work.

## Implementation

- `StructuredLLMCache` admits only committed successful results, including the one successful format repair. It verifies the original/repair call chain, private receipt, exact local publication and stage occurrence before writing a cache entry. Uncommitted responses and failed outputs are rejected.
- Cache keys bind the private RunID, provider's canonical request identity, model/endpoint, original prompt/schema, sampling, output limits, provider policy and repair policy. Logical call identity is normalized so a later attempt can reuse the same admitted input. Only the `private` classification is accepted; no shared or cross-run partition is provided.
- Cache reuse verifies complete Blob bytes, source and provider calls, request binding, provenance and the current strict output validator before creating the cache-hit call. The result has zero new provider usage, no physical provider calls, no borrowed writer token, and an explicit current-run `PendingCacheReuse` for atomic stage attachment.
- Cache-hit and reuse identities are deterministic. A process can stop after logical cache completion or after reuse-record insertion and recover the same provenance without another provider request.
- Local response writes now begin their own durable artifact dispatch before writing. An unused local slot aborts and releases bytes before the next transport attempt. Published writes remain pending until their stage occurrence and byte settlement commit together; their physical and logical producing calls then become terminal in the same transaction.
- A rejected stage releases private pins and closes local calls. Bytes already published into canonical storage are charged at their measured physical-new-byte count; unpublished staging reservations are released. Provider calls awaiting reconciliation remain subject to the existing interrupted-attempt protocol.
- Migration 21 changes only the cache source's call-record foreign key from same-attempt to same-run scope. The current cache-hit call retains its full current-attempt binding. Historical migrations remain unchanged. The table rebuild preserves identities, dependent rows and terminal guards and checks the actual foreign-key graph before clearing the deferred counter.

## Failure-first findings

1. A committed private artifact still had a PREPARED local call, preventing it from serving as a terminal cache source. The new lifecycle test first reproduced that state, then passed after atomic local completion was added.
2. Stage rejection left published private bytes reserved and the local physical call SENT. A regression reproduced the held reservation; cleanup now records the publication cost and terminal state.
3. The cache bridge and its constructor were absent, so the first integration tests failed to compile.
4. Reusing a committed response from the next stage failed on an existing foreign key. Migration 21 permits an earlier attempt in the same run while retaining cross-run rejection.

## Focused verification

- Original success with repair disabled and successful repaired output both cache and reuse without additional HTTP requests.
- Cache entry creation before stage commit fails. A fault inside local-call completion rolls back occurrence insertion, byte settlement and physical state together; retry commits once.
- Input, run, provider policy, sampling and output-limit changes miss the cache. A public/shared classification is rejected. Missing private files fail closed and invalidate the candidate.
- Substituted provenance fails before a current cache-hit call is created. Restoring valid metadata then succeeds without another provider request.
- An injected reuse-commit failure leaves the already-completed logical hit recoverable. Reopening SQLite and retrying creates one reuse record and no physical provider calls.
- Real subprocess exits before and after reuse-record insertion recover the same cache-use identity. Provider HTTP count remains two for the original invalid response and its single repair.
- A populated M20 database containing successful physical provider work, settled reservations and an existing cache hit upgrades to M21 without changing those records. The earlier-attempt hit fails before upgrade and succeeds after it; a direct cross-run source substitution still fails. Foreign keys and terminal triggers remain enabled.

## Repository gates

- `go test ./...` and `go vet ./...`: PASS across 22 tested packages and three packages without tests.
- `go test -race ./...`: PASS; application 401.187 s, SQLite 234.696 s. An earlier full run found seven test assertions still expecting migration 20; those assertions were corrected and the entire gate rerun successfully.
- Linux amd64 `CGO_ENABLED=0 go build ./...`: PASS.
- Architecture check: PASS for 26 normative documents. Formatting check: PASS for 255 Go files at this checkpoint. Tracked patch and 43 new-file whitespace checks: PASS.

All fixtures use local HTTP and private temporary storage. No paid provider or Docker smoke and no commit are part of this checkpoint. Subsequent graph work has its own verification checkpoint.

## Remaining integration

The factory must compose cache lookup, durable generation, stage commit and post-commit cache publication under the foreground run lock. Stage-level interrupted-attempt reconciliation, real Idea/Statement adapters and graph routing remain WF-01–WF-05 and LLM-06. Cache admission never replaces domain-chain validation or package gates, and this checkpoint does not permit READY.
