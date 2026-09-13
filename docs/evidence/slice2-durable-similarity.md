# Slice 2 durable Similarity evidence

Date: 2026-09-09 UTC+8. Branch: `codex/phase2`. Base commit: `50deb38599b1d825ea15b7bcf1140882b32ba7e8`.

Status: SIM-01 PASS, 03:43 UTC+8. Full tests/vet, race, Linux build and architecture/format/patch gates pass. Bootstrap and CLI still select Fake stages.

## Implementation

- `PhysicalProvider` binds the exact Similarity wire request to the effective endpoint, credential reference, service identity, timeout and response limits. Planning reads no credentials and sends no request. `SearchPhysical` accepts the ledger's physical identity and permits exactly one HTTP exchange even when the standalone compatibility adapter allows retries.
- `SimilarityCalls` uses the existing `CallCoordinator`, call-count and cost reservations, completion receipts and bounded retries. A recovered DISPATCHING identity never regains send authority. Missing credentials and cancellation before dispatch report no-send; incomplete HTTP responses report an unknown boundary. Completed HTTP failures retain the actual response-body digest as metadata.
- Cost is verified only when the provider explicitly reports it, including an explicit zero. Missing or invalid accounting consumes the admitted conservative ceiling under the existing ledger contract. Evidence retains the producing response's usage report; settled reservations, including earlier failed attempts, remain authoritative for total spend.
- `NewReplayableSimilarityCalls` and `SearchWithArtifacts` retain one bounded private receipt for the producing response. The receipt contains normalized evidence, exact request/policy binding, original provider/physical identity, response digest and admitted reservation usage. Credentials and the original query text are excluded. Normalized evidence stays independent of its private Blob location.
- Shared `privateResponseSession` owns local declaration, dispatch, publication, sealed recovery and unwritten-slot release. Historical LLM receipt bindings, paths and mutation identities remain unchanged. Migration 23 adds only local artifact writes beneath Similarity calls; settlement requires the closed receipt prefix/kind pair and a terminal provider parent in the same run, stage and attempt.
- Private bytes become an occurrence, settle their byte reservation and finish the local publication call in the same stage transaction. Failed/reviewed stages charge already published bytes while releasing their pins; unused or unsealed slots release their reservation after the provider parent becomes terminal.
- `SimilarityReader` accepts only read capabilities. It reconstructs currently committed evidence from the current successful attempt, exact typed input, retained occurrence, terminal provider and local publication, verified Blob bytes, frozen adapter policy, physical receipt and stored semantic output digest. It cannot finalize a sealed writer, authorize a new call or use a historical success after Review invalidation. LLM and Similarity committed readers admit separate receipt families.

## Verification

Tests use local `httptest` providers, SQLite and private Blob storage:

- One grant sends one request; a 429 followed by success consumes two call slots and the exact conservative-plus-verified cost. Terminal failures and unknown boundaries replay without another request.
- Database reopening preserves the complete evidence, trace and private artifact. Missing or corrupted receipt bytes fail closed with unchanged HTTP counts.
- Failures at local MarkSent, physical completion, logical finish and SEALED publication recover the original successful exchange. Cancellation after receiving the response still persists and settles that receipt.
- An injected local completion failure rolls back occurrence attachment and byte settlement together; retry commits the same response. Transport retries retain only the producing receipt. Budget rejection and unknown completion leave no unwritten byte reservation active.
- Committed reads require stage attachment, reject changed request/policy and substituted occurrence/attempt/output/provider/publication proof, and refuse missing bytes without regeneration. A real ReviewRevise/ApplyReview invalidates the old success. The reader test facade exposes no mutation methods and its provider panics if an exchange is attempted.

The initial complete normal suite and vet pass (application 34.547 s, SQLite 26.896 s, integration 12.581 s). Linux amd64 with CGO disabled builds successfully. Architecture consistency covers 26 normative documents, formatting covers 289 Go files, and tracked patch whitespace checks pass. A focused race run passed before the final committed-reader addition (application 226.651 s).

The first full race run reached Go's default 10-minute package timeout while the active Similarity subtest had been running for only 3 s inside fresh SQLite migrations. It reported no data race; SQLite itself passed in 278.167 s and integration in 90.493 s. CI and the documented command now specify `go test -race -timeout 20m ./...`, accommodating the expanded instrumented suite while preserving operation-level deadlines. The complete rerun passes: application 825.080 s, SQLite 390.728 s and integration 114.754 s. This sweep began before the independent WF-04b stage-gap correction; that correction has its own subsequent normal/focused checks and remains scheduled for the next complete sweep.

## Remaining composition

SIM-01 does not enable live CLI execution or complete Slice 2. The next executor must derive a stable Similarity request from the verified Statement chain, require the active current attempt and attach successful evidence before evaluating its review/acceptance route at a separately committed boundary. Explicit workflow/config selection, fresh BLOCKED dependency checks, crash reconciliation through the run service, private evidence caching and bounded business mutation remain open. No paid provider calls, Docker smoke, commits or READY path were exercised.
