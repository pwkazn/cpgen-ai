# Slice 2 business routing implementation plan

Status: **Deferred and superseded as the active work order, 2026-09-09.** The user prioritizes ACCEPT → Solution/Data/Docker/Judge/Package and non-accepted business results → human review. Follow the [current MVP generation-loop plan](2026-09-09-mvp-generation-loop.md). Mutation is postponed for redesign and is not an MVP prerequisite.

SIM-03a, MUT-01a–MUT-01c, MUT-02a–MUT-02b and BR-01a retain their completed evidence. The remaining sections preserve the earlier design for reference only; their imperative wording and "next" sequence are not current implementation instructions. Actual mutation route execution remains unimplemented.

## Fixed compatibility boundary

`slice2.idea.statement.similarity.checkpoint.v1` always retains Similarity evidence and stops at non-waivable `NEEDS_REVIEW`. Preserve this revision, all prior attempts and frozen configuration bytes. Enable actual routing only through a new compiled, explicitly selected revision after its complete lifecycle gates pass. Acceptance at the implemented end of Slice 2 must still stop before Solution/Quality/Package and cannot produce READY.

## 1. SIM-03a: inspect the committed decision and allowance

- Build a deterministic read-only plan from the verified current Statement/Similarity chain and checkpoint input. Bind run/version/revision/config, submitted snapshot, source batch/selected idea, problem, semantic Similarity input, evidence, policy decision and shared mutation quota.
- Accept → continue to the implemented slice boundary; review band → review; reject with quota → plan Idea mutation; exhausted quota → review; insufficient evidence → plan fresh Similarity evidence. Incompatible or unverified evidence must fail validation.
- CONTENT and METADATA consume the same per-stage logical cap. Read all account/claim projections consistently without creating an account or spending a claim. A plan is an observation, never dispatch authorization.
- Preserve every frozen budget dimension in RunView. Mutation count and package byte caps are run-level limits, not physical reservation dimensions.
- Verify deterministic rereads, all five outcomes, tampered bindings, pending cancellation, exhausted shared quota, malformed JSON and zero extra HTTP/writes.

## 2. MUT-01: deterministic mutation contracts and lineage

- Define a pre-claim immutable core containing request/source batch, optional parent, trigger kind/evidence, policy/scope, logical ordinal and candidate count. Hash this core before quota acquisition. Bind the returned durable claim into a separate immutable authorized intent; the claim cannot contain the hash of an object that itself contains that claim.
- Similarity triggers require the committed rejection decision and the selected source parent. No-feasible triggers require a deterministic full-batch feasibility report and may omit the parent.
- Derive batch identity and child seed axes from the frozen request/seed, source and intent. One logical mutation claim may generate the configured 2–8 candidates. Allocate each child a distinct deterministic lineage ordinal in an overflow-checked range; never reuse one parent/ordinal pair for all children or charge a separate logical claim per child.
- Expose a versioned mutation draft input that cannot be mistaken for the initial batch-zero draft. The provider supplies content only; local binding fixes lineage, request, resource fields and hashes. Re-run feasibility and selection for every resulting batch.
- Verify exact deterministic bytes, distinct child identities, overflow rejection, crossed request/parent/evidence/claim refusal and replay independent of wall clock.

## 3. SIM-03b / WF-05a: persist route authorization atomically

Prerequisite checkpoints: the [read-only plan](../../evidence/slice2-similarity-route-plan.md), [core/intent/draft contracts](../../evidence/slice2-mutation-contracts.md), [result ledger](../../evidence/slice2-artifact-mutation-records.md), [candidate collection](../../evidence/slice2-idea-candidates.md), [atomic completion](../../evidence/slice2-atomic-mutation-stage.md) and [mutation provider contract](../../evidence/slice2-mutation-prompt.md) are accepted. They do not execute this authorization transaction or register a new compiled workflow.

