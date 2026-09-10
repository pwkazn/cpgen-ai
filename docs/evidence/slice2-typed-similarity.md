# Typed Similarity execution and committed decision evidence

Date: 2026-09-09 UTC+8. Branch: `codex/phase2`. Base commit: `50deb38599b1d825ea15b7bcf1140882b32ba7e8`.

Status: SIM-02 PASS, 04:22 UTC+8. Complete tests/vet, race, Linux build, architecture, formatting and patch checks pass. The complete race sweep also includes WF-04b.

## Scope

`SimilarityInputV1` is a strict domain identity contract committed by Statement before the next attempt exists. It binds the request snapshot, validated ProblemSpec, public query projection, decision policy, physical provider policy, result limit and execution policy. The execution policy includes the compiled workflow revision, exact retry settings and conservative cost ceiling. Private generation content remains in the application layer; the similarity package still receives only its existing allowed projection.

`SimilarityExecutor` shares the generation executor's durable store, private blobs, clock and foreground lock ownership contract. Input reconstruction verifies the committed Idea/Statement chain and the next-stage digest before dispatch. Execution requires the exact current RUNNING attempt, immutable run/config/workflow bindings and an open active-time interval. The run-bound call ledger permits heartbeat version changes within that attempt.

The wire logical identity derives from run, attempt and semantic input; the open time derives from the persisted attempt start. An executor reconstructed during the same attempt replays existing evidence without HTTP. A fresh BLOCKED attempt uses a different wire Idempotency-Key, even though its semantic input remains unchanged. No similarity cache or business mutation is introduced.

The new compiled revision is `slice2.idea.statement.similarity.checkpoint.v1`: Idea → Statement → Similarity → `slice2_checkpoint`. The historical three-stage revision remains unchanged. Evidence collection returns a typed value, producing private occurrence and separate policy decision. Successful collection can commit evidence before the later checkpoint acts on Accept, NeedsReview, Reject or insufficient-evidence Blocked. Collection success does not mean policy acceptance. None of these compiled graphs can produce READY.

`ReadCommitted` reconstructs private content, exact attempt-bound request, verified committed receipt and policy decision without sending HTTP. The lower-level reader adds an explicit typed-input API and preserves its older wire-request-digest API. Transport failure, budget exhaustion and unknown boundary produce bound control outcomes without content repair.

## Verification

Failure-first tests captured the missing strict domain input and new graph revision. Local HTTP, SQLite and private-blob integration checks cover:

- Full Idea → Statement → Similarity commits, executor reconstruction across heartbeat advancement, exact receipt equality and committed reads with one Similarity request and two generation requests.
- Substituted content/query/input, changed physical or decision policy, limit, retry settings and cost ceiling rejected before new HTTP; closed accounting and obsolete attempt rejected.
- A failed dependency replays its original BLOCKED result; a new attempt performs one fresh request with a distinct wire identity and commits readable evidence.
- Zero call budget, HTTP rejection and unknown connection boundary map to review, without a format-repair or repeated request.
- Review-band, rejection and insufficient-hit decisions remain bound to retained committed evidence. They are not changed into acceptance by evidence collection.
- Existing graph revisions keep their stage sequence; the four-stage revision reaches an explicit pause through the same checked graph contract.

Complete normal tests and vet pass (application 47.022 s, SQLite 27.642 s, integration 8.027 s). Linux amd64 build with CGO disabled passes. Architecture consistency passes for 26 normative files; formatting passes for 293 Go files and `git diff --check` is clean. Complete race verification passes with the existing 20-minute package timeout (application 980.753 s, SQLite 400.332 s, integration 120.262 s). This sweep built the SIM-02/WF-04b checkpoint before subsequent WF-04c terminal-cleanup changes; those changes require their own acceptance.

## Remaining integration

The CLI remains Fake-only. WF-02 must explicitly select frozen real dependencies and atomically commit these typed outputs; SIM-03 must apply the committed policy decision at the slice checkpoint through accepted review/retry/mutation rules. WF-04 must reconcile original dispatched calls before terminal cancellation or attempt interruption releases response resources. Paid providers, Docker smoke, business mutation, later verification gates and READY are outside this checkpoint.
