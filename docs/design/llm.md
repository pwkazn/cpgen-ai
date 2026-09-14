# LLM, Prompt, and Structured-output Design

Status: Current under ADR-0006

Real Idea/Statement assembly uses strict versioned content drafts and local domain binding. Models do not calculate domain hashes or choose request/resource identities. Built-in historical prompt versions remain available for receipt validation; the new draft prompts and their one-call format repairs have separate exact schema/template references. See [content draft evidence](../evidence/slice2-content-drafts.md).

## 1. Boundary

Model calls are ordinary stage-local effects through MeteredLLM. The coordinator and stage code depend on a provider-neutral interface; provider fields remain inside adapters. Model output is always a candidate that deterministic validation may reject.

The model port cannot access SQLite, artifact directories, Docker, run locks, or unrestricted logs.

The 2026-09-08 ADR-0006 amendment selects LangChainGo `v0.1.14`, confined to `internal/agent`, with `port.MeteredLLM` unchanged. `NewLangChain` shares the existing HTTP adapter's endpoint, canonical identity, prompt/schema registry, error classification and strict response validation. Contract tests compare both adapters before application wiring changes.

The library's HTTP doer is restricted to one physical request per invocation. CPGen restores the admitted top_p value (dropped by this library version), selects max_tokens explicitly, rejects other semantic request rewrites, and forwards only canonical CPGen headers/body to the policy-controlled transport. Ambient OpenAI environment configuration cannot override the configured provider or add organization headers. SDK error strings and lossy response DTOs are never persisted; the original capped bytes determine validation and missing-usage accounting. Explicit non-completion finish reasons, including length and content_filter, reject even syntactically valid JSON.

The standalone adapter does not provide durable metering by itself. The LLM-03 dispatch checkpoint adds `port.PhysicalLLM` and `execution.LLMCalls`: planning validates the same request and endpoint policies without I/O; one `GeneratePhysical` call performs at most one HTTP exchange and returns accounting even on failure. CallCoordinator reserves the complete retry plan, supplies physical identities, settles each response and returns the database CallTrace. Logical response usage sums settled reservations across attempts. Missing usage and cost without verified pricing are charged at their explicit reservation ceilings; this is conservative accounting, not a provider invoice.

Only a newly issued grant from a PREPARED row authorizes a send. Resuming DISPATCHING/SENT without a sealed receipt resolves to UNKNOWN without another HTTP request. `NewReplayableLLMCalls` reserves private response artifact slots before provider dispatch and publishes a request-bound receipt through the existing Blob writer/pin protocol. Recovery finishes SEALED publication, verifies bytes and revalidates the strict schema before restoring provider accounting. A sealed publication failure keeps its reservations for local reconciliation. The executor must hold the run lock and shared artifact-maintenance lock. The ledger-only constructor retains `ErrLLMReplayUnavailable`; neither constructor recreates completed responses with another paid request.

Private receipt bodies contain structured output, allowlisted metadata and accounting; credentials and prompt variables are excluded. The returned `RawBlob` is pending occurrence evidence for atomic stage commit, not a package export. Unused slots release byte reservations; finalized receipts remain pinned until attachment. Local publication receives its own artifact grant and reaches a terminal producing call atomically with stage attachment and byte settlement. Rejected-stage cleanup charges already-published physical bytes and releases the private pin. Full verification status is tracked in [replay evidence](../evidence/slice2-private-llm-replay.md) and the later cache checkpoint. Bounded JSON repair and private cache provenance are implemented as described below. Explicit preview, Solution and full MVP selectors now wire real stages through the CLI; omitted selection retains Fake behavior. See [MVP acceptance](../evidence/mvp-package-commit-foundation.md).

After a dispatch attempt, receipt settlement uses a bounded five-second context that survives caller cancellation. SQLite accepts a stale receipt version only when the run advanced exactly once due to a matching pending cancel, and the same stage/attempt remains RUNNING; other version conflicts remain errors. Planning, authorization and retry waits retain the caller's cancellation context. Retry waits honor the later of persisted backoff and the adapter's Retry-After value, capped separately at one minute.

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

`execution.StructuredLLMCalls` permits at most one configured JSON-format repair. Before the original call it binds the complete repair policy, compiled prompt version and implementation revision into the provider policy digest, and preflights both prompt/schema bindings. Recovery with a different allowance or prompt is rejected. Configuration defaults to zero repairs and accepts only zero or one.

The repair request includes the original task variables and canonical local error codes. It never includes rejected model output, provider-controlled field paths or fragments. Eligible errors are JSON syntax, duplicate/unknown fields, schema version and typed decoding failures. Domain-semantic rejection, HTTP/envelope failures, truncation, oversized output, invalid UTF-8 and missing local validators do not trigger format repair.

Eligible validation failures are persisted as bounded `cpgen.llm-validation/v1` private receipts containing code-only diagnostics and accounting. `ReadFormatRepair` verifies the receipt against the exact terminal physical failure before it can authorize repair planning. The one repair has its own deterministic logical identity and full durable transport plan; it cannot recursively repair its own rejection. `StructuredLLMResult` returns each call's trace and artifact separately and sums settled usage across both calls. The final response keeps the producing call's trace and usage. A successful stage must attach both validation evidence and repaired output in its atomic occurrence commit.

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

`execution.StructuredLLMCache` implements a private, same-run response cache. It normalizes logical call identity while retaining the original provider request policy and all semantic inputs. Only stage-committed successful output can be published; a repaired success is verified through its original rejection and deterministic repair call. The source is the terminal local artifact-producing call, and its private receipt retains the exact original provider call.

`CacheService.ReuseValidated` performs the owning adapter's strict validation before creating current-call provenance. A hit returns a zero-usage response with a CACHE_HIT trace and `PendingCacheReuse`, with no new writer token or provider physical calls. Later attempts in the same run are allowed by migration 21; cross-run sources remain forbidden. Cache keys are immutable and private entries have no configured TTL in this bridge. Missing/corrupt files, invalidated entries and changed policies cannot silently provide an output. The application factory still needs to compose this bridge with generation and stage commits.

## 8. Provenance and privacy

Raw prompts and responses are private by default. Logs contain IDs, digests, sizes, timings, usage, and sanitized error codes. Package-safe projections must be explicitly declared and independently reviewed before they can become package occurrences.

Secrets, API keys, authorization headers, local paths, and private source content are removed from errors, traces, and exported reports.

## 9. Provider capability and failure

At command start or first use, an adapter may perform an ordinary metered capability check for endpoint reachability, model compatibility, response-schema support, and policy constraints. Failure blocks the current stage with dependency identity, policy digest, evidence, and retry-after time.

Manual resume starts a fresh attempt of the same stage and repeats the relevant check through MeteredLLM before sending ordinary work. Prior health data is diagnostic only.

## 10. Tests

Tests use a deterministic Fake adapter and cover canonical request encoding, schema validation, repair bounds, stable identity, physical CallTrace sequence, cancellation, timeout, response-size cap, unknown send boundary, conservative usage, cache provenance, privacy redaction, and fresh dependency checks after a blocked restart.
