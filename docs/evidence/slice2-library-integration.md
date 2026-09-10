# Slice 2 library integration checkpoint

Date: 2026-09-08

Follow-up: [provider configuration and durable dispatch](slice2-durable-llm-dispatch.md) records the subsequent LLM-02/LLM-03a work. Remaining-work statements below describe this earlier library checkpoint.

Scope: INT-01, INT-02, selected INT-03 behavior/regressions, and LLM-01 on `codex/phase2`, based on `50deb38`. Changes are in the working tree. This record does not claim full Slice 2, real-provider CLI, production graph, or MVP acceptance.

## Delivered

- Amended ADR-0006 and current designs for in-process LangGraphGo assembly in `internal/application` and LangChainGo adaptation in `internal/agent`. SQLite remains the authoritative progress store; the eight-stage business graph, locks, ledgers, watchdog and READY gate remain required.
- Pinned LangChainGo v0.1.14 and LangGraphGo v0.8.5 with minimum Go 1.25.0. CI now covers minimum/current Go, module tidiness/integrity, race tests, vet, architecture checks and Windows/Linux cross-builds.
- Added `agent.NewLangChain` implementing the existing MeteredLLM port. Shared code retains endpoint/DNS policy, prompt/schema bindings, canonical identities, bounded explicit retry, conservative unknown-send handling and strict raw-response validation.
- Compared canonical HTTP bytes, idempotency keys and typed outcomes against the HTTP adapter. Restored top_p omitted by the pinned SDK, explicitly selected max_tokens, and rejected SDK changes to system messages or omission of temperature before any network send. Environment-only organization/provider defaults cannot override admitted configuration.
- Retained bounded raw response bytes for strict validation and usage accounting; SDK errors/DTOs do not become persisted evidence. Explicit non-completion finish reasons reject otherwise valid JSON. Disabled Go HTTP request replay so explicit attempts own retries.
- Added graph dependency probes for concrete typed state, serial order, conditional review, checked commit errors, cancellation and absence of implicit node retries. These probes are fixtures, not a production graph or a persistence bridge.

## Failure-first evidence

1. `go test ./internal/agent -run TestLangChain -count=1` failed because `NewLangChain` did not exist before implementation.
2. `TestLangChainRejectsSDKOmittedTemperatureBeforeSend` reproduced a real bug: an SDK-omitted temperature decoded as zero and issued one HTTP request instead of being refused. Presence-aware decoding made the test pass without a send.
3. The `function-finish-without-call` parity case reproduced an upstream SDK nil-pointer panic for `finish_reason=function_call` with no function payload. The doer now rejects unsupported/non-complete choices before SDK decoding; CPGen returns the same sanitized protocol failure as the HTTP adapter.

## Verification

The host is Windows/amd64 with Go 1.26.5 and the downloaded minimum Go 1.25.0. Test commands use a task-specific temporary GOCACHE. Initial sandboxed baseline runs failed on cache/path/Windows named-pipe permissions; normal-permission runs validate those unchanged platform contracts. The default module proxy timed out, so dependency downloads used a temporary GOPROXY override to goproxy.cn; no repository proxy or credential setting was added.

| Gate | Result |
| --- | --- |
| Focused agent/application/workflow tests | PASS |
| `go test ./...` (Go 1.26.5) | PASS |
| `go vet ./...` (Go 1.26.5) | PASS |
| `go mod verify` | PASS |
| Architecture documentation check | PASS, 26 normative files plus library contracts |
| Linux CLI cross-build, Go 1.25.0, CGO disabled | PASS |
| Linux new provider adapter build, Go 1.25.0, CGO disabled | PASS |
| `go test ./...` (Go 1.25.0) | PASS |
| `go test -race ./...` (Go 1.26.5) | PASS |
| Windows CLI cross-build, Go 1.25.0, CGO disabled | PASS |
| Module tidiness under Go 1.25.0 | PASS, go.mod/go.sum hashes unchanged |
| Final formatting and patch checks | PASS |

After the final SDK panic guard, full test/vet/race and minimum-toolchain/build commands passed again. Unchanged packages reused successful test-cache entries; the modified agent package reran successfully under both toolchains and the race detector.

HTTP fixtures cover success, absent/partial/negative usage, duplicate outer and structured fields, unknown structured fields, invalid UTF-8, trailing/malformed JSON, null/empty choices, explicit truncation/filtering, Content-Length and chunked response caps, 401/403/307/429/503, no-send retry, unknown-send refusal, pre-call and in-flight cancellation/timeout, stable retry identity and eight concurrent calls. Each fixture checks actual request counts; these counts are not yet asserted against the durable ledger.

## Remaining work

LLM-02–LLM-06 and WF-01–WF-07 remain open. In particular, each authorized physical LLM dispatch must be bridged to CallCoordinator with one adapter attempt, durable result replay and usage settlement on failures; JSON repair and cache provenance need separate integration. The real provider and typed stages are not enabled in CLI configuration or bootstrap. The graph must load authoritative SQLite/Blob state and preserve all revision/review/cancel/restart behavior before replacing Fake dispatch.

No paid model calls or public similarity calls were made. No opt-in real Docker or DeepSeek smoke was run for this checkpoint. Historical Slice 0/1 evidence and existing generated tasks were not modified.

## Upstream reference points

- [LangChainGo v0.1.14 GenerateContent](https://github.com/tmc/langchaingo/blob/v0.1.14/llms/openai/openaillm.go): inspected request translation and response conversion; local contract tests pin the required behavior.
- [LangGraphGo v0.8.5 module](https://github.com/smallnest/langgraphgo/blob/v0.8.5/go.mod) and [typed graph](https://github.com/smallnest/langgraphgo/blob/v0.8.5/graph/state_graph.go): verified dependency/toolchain requirements and error propagation using the pinned source and compatibility probes.
