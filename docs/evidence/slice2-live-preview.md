# Explicit live preview run service

Date: 2026-09-09 UTC+8. Branch: `codex/phase2`.

Status: WF-02/WF-03 preview composition and WF-04d provider lifecycle PASS (05:55 UTC+8). Complete normal tests/vet, production race sweep, focused supplementary race tests, Linux compilation and architecture/format/patch checks pass. This is local-fixture preview acceptance, not full Slice 2 business routing or MVP acceptance.

## Implemented boundary

`workflow.revision: slice2.idea.statement.similarity.checkpoint.v1` explicitly selects production Bootstrap composition of the LangChain provider adapter, durable typed generation executor, Similarity provider and foreground run service. Omitting the selector preserves Fake behavior and older effective configuration bytes. Merely configuring either provider does not enable live generation.

The closed configuration freezes candidate count, both per-exchange cost ceilings, provider/service identities, time/output limits, credential references and Similarity decision thresholds. Content selection and two-attempt transport policy are compiled. No raw secret, prompt, arbitrary graph order or test HTTP bypass enters YAML. The service derives storage/locks/clock from one generation executor and requires Similarity to share it exactly.

Creation admits the strict generation request, preserves submitted fields, chooses an omitted seed once from the OS random source and persists the exact signed 64-bit value. Explicit seeds are preserved. Resume and cancellation reject a changed configuration before mutation.

The graph executes Idea → Statement → Similarity → `slice2_checkpoint`. Every next-stage input is reconstructed from authoritative SQLite metadata and verified private receipts. Successful stage commits atomically attach occurrences and exact domain output/next-input digests. Cache indexing is optional after the authoritative commit; index failure cannot hide a committed advance. The checkpoint re-reads committed Similarity evidence, retains its policy decision and stops at non-waivable review for every decision kind. SIM-03 business routing and later quality/package gates remain outstanding; this revision cannot produce READY.

## Recovery and control

Ordinary RUNNING recovery retains the exact current attempt and original logical identities. Before-call, sealed-receipt and completed-call interruptions resume without replacing that attempt or repeating an existing HTTP exchange. Upstream committed stages are read rather than regenerated.

`GenerationExecutor.ReconcileStage` and `SimilarityExecutor.ReconcileStage` rebuild original requests from the frozen current input. They use the receipt-only coordinator before cancellation or exhausted-budget terminal transitions. Missing operations remain absent; cache cleanup checks an already-terminal immutable identity without looking up a new cache hit. Existing original/repair calls settle before stage resources are released. The run version is reloaded after settlement.

A read-only SQLite predecessor checkpoint identifies attempts that follow BLOCKED, including recovery after BeginStage and before call opening. Generation bypasses a surviving historical cache source for that attempt. Fresh provider work remains inside active accounting and durable budgets; replay of the same new attempt does not send twice. Ordinary review invalidation still permits valid same-run cache reuse.

Live-owner cancellation testing exposed an accounting heartbeat racing a provider/control version change. A version conflict now waits for the next bounded heartbeat instead of becoming a false budget interruption. A separate production Bootstrap test exposed local-zone cache timestamps; the production clock now returns canonical UTC. Both regressions were reproduced before correction.

## Verification

- Complete normal tests and vet pass after the subprocess and commit-error additions: application 50.557 s, SQLite 20.895 s, integration 6.175 s.
- The production composition's complete race sweep passes: application 907.325 s, SQLite 311.994 s, integration 98.739 s. Subsequent test additions have separate race verification: subprocess/active-owner tests 108.761 s, commit-error tests 36.204 s and CLI examples 5.261 s.
- Linux amd64, CGO-disabled command build passes.
- The full run-service suite covers committed chain/read-only review resume, before-call/sealed/completed same-attempt restart, both providers at absent/OPEN/sealed/completed cancellation boundaries, pending cancellation across restart, exhausted-budget sealed cleanup, frozen-config refusal before writes, fresh Similarity attempts after BLOCKED, and active-owner cancellation from a second service handle.
- The active-owner cancellation regression passes 20 repetitions after correcting heartbeat conflict handling. Caller-supplied cancellation expected versions remain strict; the test client refreshes after a rejected stale version.
- A historical cache source is tested both for allowed zero-budget review reuse and mandatory fresh generation after a BLOCKED predecessor, including reconstructed executor replay.
- Fifteen real crash/new-process-resume scenarios cover all three provider stages at attempt creation, before sealing, after sealing, provider completion and stage commit. Unknown unsealed responses pause without resend; recoverable responses finish with the same attempt and exactly two LLM/one Similarity exchanges across the whole chain. Further resume leaves the projection unchanged.
- Six commit-error scenarios reject a commit or lose its successful return at each provider stage. Resume retains original call/attempt identities and exactly three committed artifact references.
- Checked-in configuration and zero-budget request examples pass through the public CLI parser, production Bootstrap, generation and repeated resume. The CLI returns exit code 6/NEEDS_REVIEW at Idea without credential lookup or provider dispatch. This example test was added after the full sweep compiled and has separate focused verification.
- A configuration review aligned omitted candidate count with the accepted four-candidate default; explicit two-candidate examples remain unchanged. The regression was reproduced and the complete config/CLI race suites pass (2.164 s/19.363 s). Existing runs retain their frozen effective count and digest.
- Architecture consistency passes for 26 normative files, formatting for 309 Go files, and patch whitespace is clean at this checkpoint.

No paid provider, external Similarity service or Docker invocation is included. General business routing, mutation/repair revision handling and later verification gates remain separate work.
