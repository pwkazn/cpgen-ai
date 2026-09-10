# SQLite Projection, Domain Ledgers, Artifacts, and Cache

Status: Current under ADR-0006

## 1. Scope

SQLite is the local durable projection and audit store for the fixed CPGen pipeline. It is not a replay engine or general scheduler. Product-specific ledgers preserve budget, call, artifact, sandbox, review, and package integrity across process restart.

## 2. Database configuration

- one database below the private runtime directory;
- ordered, checksum-verified migrations;
- foreign_keys=ON, journal_mode=WAL, synchronous=FULL;
- busy timeout bounded by local configuration;
- explicit BEGIN IMMEDIATE for state-changing transactions;
- injected canonical clock and stable serialization;
- no external I/O, hashing, fsync, rename, verified Blob read, Docker operation, provider call, or blocking wait inside a write transaction.

Read-only commands use ordinary snapshots. Mutating run commands also hold the OS-backed run lock.

## 3. Workflow projection

The minimal tables are:

~~~text
runs(
  run_id PK,
  request_digest,
  config_digest,
  workflow_revision,
  schema_version,
  state,
  current_stage_name,
  current_stage_ordinal,
  run_version,
  active_elapsed_ns,
  active_started_at?,
  last_accounting_heartbeat_at?,
  cancellation_summary?,
  final_package_occurrence_id?,
  final_quality_report_id?,
  created_at,
  updated_at
)

stage_records(
  run_id,
  stage_name,
  stage_ordinal,
  workflow_revision,
  schema_version,
  input_digest,
  output_digest?,
  state,
  attempt_count,
  current_attempt_id?,
  logical_idempotency_key,
  last_error_code?,
  last_error_digest?,
  updated_at,
  PRIMARY KEY(run_id, stage_name)
)

stage_attempts(
  attempt_id PK,
  run_id,
  stage_name,
  ordinal,
  input_digest,
  logical_idempotency_key,
  state,
  output_digest?,
  result_kind?,
  error_code?,
  started_at,
  finished_at?,
  UNIQUE(run_id, stage_name, ordinal)
)

run_events(
  event_id PK,
  run_id,
  run_version,
  event_type,
  payload_digest,
  canonical_payload,
  created_at,
  UNIQUE(run_id, run_version)
)

control_requests(
  request_id PK,
  run_id,
  kind,
  reason_digest,
  state,
  created_at,
  applied_at?,
  UNIQUE(run_id, kind, state)
)

review_decisions(
  decision_id PK,
  run_id,
  expected_run_version,
  workflow_revision,
  stage_name,
  input_digest,
  kind,
  state,
  evidence_digest,
  policy_digest,
  payload_digest,
  created_at,
  applied_at?,
  applied_run_version?
)
~~~

Projection changes and their run event commit in one transaction. run_events are an append-only audit stream; runtime state is read directly from runs and stage_records.

## 4. Expected-version transitions

Every transition accepts expected run version and the intended current stage. A short transaction:

1. validates run state, current stage, version, and cancellation guard;
2. validates referenced attempt and domain evidence;
3. applies exactly one projection update;
4. advances run version;
5. appends exactly one ordered event;
6. commits or returns a typed conflict.

The same operation ID is replay-safe. A duplicate request returns the previously committed result. Callers never repair a conflict by writing an arbitrary state.

## 5. Budget and call ledgers

~~~text
budget_accounts(
  run_id,
  dimension,
  limit_value,
  reserved_value,
  settled_value,
  account_version,
  PRIMARY KEY(run_id, dimension)
)

call_records(
  call_id PK,
  logical_operation_id,
  run_id,
  attempt_id,
  call_kind,
  provider,
  request_digest,
  policy_digest,
  idempotency_key,
  dispatch_state,
  outcome_kind?,
  call_trace_digest?,
  authorized_at,
  completed_at?,
  UNIQUE(logical_operation_id, call_id)
)

