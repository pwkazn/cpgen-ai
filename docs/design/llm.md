# LLM, Prompt, and Structured-output Design

Status: Current under ADR-0006

## 1. Boundary

Model calls are ordinary stage-local effects through MeteredLLM. The coordinator and stage code depend on a provider-neutral interface; provider fields remain inside adapters. Model output is always a candidate that deterministic validation may reject.

The model port cannot access SQLite, artifact directories, Docker, run locks, or unrestricted logs.

## 2. Request and response contracts

~~~text
LLMRequest
  operation_kind
  prompt_ref
  prompt_version
  template_digest
  model_policy_ref
  canonical_messages
  response_schema_ref
  response_schema_digest
  temperature
  max_output_tokens
  logical_idempotency_key
  privacy_classification

LLMResult
  structured_value
  canonical_response_digest
  provider_request_id?
  usage
  finish_reason
  CallTrace
  safety_metadata
~~~

Messages use typed roles and canonical UTF-8 encoding. Tool calls, images, streaming, or provider extensions are disabled unless a later version explicitly adds them.

## 3. Prompt versions

Every prompt has a stable reference, semantic version, template digest, expected input type, output schema, and migration policy. Run records store the prompt reference and digest used by each call.

Templates are rendered from typed values. Untrusted request text is delimited as data and cannot change system policy, tool access, artifact paths, or budget rules.

## 4. Strict structured output

Adapters request the narrowest supported structured-output mode, then independently validate:

- maximum encoded bytes;
- UTF-8 and JSON syntax;
- exact schema version;
- unknown and duplicate fields;
- required fields and enum values;
- string normalization and length;
- integer ranges and finite numbers;
- cross-field domain invariants.

A provider claiming schema success does not bypass local validation. Invalid output becomes a typed stage result with sanitized evidence.

## 5. Stage-local repair and retry

A stage policy may make a bounded repair call when validation errors are safe to disclose. The repair request includes only canonical error codes and the minimum invalid fragment needed for correction.

Transient transport failures may be retried within the same foreground stage attempt and budget. Every physical call gets a CallTrace record while the logical idempotency key stays stable. Retry stops on success, blocking, review, permanent failure, cancellation, or budget exhaustion.

If the provider send boundary is unknown, the adapter queries the original provider identity when possible. Otherwise it conservatively settles the reservation and returns a typed review, blocking, or failure outcome. It never sends the same logical work under a new key merely because the process restarted.

## 6. Metering and accounting

Before a physical request, MeteredLLM reserves:

- one model call;
- maximum input and output tokens under the selected tokenizer policy;
- maximum configured cost;
- optional artifact bytes for captured package-safe evidence.

After response or failure it settles actual usage where trustworthy and stores pricing policy, tokenizer revision, request digest, provider identity, timings, retry classification, and CallTrace. Missing provider usage is derived conservatively under a versioned rule.

Stage code sees only the typed result and read-only remaining-budget summary.

## 7. Cache

The canonical cache key includes adapter protocol, provider and model identity, prompt reference and digest, canonical messages, response schema digest, sampling parameters, policy digest, and privacy partition.

A cache hit must:

- verify policy and optional expiry;
- retain the source call;
- verify referenced Blobs;
- create current-run logical call and artifact provenance;
- apply the documented logical accounting;
- pass the same local schema and domain validation.

Private prompts or responses are never placed in a shared cache partition.

## 8. Provenance and privacy

Raw prompts and responses are private by default. Logs contain IDs, digests, sizes, timings, usage, and sanitized error codes. Package-safe projections must be explicitly declared and independently reviewed before they can become package occurrences.

Secrets, API keys, authorization headers, local paths, and private source content are removed from errors, traces, and exported reports.

## 9. Provider capability and failure

At command start or first use, an adapter may perform an ordinary metered capability check for endpoint reachability, model compatibility, response-schema support, and policy constraints. Failure blocks the current stage with dependency identity, policy digest, evidence, and retry-after time.

Manual resume starts a fresh attempt of the same stage and repeats the relevant check through MeteredLLM before sending ordinary work. Prior health data is diagnostic only.

## 10. Tests

Tests use a deterministic Fake adapter and cover canonical request encoding, schema validation, repair bounds, stable identity, physical CallTrace sequence, cancellation, timeout, response-size cap, unknown send boundary, conservative usage, cache provenance, privacy redaction, and fresh dependency checks after a blocked restart.
