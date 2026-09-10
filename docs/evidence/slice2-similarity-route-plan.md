# Slice 2 committed Similarity route planning

Status: Accepted

Checkpoint: 2026-09-09 06:32 UTC+8. SIM-03a completes a read-only prerequisite; SIM-03 business execution remains open.

## Implemented behavior

`SimilarityRoutePlanner.Read` owns shared run/artifact locks, verifies the frozen current preview checkpoint, resolves the committed typed Statement/Similarity chain and reads the authoritative shared mutation allowance. Pending cancellation, incompatible policy/config/revision, absent proof or a changed run version reject planning. No HTTP request, cache lookup, quota claim or stage transition occurs.

The bounded, canonical plan binds run/version/revision/config, submitted request snapshot, source batch and selected idea, ProblemSpec, semantic Similarity input, retained evidence, computed policy decision and quota projection. Validation recomputes decision/scope/plan digests and the action. JSON decoding rejects unknown fields, duplicate keys, case aliases and null fields without replacing an existing value on failure.

Accept plans continuation, the review band plans review, rejection plans Idea mutation while shared quota remains, exhausted quota plans review and insufficient evidence plans a fresh Similarity recheck. Incompatible evidence cannot become a new route. The preview revision still stops at its non-waivable review checkpoint for every retained decision; no plan is an execution grant.

`ReadMutationBudget` reads stage/run identity, immutable limit, stage/per-kind accounts and durable claim counts in one SQLite statement. CONTENT and METADATA share the same cap. Reads create no accounts. Exact claim replay does not consume a second slot, and inconsistent per-kind or aggregate projections fail validation.

## Regressions found during acceptance

A real run with a positive mutation allowance failed generation admission because `BudgetSnapshot` omitted `MaxMutationsPerStage`; it also omitted `MaxPackageBytes`. A failure-first test compared all frozen limits and reproduced both zero values. The reader now joins immutable run limits with physical account rows in the same query and verifies the request binding. Logical mutation/package caps are not converted into physical reservations.

Full ordinary testing also exposed a pre-existing Windows subprocess-test race. Its helper used an empty `select {}`, which can cause Go's fatal deadlock exit before the parent's `Kill`. A direct helper invocation reproduced `READY` followed by the fatal stack. The helper now waits with a 30-second fail-safe timer; the parent verifies conflicting acquisition is still blocked before killing, waits for abnormal exit and registers early cleanup. A separate race repetition exposed a one-millisecond timing assertion; its configured retry interval is now ten seconds with a one-second single-attempt bound, preserving the polling assertion without benchmarking OS scheduling.

## Verification

- All five route outcomes, stable read-only repetition, positive/zero/shared mutation budgets, malformed plan envelopes, changed evidence and pending cancellation pass focused local HTTP tests. Whole preview collection remains exactly two LLM exchanges and one Similarity exchange.
- Mutation quota tests verify absent-account reads, shared CONTENT/METADATA spending, exact replay, incompatible stage refusal and corrupted projection rejection.
- Complete ordinary tests pass after the lock fixture correction, and `go vet ./...` passes. The new application/SQLite normal sweep reports 55.524 s/21.264 s; unchanged results were cached on the subsequent successful whole-tree run.
- Complete `go test -race -timeout 20m ./...` passes: application 1069.266 s, SQLite 318.858 s, integration 92.232 s. The lock fixture corrections added after that sweep compiled have separate full runlock verification: 30 ordinary repetitions and 30 race repetitions (3.744 s for the final race repetition).
- Linux amd64 command build with CGO disabled passes. Architecture check passes for 26 normative files; formatting and patch whitespace pass.

The later mutation core/intent/draft contracts receive separate acceptance. Actual atomic route authorization, source retention across invalidation and bounded mutation/recheck execution remain in the [business routing plan](../superpowers/plans/2026-09-09-slice2-business-routing.md). No paid provider, external Similarity service or Docker call is included.