budget_reservations(
  reservation_id PK,
  run_id,
  attempt_id,
  call_id,
  dimension,
  reserved_value,
  settled_value?,
  state,
  UNIQUE(call_id, dimension)
)
~~~

Authorization reserves all declared dimensions before irreversible work. The dispatch lifecycle distinguishes not-sent, sent, completed, unknown, and locally completed outcomes. Settlement is monotone and cannot exceed the configured account limit. Unknown send boundaries keep the original call identity and charge conservatively.

CallTrace records the physical provider or Docker interaction, timings, request and response digests, retry classification, and outcome evidence. A cache result has its own logical call evidence and references the original source call.

## 6. Artifact ledger

~~~text
artifact_declarations(
  declaration_id PK,
  run_id,
  attempt_id,
  call_id?,
  role,
  media_type,
  logical_path,
  max_bytes,
  declaration_digest
)

writer_tokens(
  writer_token_id PK,
  declaration_id,
  state,
  expected_digest?,
  expected_size?,
  final_digest?,
  final_size?,
  created_at,
  finalized_at?
)

blobs(
  digest,
  size,
  state,
  canonical_relative_path,
  verified_at?,
  PRIMARY KEY(digest, size)
)

blob_pins(
  pin_id PK,
  writer_token_id,
  digest,
  size,
  state,
  created_at,
  released_at?
)

artifact_occurrences(
  occurrence_id PK,
  run_id,
  attempt_id,
  call_id?,
  declaration_id,
  writer_token_id,
  digest,
  size,
  role,
  logical_path,
  media_type,
  revision_digest,
  provenance_digest,
  created_at
)
~~~

All provenance shadow columns are NOT NULL where an external producer exists, and composite foreign keys prove same-run ownership. Occurrences are bound to the original producing attempt and call, never to transient process ownership.

## 7. Blob write and verified-read protocol

1. Declare role, media type, logical path, and size cap in a short transaction.
2. Create a single-use writer token and budget reservation.
3. Stream bytes into a private temporary file while hashing and enforcing limits.
4. fsync outside the database transaction.
5. Atomically publish or deduplicate the canonical digest and size.
6. In a short transaction, finalize the token, settle bytes, pin the Blob, and record evidence.
7. At stage commit, create the run-scoped occurrence and release or retain the pin according to policy.

A conflicting digest-size identity, path traversal, symlink escape, corrupt verified read, or token reuse fails closed. High-trust consumers rehash before use.

## 8. Cache ledger

~~~text
cache_entries(
  cache_key PK,
  kind,
  schema_version,
  policy_digest,
  value_digest,
  expires_at?,
  state
)

cache_entry_sources(
  cache_key,
  source_call_id,
  source_occurrence_id?,
  PRIMARY KEY(cache_key, source_call_id)
)

cache_blob_refs(
  cache_key,
  digest,
  size,
  role,
  PRIMARY KEY(cache_key, digest, size, role)
)

cache_uses(
  cache_use_id PK,
  cache_key,
  run_id,
  attempt_id,
  logical_call_id,
  source_digest,
  created_at
)
~~~

Cache keys use canonical, versioned inputs. A hit checks policy, expiry, source evidence and Blob readiness, then verifies the bytes and the owning adapter's output contract before creating a current-run logical cache-hit call and reuse record. The stage transaction attaches a CACHE_REUSE occurrence to that record; it does not borrow a source writer token or charge another physical write. Source calls can belong to an earlier stage/attempt in the same run, while the current call and occurrence remain bound to the current attempt. Cross-run sources remain forbidden. A stale provider-health snapshot may be diagnostic but cannot unblock a stage without a fresh current-attempt check.

## 9. Sandbox ledger

~~~text
sandbox_executions(
  sandbox_execution_id PK,
  run_id,
  attempt_id,
  logical_operation_id,
  scope_digest,
  plan_digest,
  engine_identity_digest,
  watchdog_control_digest,
  state,
  lifecycle_version,
  created_at,
  cleaned_at?
)