- Add a checked SQLite command that owns a single immediate transaction. Recheck current run/stage/attempt, expected version, frozen policy/input/evidence, active accounting and absence of pending cancellation before any new work.
- Persist the exact canonical route/core and original command identity. Acquire the logical mutation claim and bind its authorized intent in the same transaction as downstream invalidation and the new authoritative input. An idempotent replay returns the original result without spending quota or invalidating a newer revision twice.
- The existing generic `ClaimMutation` ledger is not an active-attempt or cancellation guard. Its immutable timestamp participates in command identity; do not recreate timestamps on replay or call it as independent authorization before/after the checked route transaction.
- Preserve old committed source receipts as explicit intent references before invalidation. Current-input readers must continue rejecting invalidated historical stages. A narrow history reader may resolve only the exact proofs retained by the authorized intent.
- For insufficient evidence, retain the historical decision and move to a fresh Similarity attempt with a new physical identity. Consume no content mutation claim and never present replay of old insufficient evidence as current dependency health.
- Verify changed version/cancel/policy/evidence refusal with no partial writes, concurrent claims, quota exhaustion, exact replay and transaction rollback at every inserted proof/claim/invalidation boundary.

## 4. MUT-02: preserve no-feasible evidence and execute authorized drafts

- MUT-02a now provides `CollectIdeaCandidates` and `ReadIdeaCandidates` for a durable full initial batch. The existing preview still returns review when selection finds no feasible candidate. Compose collection and selection as separate compatible graph boundaries before making `NO_FEASIBLE` executable; uncommitted provider bytes cannot be a mutation source.
- Reconstruct mutation input from the persisted authorized intent and retained source proof after restart. Bind all original/repair calls to the exact active mutation attempt through the current ledger, receipt and accounting protocols.
- Successful batch commit must retain the actual logical operations, physical reservations, settlement snapshot and output batch in its immutable mutation record. Failure/cancellation retains intent and terminal accounting without fabricating a successful record. Optional cache reuse needs an explicit mutation identity/provenance policy before it can be enabled.
- Existing provider response occurrences are private `EVIDENCE`; the generic mutation ledger requires an `OUTPUT` occurrence. Publish the validated typed batch under an explicit local-write budget and retain that output with its actual call/reservation evidence. A settled local write can attach only with matching finalized bytes and successful physical receipt; stage attachment must not charge it twice. Failed business calls may still retain successful diagnostic writes.
- Mutation records require unique operations/reservations, matching immutable grant limits and committed output references. MUT-01c supplies `FinishMutationStage` to attach output, retain complete attempt evidence and advance atomically. Integrate that command for successful mutation batches; the ordinary preview remains on `FinishStage`. `ReadMutationRecord` recovers exact result metadata and is not a source-content reader.
- MUT-02b supplies the compiled `idea.mutate` prompt and one optional format repair under separate input/schema identities. Select it only after the new route authorization reader has reconstructed and validated the complete authorized input. Prompt resolution and a structural grant alone cannot authorize a new physical call.
- A feasible child receives a new deterministic selection, then a new Statement and Similarity chain. Rejected/infeasible subsequent batches spend another shared logical claim only through the same checked command. Budget exhaustion enters review.

## 5. WF-07a: prove the complete bounded loop

- Local HTTP fixtures: reject → authorized mutation → new selection/Statement → accept; repeated reject → exhausted review; no-feasible → new batch; insufficient evidence → fresh success; human review remains explicit.
- Kill real processes after route observation, authorization transaction, draft opening, response seal, call completion, mutation record and stage commit. Reopen in a new process and prove exact claim/call counts, immutable seed/config, current input and historical source retention.
- Test cancellation and active-time exhaustion at the same boundaries. Receipt cleanup may finish existing calls, but cannot create a claim, draft, provider retry or repair. Concurrent Resume must retain the existing per-run process lock.
- Run full normal/vet, full race with the documented package timeout, Linux build and architecture/format/patch checks before enabling the new revision. Paid provider smoke, actual YuantiJi protocol acceptance and Docker integration remain separate opt-in evidence.

## Completion criteria

Under this superseded design, SIM-03 required fresh insufficient-evidence rechecks and bounded content mutation. Those conditions are no longer current MVP completion criteria; use the forward-loop plan's ACCEPT/review split. The historical evidence still does not claim an executed mutation loop.

