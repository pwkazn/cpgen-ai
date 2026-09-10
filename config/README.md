# Local provider and workflow configuration

`solution.example.yaml` selects `slice3.idea.statement.similarity.solution.checkpoint.v1`:
ACCEPT continues to Reference/Brute generation and real Docker sample checking;
other business decisions enter review. A passing Solution stops at
`solution_checkpoint` with exit code 6. This selector cannot produce READY. Historical preview
and Fake selectors keep their existing behavior.

`mvp.example.yaml` selects the full ordinary-problem workflow. Similarity ACCEPT
continues through Solution, reproducible data, Judge, Quality and Package. READY
requires the current verified package transaction. Nonaccepted business results
enter review; automatic mutation remains deferred. Independent CLI crash recovery, export and execution revalidation pass; see [package evidence](../docs/evidence/mvp-package-commit-foundation.md).

After configuring endpoints, credentials, local paths and the pinned toolchain,
use `mvp.request.yaml` for an explicitly budgeted run:

```powershell
go run ./cmd/cpgen --config config/mvp.example.yaml generate --request config/mvp.request.yaml
go run ./cmd/cpgen --config config/mvp.example.yaml run show RUN_ID
go run ./cmd/cpgen --config config/mvp.example.yaml run export RUN_ID --output D:/output/problem.zip
```

Replace `RUN_ID` with the returned identifier. Export requires READY, rechecks
the current committed proof, and refuses to replace an existing destination.
The destination directory must exist and support hard links. Positive example
budgets permit external calls only when you invoke generation; no example
contains credentials. Keep the exact effective configuration for resume/export.

For either forward selector, configure the required `sandbox` mapping:

| Field | Required value |
| --- | --- |
| `engine_endpoint` | Explicit local `npipe:////./pipe/docker_engine` on Windows or `unix:///var/run/docker.sock` on Linux; remote TCP endpoints are rejected |
| `toolchain_lock_path` | Absolute path to the image/toolchain lock |
| `toolchain_lock_digest` | Canonical SHA-256 digest printed by `cpgen-image-lock`; replace the example's all-zero placeholder |

`config validate` checks the closed YAML without reading the lock or contacting
Docker. Bootstrap checks the actual lock digest, local Engine capability and
installed pinned images before generation. The running CLI launches its own
detached watchdog. Build images with `go run ./cmd/cpgen-image-lock --output
<absolute-lock-path>` using the existing offline toolchain/image prerequisites.
All sandbox fields participate in the effective configuration digest. Keep the
original file/path/settings available when resuming a run.

The existing zero-budget request can also exercise this selector; it stops at
Idea without provider dispatch, but Bootstrap still requires the configured
local Docker installation. Positive-budget requests must cover separately
reserved transport attempts, artifacts and container creates. A two-sample
passing Solution currently uses 16 container creates. Examples are illustrative
limits, not provider prices or universal workload estimates.

`deepseek.example.yaml` is a credential-free integration target. Replace its
`storage.state_root` with a private absolute directory before using it. The
endpoint/model pair follows TODO LLM-02; this example makes no live availability
claim. This provider-only example runs the Fake workflow. Provider configuration
alone does not select live execution.

`slice2.example.yaml` explicitly selects the compiled live preview workflow:
Idea → Statement → Similarity → `slice2_checkpoint`. It requires both provider
blocks and positive per-exchange cost ceilings. The final checkpoint retains
the evaluated Similarity decision and pauses for non-waivable review. This
revision does not apply the later business-routing/mutation policy or produce
READY; Solution, Data, Judge, Quality and Package gates remain required.

After replacing `storage.state_root` with a private absolute path, the included
`slice2-zero-budget.request.yaml` exercises admission without provider dispatch:

```powershell
go run ./cmd/cpgen --config config/slice2.example.yaml config validate
go run ./cmd/cpgen --config config/slice2.example.yaml config effective --redact
go run ./cmd/cpgen --config config/slice2.example.yaml generate --request config/slice2-zero-budget.request.yaml
```

The final command intentionally returns `NEEDS_REVIEW` at Idea (CLI exit code
6; `go run` itself reports a nonzero child exit). For actual generation, set
provider endpoints/model/service identities, credentials and request budgets.
The example ceilings are illustrative reservation limits, not quoted prices.
Transport policy allows two separately reserved physical attempts per logical
call; format repair, when enabled, has its own call and reservations.

All effective settings are frozen on creation. Resume and cancellation through
the live service reject changed configuration before mutation. Changing only
an environment variable's credential value leaves the digest unchanged. Keep
the original configuration available for existing runs.

