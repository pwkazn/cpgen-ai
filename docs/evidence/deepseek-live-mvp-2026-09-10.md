# DeepSeek live MVP attempt — 2026-09-10

Status: blocked by provider balance; no end-to-end success is claimed.

The requested endpoint is `https://api.deepseek.com`, with model
`deepseek-v4-flash`. The user explicitly selected local fixture Similarity;
model generation and Docker execution are intended to remain real. Credentials
are supplied through `CPGEN_DEEPSEEK_API_KEY` only, never in this document,
configuration files or committed source.

## Observed result

- Docker doctor returned HEALTHY for Engine 29.7.2 and all three pinned images.
- Authenticated GET /models succeeded. The returned IDs were `deepseek-flash`
  and `deepseek-v4-pro`; the requested model name was not listed. It was not
  silently replaced. Model-name compatibility remains unverified.
- Formal run `run_c064c82a36d7c9d76c24a7adb42e2bf7` stopped at
  `NEEDS_REVIEW / idea`; no Similarity fixture request or sandbox create occurred.
- The durable model call was rejected as a protocol failure. A separate minimal
  chat diagnostic returned HTTP 402, `invalid_request_error`, with provider
  message `Insufficient Balance`. Authentication does not establish available
  balance. No further paid requests were sent after this diagnosis.
- One LLM-call allowance was consumed in the formal run. Its conservative
  token/cost settlement is a local budget record, not an actual provider bill.
  Model-list and direct diagnostic probes are outside that run's ledger; an
  earlier interrupted probe's result was not recovered.

Private evidence remains under `D:/cpgen-private/deepseek-live-20260910-01`:
`config.yaml`, `request.json`, `result.json`, `events.json`, `budget.json` and
SQLite/blob state. The repository's ignored `.tmp/deepseek-live-01.log` records
the failed test, and `.tmp/deepseek-live-doctor.json` records Docker readiness.
No ZIP was produced.

## Reusable acceptance entry

The entry was subsequently generalized as `TestLiveProviderMVPWithFixtureSimilarity` and remains disabled by default. To enable it,
supply `CPGEN_RUN_LIVE_MVP=1`, `CPGEN_RUN_DOCKER_CANARY=1`, an absolute
`CPGEN_DOCKER_TOOLCHAIN_LOCK`, the credential environment variable, and a fresh
absolute private `CPGEN_LIVE_ROOT`. `CPGEN_LIVE_BASE_URL` and `CPGEN_LIVE_MODEL` explicitly select the endpoint and model; `CPGEN_LIVE_API_KEY` supplies the credential. There is no implicit provider/model default.

The test uses the production model adapter without an injected HTTP client.
Only the Similarity adapter receives a local TLS fixture transport. It retains
private run evidence, requires READY, exports with a separately built CLI after
removing provider credentials, and recompiles/revalidates the ZIP with a fresh
store. A root marker prevents accidental reruns from silently creating another
paid run. Independent verification budgets scale with the validated case index.

Default opt-out test compilation and application vet pass. The live invocation
failed as recorded above; export and execution revalidation in this new live
entry remain unexecuted until the provider account can generate responses.
The earlier all-fixture acceptance remains separate evidence.

Next: after balance is available, resolve any model-name incompatibility from
an actual provider response and rerun with an explicitly budgeted fresh root.
Do not waive the stopped run's review state or substitute model-generated
content to manufacture a passing result.