sandbox_resources(
  resource_id PK,
  sandbox_execution_id,
  ordinal,
  kind,
  call_role,
  deterministic_name,
  expected_labels_digest,
  engine_resource_id?,
  identity_evidence_digest?,
  state,
  lifecycle_version,
  UNIQUE(sandbox_execution_id, ordinal)
)

sandbox_events(
  sandbox_event_id PK,
  sandbox_execution_id,
  resource_id?,
  lifecycle_version,
  event_type,
  evidence_digest,
  created_at
)
~~~

SandboxExecution and the complete non-expanding planned resource set commit before Docker create. Authorization binds RunID, AttemptID, SandboxExecutionID, LogicalOperationID, ScopeDigest, PlanDigest, and EngineIdentityDigest. Physical grants add only call ID, resource ordinal, and role.

Deterministic names and labels plus engine identity allow the watchdog and later reconciler to inspect and clean exact resources. Cleanup commands use expected lifecycle versions and remain valid after cancellation. They cannot authorize target start, continue export, or touch a resource absent from the persisted plan.

## 10. Package ledger

~~~text
packages(
  package_id PK,
  manifest_digest,
  tree_digest,
  created_at
)

package_occurrences(
  package_occurrence_id PK,
  package_id,
  run_id,
  relation,
  state,
  verification_receipt_id?,
  final_quality_report_id?,
  created_at,
  verified_at?
)

verification_receipts(
  verification_receipt_id PK,
  package_id,
  structure_digest,
  semantic_digest,
  toolchain_digest,
  created_at
)
~~~

A package occurrence becomes VERIFIED only after structural and semantic gates pass. A deferred same-run constraint requires runs in READY to reference a VERIFIED occurrence with a final quality report. Publication and READY projection update commit atomically.

## 11. Review and control integrity

At most one active cancellation request and one active PENDING review decision exist per run. Cancel insertion is the only mutating command allowed without the run lock while a foreground executor owns it; the transaction only inserts the request. The lock-holding process observes and applies it.

Review creation is allowed only in NEEDS_REVIEW and records the expected version and evidence bindings. Applying a review occurs during manual resume under the run lock.

## 12. Restart reconciliation

On resume, named idempotent operations inspect only the current run and its domain ledgers:

- close or replay the current stage attempt;
- reconcile the original provider call identity;
- verify published Blob state and writer tokens;
- settle reservations conservatively;
- stop and clean exact SandboxExecution resources;
- apply an already-committed stage result or start a fresh attempt.

There is no database table whose rows schedule arbitrary recovery work. Each operation validates expected run or lifecycle version before commit.

For real LLM stage composition, M22 retains immutable logical opening and completion commands alongside their call records. Each bounded receipt commits atomically with the corresponding call transition and is verified against the existing command digest on read. The application stage-bound ledger can refresh optimistic versions after active-time accounting while replaying the exact original logical command. It checks the current RUNNING stage attempt on every mutation, retries only bounded database version conflicts and never retries provider I/O. Legacy calls without retained command metadata require their exact original command for adoption. This bridge is a composition prerequisite; interrupted-attempt reconciliation must still resolve already-dispatched identities before any new attempt.

## 13. Garbage collection

GC is an explicit maintenance command. Stateful run commands hold a shared global artifact lock; GC takes the exclusive form, selects only unreferenced READY Blobs, rechecks references in a short transaction, moves bytes to private trash outside the transaction, then records deletion. Crash recovery can restore or finish trash entries idempotently.

## 14. Tests

Required tests cover migrations, foreign keys, expected-version conflicts, atomic projection plus event, budget races, duplicate call settlement, Blob traversal and corruption, cache provenance, package READY constraints, crash injection at every durable boundary, and verification that no external I/O executes inside a SQLite write transaction.