The entire `llm` mapping is optional. When absent, the previous effective JSON
bytes and digest are preserved. When present, `base_url`, `model` and
`api_key_env` are required. `api_key_env` is an environment variable **name**;
neither configuration loading, validation, effective output, hashing nor
application mapping looks up its value or checks whether it is set. Credential
availability and validity are checked at provider dispatch.

| Field | Default when omitted | Accepted values |
| --- | --- | --- |
| `base_url` | required | HTTPS base URL, at most 2048 bytes; DNS host or non-local unicast IP literal, optional valid port/path; no credentials, query, fragment, percent escapes or dot path segments |
| `model` | required | Non-empty UTF-8, at most 256 bytes; no surrounding whitespace, controls or environment interpolation |
| `api_key_env` | required | `[A-Za-z_][A-Za-z0-9_]*`, at most 256 bytes |
| `timeout` | `30s` | Positive Go duration, at most `10m` |
| `max_output_tokens` | `4096` | Integer from 1 to 1048576; application policy bound, not a provider capability claim |
| `max_response_bytes` | `1048576` | Integer from 1 to 67108864 |
| `max_format_repairs` | `0` | Integer 0 or 1; one optional JSON-format regeneration, separately metered |

Unknown and duplicate keys, aliases/merge keys, nulls, incorrect scalar types,
empty required strings and invalid or out-of-range values are rejected with
field-qualified errors. Defaults apply only to omitted limit fields. There are
no configurable prompt, schema, raw API key, HTTP test bypass or adapter retry
fields. Timeouts are normalized to duration strings in effective output; all
provider fields, including the credential reference, participate in the digest.
The disabled repair default is omitted from effective JSON to preserve older
provider snapshots; enabling it changes the effective policy digest.

`application.BuildLLMConfig(config.Config)` returns `(agent.Config,
port.OutputLimit, error)`. It rejects an absent provider, fixes `MaxAttempts` to
**1**, and installs fresh built-in Idea/Statement prompt and schema registries.
Schema digests identify compiled domain decoding/validation contracts; prompts
are versioned definitions, with domain and input-chain validation still
performed by the typed stage executor. This helper performs no credential or
network I/O and does not construct a workflow. Compiled format-repair variants
use the same output schemas. `BuildFormatRepairPolicy(config.Config, step)`
selects the Idea or Statement repair variant and the configured allowance.

`NewStructuredLLMCalls` binds that policy before the first paid call, validates
both prompt bindings locally, and permits one repair only for allowlisted JSON
format errors. It passes original task input plus sanitized error codes;
rejected model text is never copied into the repair prompt. Transport and domain
failures do not replenish or consume this format allowance. Each logical call
retains its own trace, private artifact and durable usage accounting.

The returned output limit must be applied to `GenerateRequest.MaxOutput` before
durable ledger admission; stages may choose smaller limits. Its byte ceiling
also caps the entire provider HTTP response, so envelope overhead counts toward
that cap. `agent.Config` itself has no token-limit field. Transport retries
belong to the durable call ledger. DNS/IP validation, redirects, decompression
limits and timeouts remain enforced by the existing adapter at dispatch; config
validation does not resolve hostnames.

The optional `workflow` fields are `revision` (the exact preview, Solution or MVP revision),
`idea_count` (2–8, default 4), `llm_cost_upper_bound_micro_usd` and
`similarity_cost_upper_bound_micro_usd` (required positive integers, each at
most `MaxInt64/8`). No arbitrary stage order, prompt, retry program or model
instruction is accepted from YAML.

The optional `similarity` block requires `endpoint`, `api_key_env`,
`provider_identity`, `service_identity`, `policy_ref`, `acceptance_threshold`
and `rejection_threshold`. Endpoint and credential-reference validation follow
the LLM rules. Thresholds must be finite in [0,1], with acceptance ≤ rejection;
the whole gap is the review band. `timeout` defaults to `30s` (maximum `10m`),
`max_response_bytes` to 1048576 (maximum 67108864), `limit` to 20 (1–10000),
and `minimum_hits` to 1 (1–limit). Index or service revisions participate in
the frozen provider policy. Omitting `workflow` and `similarity` preserves the
earlier effective bytes; provider blocks can be present without live selection.

Physical-call replay and terminal provider reconciliation are integrated with
the preview run service. Its acceptance uses local HTTP fixtures; no paid
provider or Docker smoke result is claimed.
