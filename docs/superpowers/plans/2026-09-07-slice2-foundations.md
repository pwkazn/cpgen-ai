# Slice 2 Foundations Implementation Plan

Status: active on `codex/phase2`

Scope: request, Idea, Statement, provider-neutral model contracts, strict
structured output, similarity evidence and policy. Keep the executor local
and foreground-only; do not add a hosted workflow service or daemon.

## Tasks

1. **Strict content contracts**
   - Add immutable, versioned `GenerationRequestSnapshotV1`, `IdeaBatch`,
     `IdeaCandidate`, `IdeaSelection`, `StatementInput`, and `ProblemSpec`
     domain values.
   - Implement canonical encoding, digest binding, Unicode/length/range
     validation, deterministic candidate ordering, and rejection of invalid
     selection chains.
   - Add table-driven tests for normalization, required/forbidden conflicts,
     seed determinism, feasible filtering, and stale digest rejection.

2. **Prompt registry and strict structured output**
   - Add typed prompt references with immutable template/schema digests and a
     registry that rejects duplicate or unknown versions.
   - Add duplicate-key/unknown-field/schema-version/size/UTF-8 validation for
     structured model responses, with sanitized typed errors.
   - Add deterministic Fake model fixtures and tests for bounded repair input.

3. **Metered model adapter**
   - Implement an OpenAI-compatible, provider-neutral HTTP adapter behind the
     existing `MeteredLLM` port, including HTTPS/host policy, timeout,
     response caps, stable logical identity, bounded retry, usage settlement,
     privacy redaction, and cache provenance.
   - Keep API keys in environment references only; never persist or log them.
   - Add httptest coverage. Real DeepSeek smoke is opt-in and must be run only
     when explicitly requested.

4. **Similarity evidence and policy**
   - Implement package-safe projection, deterministic hit sorting,
     `SimilarityEvidence`, cache provenance, exact threshold/review-band
     decisions, and typed BLOCKED/REJECT/ACCEPT/NEEDS_REVIEW results.
   - Add an OpenAI-independent HTTP adapter and deterministic Fake tests.

5. **Phase 2 workflow wiring and E2E**
   - Extend the statically typed pipeline with Idea, Statement, and Similarity
     stages while preserving Slice 1 resume/review/budget boundaries.
   - Add deterministic end-to-end tests, fresh dependency revalidation after
     BLOCKED, and opt-in provider smoke tests using explicit environment
     configuration.
   - Update status/evidence only after all focused, race, vet, architecture,
     and full tests pass.

## Gates

Focused tests precede each implementation task. Before claiming completion:

```powershell
$env:GOTOOLCHAIN='go1.24.13'
go test ./...
go test -race ./...
go vet ./...
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
git diff --check
```

