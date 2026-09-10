# Slice 2 typed generation execution

Date: 2026-09-09 UTC+8. Branch: `codex/phase2`. Base commit: `50deb38599b1d825ea15b7bcf1140882b32ba7e8`.

Status: WF-02a PASS. Bootstrap and CLI still select Fake stages.

## Implementation

- `GenerationExecutor` is an application composition component with concrete Idea and Statement methods. Workflow steps do not receive its storage, locks or provider adapter. The foreground caller owns the run/artifact locks, active-time interval and stage commit.
- Admission checks the exact Slice 2 run, request/config/workflow identity, current RUNNING attempt, persisted typed input and an open active-time interval. A stale optimistic version is permitted only within those unchanged bindings; call transitions use the existing attempt-bound ledger bridge.
- Idea restores the submitted request snapshot. Statement rebuilds the verified committed Idea chain before any provider operation. Compiled content prompts, output validators, deterministic draft binding and semantic identities are reused.
- Each original logical call derives from the run, stage and attempt, with its opening time anchored to the persisted attempt start. Reconstructing the executor preserves the same call identity despite heartbeat version changes. Existing provider calls are replayed before any cache lookup can replace them.
- A verified same-run cache hit returns current-attempt reuse occurrences, no raw writer token and zero provider usage. If a previously opened cache call no longer has a usable cache candidate, execution fails closed instead of falling back to a paid request.
- `GenerationStageResult` carries the typed outcome plus application-owned private occurrences, traces and settled usage. Successful output can attach its original/repair receipts atomically with the stage. Cache indexing independently verifies the committed producer and therefore fails before stage commit.
- Budget exhaustion, unknown send boundaries, invalid bound content and no feasible initial candidates request review. Retryable/unavailable/incompatible dependencies return a bound BLOCKED checkpoint. A format-repair allowance never becomes a transport or business-content retry loop. Non-successful stage finishes retain the existing ledger settlement/release behavior rather than attaching success occurrences.

## Verification

The tests initially failed to compile without the executor, then exercised real SQLite, private Blob publication and a local HTTP provider:

- Both stages require one format repair, preserve separate receipts and usage, commit their typed chain and reconstruct it without additional HTTP.
- A recreated executor replays the original Idea result after heartbeat version advancement. Its cache source cannot be indexed before commit, but indexing succeeds after attachment.
- Substituted seed/input, a closed active-time interval and an obsolete attempt are rejected before dispatch.
- Zero remaining model calls produce review with no HTTP; committed prior-attempt cache reuse still succeeds at zero additional usage after a real ReviewRevise transaction and a new attempt.
- HTTP 503 yields BLOCKED, HTTP 400 yields review and a closed response connection yields unknown-boundary review. Each terminal call replays without a second request or a format-repair call.

Full tests and vet pass. Linux compilation, architecture consistency (26 documents), formatting (278 Go files after the harness regression test) and patch whitespace checks pass. Full repository race verification passes: application 583.601 s and final integration rerun 110.829 s; unchanged packages are cached on the rerun.

The first full race run exposed a harness defect during a slow helper readiness check: embedding `bytes.Buffer` promoted `ReadFrom`, allowing `io.Copy` to bypass the synchronized `Write` method. The buffer is now private, a streaming-copy regression exercises concurrent diagnostic reads, and helper cleanup is registered before readiness waiting. The bounded readiness allowance is 30 s for instrumented subprocess startup. Focused race/crash tests pass (52.700 s), followed by the successful complete race run. Production execution behavior is unchanged by this harness fix.

## Integration still required

The run service must select the compiled workflow explicitly, start accounting before fresh BLOCKED dependency checks, attach these occurrences before advancing, and restore/reconcile durable calls before releasing interrupted-attempt resources. Configuration alone does not enable live generation. Similarity's durable provider evidence, business mutation routes and later Solution/Data/Judge/Quality/Package gates remain separate work. No paid HTTP request, Docker smoke, commit or READY path is included.
