# LangChain SDK compatibility policy (2026-09-10)

CPGen now retains LangChainGo's serialized request, including model-specific
message conversion, omitted sampling options and token field selection. The
previous field equality check and forced temperature/top_p restoration are
removed. This supersedes the request policy described in the historical
APINode acceptance report; that report still describes the run at its commit.

Serialization happens locally before budget admission with a capture-only
HTTP client and a placeholder token. Planning performs no network or credential
I/O, and the input bound uses the actual serialized byte count. CPGen then
dispatches those bytes through its existing endpoint/authentication transport.
The SDK does not consume provider responses or perform physical retries.

The existing request_digest continues to identify business intent for persisted
receipt compatibility. New wire_request_digest metadata records the hash of
actual dispatched bytes and survives private receipt metadata filtering. Old
receipts remain readable without that optional field.

Response schema validation, bounded raw response decoding, dispatch accounting,
cancellation, endpoint policy and credentials remain under CPGen control.
Additional SDK JSON fields are accepted; capture still bounds the request size
and rejects malformed JSON and repeat capture invocations.

Local regression coverage exercises GPT-5 sampling omission, o1/o3 message
conversion, token limits, exact planned byte counts, wire evidence, credential-free
planning, HTTP failure/response validation parity and concurrent requests.
No paid provider call is required for this change.
Validation passed: go test ./..., go test -race ./internal/agent, go vet ./..., and git diff --check.