## Next executable sequence

The remaining work has two distinct transactions: **authorize and restart** before provider work, then **attach the validated batch and finish** after all accounting is terminal. MUT-01c implements only the second transaction. Keep these responsibilities separate in interfaces, events and acceptance claims.

| Item | Concrete implementation boundary | Acceptance before moving on |
|---|---|---|
| BR-01: typed batch publication | Add an application-owned private-Blob output child for generation. Publish canonical IdeaBatch bytes under an explicit local-write reservation, binding snapshot, semantic batch digest, input digest and parent generation operation. Retain raw provider EVIDENCE separately. | Initial and mutated batches; semantic digest distinct from Blob hash; prepare/seal/settle/attach crashes; missing bytes and changed provenance rejected; zero extra provider calls and exact physical-byte charges. |
| BR-02: retained source proof | Persist narrow immutable references to the exact source stage attempt, typed batch OUTPUT and committed trigger proof. Add a history reader callable only through an authorized intent, checking occurrence scope, bytes and semantic digest. | Source remains readable after downstream invalidation; ordinary current readers still reject it; crossed run/attempt/batch/decision and missing Blob fail; no latest-stage fallback or source reconstruction from loose prompt text. |
| BR-03: atomic authorization | Refactor the generic claim implementation into a transaction helper without changing its public command bytes. A new checked domain command verifies the route attempt and retained proofs, acquires shared quota, stores the exact bound intent and invalidates the exact compiled suffix in one immediate transaction. | Failure at proof/claim/intent/projection/event/COMMIT leaves no partial authorization. Same command returns its original grant/result; changed commands, pending cancellation, stale version and quota exhaustion cannot partially invalidate. |
| BR-04: attempt input and execution | Reconstruct the current mutation input from stored authorization and source proof. Bind the dedicated prompt, durable original/repair calls and typed output publication to that active attempt. Finish through `FinishMutationStage`. | Restart after every call/publication boundary preserves the original claim and physical identities. Failed generation, invalid content or cancellation retains accounting without a successful record. A valid committed batch retains its record even when every candidate fails feasibility; the next checked selection/authorization boundary handles that evidence. Successful output is locally rebound and reselected. |
| BR-05: compatible graph and entry point | Register a new immutable revision with collection, selection/authorization, Statement, Similarity, decision and unfinished-boundary stages. Add closed frozen configuration selection. Preserve both historical revisions and the Fake default. | Full reject→mutation→accept and quota-exhausted loops, no-feasible regeneration, fresh insufficient-evidence recheck, review, cancellation, lock contention and lost commit returns through production Bootstrap/CLI. Never READY. |

BR-01a implements the initial-batch publisher as an independent primitive and passes [complete local gates](../../evidence/slice2-idea-batch-output.md). It preserves raw original/repair evidence, verifies typed bytes, retains all-rejected batches and settles or discards the output through stage completion. Actual process exits after seal/finalization and review replay retain exact call counts and byte charges. No existing graph selects it.

The former next deliverable, now deferred, was **BR-01b: a current committed typed-batch reader**. Its proposed contract was to read only a successful current collection attempt; require exactly one OUTPUT with the compiled declaration/provenance and successful local receipt; verify bounded canonical bytes, both Blob and semantic digests, exact submitted snapshot and initial-batch binding. Original/repair EVIDENCE would retain existing strict receipt checks. Proposed tests included missing/corrupt bytes, changed role/schema/source, conflicting outputs, invalidated attempts and zero new HTTP. The forward MVP does not require this reader or the subsequent mutation-output binding.

After that current reader passes its own gates, BR-02 can define the exact retained source proof with a real typed output reference. BR-03 then owns the first production mutation claim. This order prevents an uncommitted response, an arbitrary historical row or a generic quota grant from becoming dispatch authority.

### BR-01b implementation map

Code inspection confirms that filtering only in the application reader is insufficient: SQLite's `readCommittedPrivateStage` validates every occurrence through the closed provider-media family before returning it. The new boundary therefore needs a separate composite storage read, while the historical `ReadCommittedLLMStage` behavior remains strict.

