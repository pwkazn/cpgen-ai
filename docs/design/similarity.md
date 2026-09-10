# Similarity Service and Decision Policy

Status: Current under ADR-0006

## 1. Boundary

Similarity is an ordinary typed stage-local call through MeteredSimilarity. It compares canonical package-safe projections and returns evidence; the application policy, not the remote service, decides accept, reject, review, or block.

The adapter cannot mutate run state, publish artifacts, or select workflow stages.

## 2. Types

~~~text
SimilarityRequest
  schema_version
  candidate_projection_digest
  normalized_title
  normalized_statement
  normalized_tags
  language
  policy_ref
  policy_digest
  logical_idempotency_key

SimilarityHit
  source
  external_id
  score
  canonical_url?
  title_digest?
  evidence_digest

SimilarityEvidence
  request_digest
  provider_identity
  hits
  score_summary
  observed_at
  policy_digest
  CallTrace

SimilarityDecision
  ACCEPT | REJECT | NEEDS_REVIEW | BLOCKED
  rule_id
  evidence_digest
  explanation_code
~~~

Raw private generation context is not sent. Only the explicitly defined package-safe projection is eligible.

## 3. Configuration and policy

Configuration defines endpoint identity, HTTPS and host allowlists, credentials by environment reference, timeout, maximum response bytes, maximum hits, retry bounds, cache TTL, score thresholds, review band, and policy revision.

Threshold semantics are explicit and tested at exact boundaries. Provider scores are finite normalized decimals. Unknown algorithms or missing policy metadata fail closed.

## 4. Adapter behavior

The adapter:

1. canonicalizes and hashes the request;
2. reserves call and cost budget;
3. records the stable logical identity and physical call;
4. applies DNS/IP/HTTPS, redirect, timeout, decompression, and response-size rules;
5. strictly decodes the complete response;
6. normalizes and sorts hits deterministically;
7. settles budget and stores CallTrace;
8. returns typed evidence to the stage policy.

Provider-specific response fields remain inside the adapter. URLs are sanitized before display or package-safe projection.

## 5. Retry and blocking

Transient connection, timeout, rate-limit, and selected server errors may use bounded retry during the foreground attempt. Permanent authentication, policy, schema, or compatibility errors block or fail according to typed rules.

An unknown send boundary uses the original idempotency identity for provider reconciliation if available; otherwise accounting is conservative and the stage enters a typed pause or review path.

When a dependency is unavailable, BLOCKED records the exact endpoint identity, policy digest, request input digest, error evidence, and retry-after time.

## 6. Manual resume

Manual resume acquires the run process lock and creates a fresh attempt of the Similarity stage. Before querying for new evidence, the attempt revalidates the checkpoint dependency through MeteredSimilarity under current policy and budget.

Historical health or capability data cannot by itself authorize new evidence. If the check fails, the new attempt returns BLOCKED. Existing evidence already committed for the same input revision remains auditable and may be considered only where policy explicitly allows it.

## 7. Evidence cache

The cache key includes adapter protocol, endpoint identity, canonical candidate projection, language, algorithm revision, and policy digest.

A valid hit must retain:

- the original source CallTrace;
- provider identity and observation time;
- full normalized hits and score summary;
- policy and request digests;
- current-run cache-use and artifact provenance.

Expiry, policy mismatch, corrupt Blob evidence, or a newer incompatible capability result invalidates the hit. A hit never returns a naked Blob reference.

## 8. Decision rules

Current MVP routing amendment (2026-09-09): only a verified ACCEPT may continue to Solution. REJECT, the review band and a completed business decision with insufficient evidence enter NEEDS_REVIEW, without mutation or automatic evidence recheck. Provider/transport failures still use the existing typed BLOCKED/recovery path. The following decision values remain evidence classifications; they are not mutation dispatch authorization. See the [forward-loop plan](../superpowers/plans/2026-09-09-mvp-generation-loop.md).

A versioned policy maps evidence to:

- ACCEPT below the acceptance threshold with required coverage;
- REJECT at or above the hard-rejection threshold when evidence is sufficient;
- NEEDS_REVIEW within the configured ambiguity band or for designated sources;
- BLOCKED when required evidence cannot be obtained safely.

The decision stores rule ID, thresholds, exact evidence digest, and a package-safe explanation code. Waivers bind the same policy and evidence and cannot cover later revisions automatically.

## 9. Privacy and retention

Requests omit solutions, tests, secrets, local paths, hidden prompts, and private provenance. Logs store digests, sizes, provider identity, timings, status, and sanitized errors. Retention and package projection are explicit per field.

## 10. Tests

Tests cover canonical projection, Unicode normalization, exact threshold boundaries, stable sorting, HTTPS and host policy, redirect refusal, response limits, timeout and cancellation, bounded retry, unknown send handling, cache source provenance, stale health rejection, manual resume dependency checks, review decisions, privacy, and package-safe evidence.
