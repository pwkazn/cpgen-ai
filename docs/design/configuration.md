# Configuration Model

Status: Current under ADR-0006

## 1. Sources and precedence

Configuration is local and layered in this order:

1. compiled safe defaults;
2. the explicit application configuration file;
3. environment variables for secrets and narrowly documented overrides;
4. CLI flags;
5. GenerationRequest values allowed by policy.

After merge, CPGen validates, canonicalizes, redacts, and hashes the effective configuration. The immutable redacted digest is stored on the run. Secrets are referenced by name and never persisted.

## 2. ApplicationConfig

The current local YAML schema is documented in [config/README.md](../../config/README.md). It accepts `storage`, `sqlite`, `runtime`, `fake_workflow` and optional strict `llm`, `similarity`, `workflow` and `sandbox` blocks. [deepseek.example.yaml](../../config/deepseek.example.yaml) remains a provider-only Fake example; [slice2.example.yaml](../../config/slice2.example.yaml) explicitly selects the compiled live preview. [solution.example.yaml](../../config/solution.example.yaml) enables accepted Similarity to continue through Solution generation and real Docker sample verification, with a required local Engine endpoint, absolute lock path and canonical lock digest. Bootstrap checks the actual lock and installed images; configuration validation itself is read-only and offline. Provider blocks alone preserve Fake selection. Both live selectors require frozen provider/decision settings and positive per-exchange cost ceilings. Retry and selection rules remain compiled. Omitting new blocks preserves previous effective bytes and digests. Nulls, duplicate/unknown fields, invalid scalar types and invalid limits are rejected. No environment value is expanded into a snapshot.

`application.BuildLLMConfig` binds the built-in prompt/schema registries, returns explicit output limits, and fixes adapter attempts to one. The optional format-repair allowance is zero or one; its zero default remains omitted from effective JSON. An explicit workflow selector composes the durable preview runtime, while omitted selection retains the Slice 1 Fake pipeline. Live resume/cancellation require the original effective digest. The broader configuration below describes the target design and is not a copy-pasteable local CLI configuration.

~~~yaml
schema_version: 1

runtime:
  private_dir: .cpgen
  database_path: .cpgen/cpgen.sqlite
  run_lock_dir: .cpgen/locks/runs
  artifact_lock_path: .cpgen/locks/artifacts.lock
  busy_timeout: 5s
  command_shutdown_timeout: 20s
  active_time_heartbeat_interval: 5s

workflow:
  revision: phase1-v1
  max_stage_attempts: 3
  retry_base_delay: 500ms
  retry_max_delay: 5s

artifacts:
  root: .cpgen/artifacts
  staging_root: .cpgen/staging
  trash_root: .cpgen/trash
  verified_reads: true
  max_single_artifact_bytes: 67108864

budgets:
  max_llm_calls: 40
  max_llm_input_tokens: 200000
  max_llm_output_tokens: 80000
  max_llm_cost_microunits: 5000000
  max_similarity_calls: 10
  max_sandbox_runs: 200
  max_artifact_bytes: 1073741824
  max_active_wall_time: 30m

llm:
  adapter: fake
  endpoint: ""
  model: ""
  api_key_env: CPGEN_LLM_API_KEY
  timeout: 30s
  max_response_bytes: 1048576

similarity:
  adapter: fake
  endpoint: ""
  api_key_env: CPGEN_SIMILARITY_API_KEY
  timeout: 10s
  max_response_bytes: 1048576

sandbox:
  engine_endpoint: local
  builder_image: cpgen-builder@sha256:...
  runtime_image: cpgen-runtime@sha256:...
  transfer_image: cpgen-transfer@sha256:...
  watchdog_path: cpgen-watchdog
  watchdog_control_dir: .cpgen/watchdog
  stop_grace: 2s
  cleanup_timeout: 20s
  network_enabled: false

logging:
  level: info
  format: text
  redact_private_content: true
~~~

All runtime and storage paths resolve beneath an explicitly selected private workspace unless a documented administrator policy permits an absolute path. The run-lock path is derived from validated RunID and cannot be supplied by a request.

## 3. GenerationRequest

~~~yaml
schema_version: 1
title_hint: ""
topic_tags: [graphs]
difficulty:
  lower: 1800
  upper: 2200
constraints:
  time_limit_ms: 2000
  memory_limit_mb: 512
languages: [cpp]
similarity:
  required: true
  threshold: 0.82
output:
  format: internal
~~~

Request fields are strictly decoded. Unknown fields, duplicate keys, invalid Unicode, non-finite numbers, unsafe paths, unsupported languages, contradictory ranges, and policy violations are rejected before a run is created.

## 4. Validation

Validation checks:

- schema version and workflow revision compatibility;
- private-directory permissions;
- database, lock, artifact, staging, trash, and watchdog paths do not alias unsafely;
- positive durations and bounded integers;
- budget totals fit exact SQLite integer representation;
- image references are immutable digests;
- model and similarity endpoints use HTTPS unless the explicit local-test policy allows otherwise;
- endpoint host allowlists and redirect policy;
- secret environment variable names exist without reading them into snapshots;
- active-time interval is smaller than the relevant deadline;
- Docker and artifact cleanup timeouts are bounded.

There is no configuration for a scheduler endpoint, background worker, task queue, or arbitrary stage graph.

LangGraphGo is a pinned build dependency selected by a compiled workflow revision, not a configurable scheduler backend. LangChainGo uses the redacted adapter Config. The local `llm` block supplies request output ceilings and a response envelope cap; typed GenerateRequest carries the stage's admitted limits. The DeepSeek example uses `https://api.deepseek.com` and `deepseek-v4-flash`, with credentials referenced by environment name only. Durable transport retries are owned by CallCoordinator, not by YAML or library retry settings. The preview and Solution selectors are explicitly available through CLI; the complete Data/Judge/Quality/Package selector remains internal until those stages are implemented.

## 5. Local locking and accounting values

runtime.run_lock_dir stores one deterministic OS lock file per RunID. runtime.artifact_lock_path provides shared use for stateful commands and exclusive use for GC. Locks are local process primitives and have no heartbeat settings.

runtime.active_time_heartbeat_interval controls accounting only. The timestamp supports conservative post-crash charging and never grants execution ownership.

## 6. Provider and sandbox configuration

Adapters receive a normalized, redacted value object. Stage code never reads environment variables directly. HTTP policy includes DNS and IP validation, redirects disabled by default, response caps, decompression caps, and timeouts.

Docker images are digest-pinned. Sandbox configuration selects an audited profile; requests cannot weaken mounts, network, credentials, capabilities, resource limits, watchdog behavior, or exact identity labels.

## 7. Changes and compatibility

A configuration change creates a new digest. Resume validates that immutable fields match the run and rejects incompatible workflow or schema changes. Explicit revision creates a new domain revision and downstream invalidation; it never silently mutates old evidence.

## 8. Commands and tests

cpgen config check validates configuration and an optional request without persisting state. Tests cover precedence, strict decoding, canonical digests, secret redaction, unsafe path aliases, invalid timing and budget values, immutable images, endpoint policy, and compatibility checks.