| Layer | Concrete change to prepare | Required proof |
|---|---|---|
| Port | Add a narrow committed-batch result carrying the current successful attempt, stage version, one typed output receipt and its original/repair evidence. Expose immutable proof data, with no dispatch capability. | No caller-selected arbitrary historical attempt; no partially populated success on invalid metadata. |
| SQLite | Reuse current successful-stage projection checks and private-receipt validation helpers within one read transaction. Partition the exact closed OUTPUT family from provider EVIDENCE before applying their separate metadata and local-call constraints. | Same run/stage/attempt, terminal successful publisher and provider parent, actual settled reservation, exact provenance hash, bounded occurrence count and one output. Old provider-only calls continue rejecting the composite stage. |
| Application | Read and verify the bounded typed Blob; rebuild the initial draft through the existing structured response reader and local binder; compare canonical output bytes and semantic digest. Return the batch proof independently of feasible selection. | No HTTP, quota mutation or cache-index write; exact snapshot/count/policy binding; all-rejected batch remains readable. The composing executor retains its run lock while reading and rechecks expected stage/version at the next write. |
| Acceptance | Cover ordinary/repaired/all-rejected batches, database reopen, invalidation, missing/corrupt Blob, substituted output metadata, duplicated output and crossed parent. | Focused tests first, then complete local gates. Do not register a new graph or relax existing readers to make the fixture pass. |

Relevant current implementation is in `internal/port/generation_store.go`, `internal/adapter/storage/sqlite/committed_llm_stage_reader.go`, `internal/application/generation_reader.go` and `internal/application/idea_batch_output.go`. These are implementation starting points; proposed new contracts are not yet available APIs.

### Compatibility details discovered during implementation

- `FinishStage` requires the fixed immediate successor. It cannot implement backward routing through a changed NextStage. BR-03 must explicitly validate and reset the entire compiled downstream suffix, preserving attempts/events/occurrences, as the existing review revision transaction does. It must not create a synthetic human ReviewDecision or revise frozen request/config bytes.
- A collection stage named `idea` followed by a separate selection boundary can preserve the existing content executor's stage identity. The selection boundary commits the Statement input; the collection boundary commits the batch proof. Evaluate the complete sequence before registering its revision.
- The current SimilarityRoutePlanner is deliberately bound to the historical preview checkpoint. Introduce a new versioned executing-route contract for the new graph instead of widening the meaning of already persisted v1 plans.
- `ReadIdeaCandidates` reconstructs initial batch-zero drafts. A current mutated batch requires an explicit authorized-input reader and `BindMutation`; it must never fall back to initial binding. Statement selection must bind the newly committed batch and downstream input.
- The existing generation reader expects one successful provider response plus at most one original/repair diagnostic. Adding a typed OUTPUT requires a separate filtered output-reader contract; do not treat an extra output occurrence as a third provider response or weaken existing receipt checks.
- The output publisher should use the established application-owned generation/private-Blob lifecycle and stable child identity, with only physical artifact-byte reservations. The sandbox call kind in generic ledger fixtures is not a production mutation publisher design.
- Every compiled private-output family must participate in both successful attachment and discarded-output release. BR-01a failure-first review coverage caught a family omitted from the latter query; success-path evidence alone cannot establish terminal cleanup.
- Sealed/finalized typed-output recovery is covered. Before-seal interruption remains subject to the conservative writer protocol; do not promise same-identity writable continuation without its own state transition and crash acceptance.
- Persist the original authorization timestamp and command payload. A heartbeat or process restart cannot silently manufacture a new command identity or another shared claim. Close active accounting before authorization, then recheck version and pending control state inside its transaction.
- For insufficient committed evidence, invalidate only the Similarity/decision suffix and allocate a new attempt through ordinary BeginStage. A transport UNKNOWN or unreadable private receipt remains a cleanup/dependency case and must not be reclassified into this business recheck path.

These items are implementation dependencies, not estimates of completion. Each new compiled path must receive its own complete local lifecycle gates before becoming selectable.
