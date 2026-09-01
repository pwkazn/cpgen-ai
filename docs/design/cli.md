# CLI Contract

Status: Current under ADR-0006

## 1. Principles

The CLI is the only Phase 1 interface. Commands open local resources, perform bounded foreground work, print stable output, and exit. There is no background workflow process or remote control endpoint.

Human output goes to stderr for progress and stdout for results. JSON mode emits one versioned envelope and no decorative text. Secrets and raw private prompt or source content are never logged.

## 2. Global options

~~~text
--config PATH
--workspace PATH
--json
--quiet
--log-level LEVEL
~~~

Paths are normalized before use. The runtime directory, database, locks, artifacts, staging, and watchdog controls must remain private.

## 3. Configuration and diagnostics

~~~text
cpgen config check [--request request.yaml]
cpgen doctor [--docker] [--providers]
~~~

config check parses, merges, canonicalizes, redacts, and validates configuration without changing run state. doctor reports typed capability results and does not persist workflow progress.

## 4. Generate and run commands

~~~text
cpgen generate --request request.yaml
cpgen run list [--state STATE]
cpgen run show <run-id>
cpgen run events <run-id> [--after-version N]
cpgen run resume <run-id>
cpgen run cancel <run-id> --reason "..."
~~~

generate creates a run and executes the compiled pipeline until READY, BLOCKED, NEEDS_REVIEW, FAILED, or CANCELLED.

run list, show, and events are read-only and do not acquire the run execution lock. events are ordered by run version and support stable pagination.

run resume is the only ordinary restart command. It acquires the deterministic per-run process lock, reconciles exact unfinished sandbox resources, and resumes the current compiled stage. If the prior process exited during an attempt, resume records or replays that attempt from persisted domain evidence before creating new work.

run cancel inserts one idempotent cancellation request. When an executor is active, it observes the request and stops. When no executor owns the run lock, cancel may acquire it and reconcile exact sandbox resources before committing CANCELLED.

### Process-lock conflict

If generate or resume finds the same run already locked, it returns exit code 4 and a typed StateConflict containing RunID and operation. It does not wait indefinitely, alter state, or start a stage. Different runs remain independent.

## 5. Review commands

~~~text
cpgen review show <run-id>
cpgen review revise <run-id> --patch FILE --reason TEXT
cpgen review retry <run-id> --reason TEXT
cpgen review waive <run-id> --policy RULE --reason TEXT
cpgen review reject <run-id> --reason TEXT
~~~

A mutating review command is valid only for NEEDS_REVIEW and creates one immutable PENDING ReviewDecision bound to expected run version, workflow revision, current stage input, evidence, and policy. It does not directly continue the run. The user follows with run resume.

show renders the current review checkpoint, pending decision, budgets, and referenced evidence.

## 6. Package commands

~~~text
cpgen package verify PATH
cpgen package export <run-id> --format internal|polygon --output PATH
cpgen gc
~~~

verify treats the input as untrusted and performs structural and semantic checks without modifying a run. export requires a verified package occurrence and writes through a private staging directory before atomic publication. gc is explicit maintenance and takes the exclusive artifact lock.

## 7. Restart semantics

- CREATED starts the first compiled stage.
- RUNNING means the prior command may have exited; resume reconciles the current attempt from durable evidence.
- BLOCKED starts a new attempt of the same stage and revalidates its exact dependency.
- NEEDS_REVIEW requires one applicable pending decision.
- READY, FAILED, and CANCELLED reject resume.
- A retry-after time in the future returns a typed blocked result; no background timer waits for it.
- An unknown external send boundary is never resent under a new idempotency key.

## 8. Exit codes

| Code | Meaning |
|---:|---|
| 0 | success, including READY or successful read-only command |
| 2 | invalid arguments, request, configuration, or input package |
| 3 | BLOCKED |
| 4 | state, version, review, or process-lock conflict |
| 5 | NEEDS_REVIEW |
| 6 | FAILED |
| 7 | CANCELLED |
| 8 | budget exhausted |
| 9 | sandbox or host capability incompatible |
| 10 | safe cleanup remains pending; run is not terminal |
| 70 | unexpected internal error |

Exit code 10 means a later resume must repeat exact-resource cleanup. The CLI prints the run ID and current durable state without claiming completion.

## 9. JSON envelope

~~~json
{
  "schema_version": 1,
  "command": "run.resume",
  "ok": false,
  "run_id": "run_...",
  "state": "BLOCKED",
  "run_version": 12,
  "result": {},
  "error": {
    "code": "dependency_unavailable",
    "message": "sanitized message",
    "retryable": true
  }
}
~~~

The schema is additive within a version. IDs, states, event versions, decision IDs, budget summaries, and package occurrence IDs are stable machine fields.

## 10. Acceptance

Subprocess tests verify lock conflicts, process death and resume, cancel while active, stable JSON, exit-code mapping, no secret leakage, and no progress text on stdout in JSON mode.
