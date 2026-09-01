# DockerSandbox Detailed Design

Status: Current under ADR-0006

## 1. Goals and boundary

DockerSandbox executes untrusted compilation, target programs, checkers, and transfer helpers while preserving authoritative host-side evidence. It retains the docker-direct-v2 Runner and detached watchdog proven in Slice 0.

Target code never receives the Docker socket, artifact-store credentials, local database, run locks, watchdog controls, or host paths beyond explicit mounts.

## 2. Capability check

Before Docker-required work, the adapter validates:

- engine reachability and stable engine identity;
- supported API behavior;
- required image digests;
- cgroup, CPU, memory, pids, OOM, wait, and inspect evidence;
- mount, user, capability, network, and read-only-root enforcement;
- watchdog executable and private control directory;
- deterministic label capacity and event visibility.

The result is a typed compatible, incompatible, unavailable, or unknown outcome with policy and evidence digests. An incompatible host fails closed for the requested profile.

## 3. Images and roles

- cpgen-builder: pinned compiler and build tools.
- cpgen-runtime: minimal language runtime for untrusted target or checker.
- cpgen-transfer: trusted copy helper for input import and post-stop output export.

Every image reference is a content digest. Each container has one role. A transfer or keeper process is never measured as the target.

## 4. Request contracts

~~~text
CompileRequest
  RunID
  AttemptID
  SandboxExecutionID
  LogicalOperationID
  SourceOccurrence
  Language
  ToolchainDigest
  Limits
  OutputDeclaration
  ScopeDigest
  PlanDigest
  EngineIdentityDigest

RunRequest
  RunID
  AttemptID
  SandboxExecutionID
  LogicalOperationID
  ProgramOccurrence
  InputOccurrences
  Argv
  EnvironmentAllowlist
  Limits
  OutputDeclarations
  ScopeDigest
  PlanDigest
  EngineIdentityDigest
~~~

Values are strict, copied, and validated before authorization. Commands are explicit argv arrays. Requests cannot select images, mounts, Docker flags, network, users, labels, or host paths outside the audited profile.

## 5. Authorization identity

The sealed authorization identity is exactly:

~~~text
RunID
+ AttemptID
+ SandboxExecutionID
+ LogicalOperationID
+ ScopeDigest
+ PlanDigest
+ EngineIdentityDigest
~~~

A physical grant adds only CallID, resource ordinal, and call role. The foreground CLI must hold the per-run process lock before authorizing a new resource or target start. Each database command also checks the expected SandboxExecution or resource lifecycle version.

Cleanup authorization is narrower: it can inspect, stop, kill, wait, remove, and settle only an exact persisted identity. It cannot start a target, create an unplanned resource, continue an old export, or publish an artifact.

## 6. Complete resource plan

Before the first Docker create, one short transaction stores:

- SandboxExecution and its stable logical identity;
- the complete non-expanding resource set;
- deterministic container and volume names;
- expected canonical labels and call roles;
- engine identity digest;
- plan and scope digests;
- deadlines and cleanup policy;
- watchdog control-record digest;
- initial lifecycle versions.

Labels include version, RunID, AttemptID, SandboxExecutionID, LogicalOperationID, CallID where applicable, resource ordinal, role, plan digest, and engine digest. No secret, path, user text, or mutable process identifier is included.

## 7. Pre-create watchdog protocol

1. Start the watchdog with a private authenticated local control channel.
2. Send the sealed complete plan.
3. The watchdog verifies digests, subscribes to engine events, and performs a resource-kind baseline scan.
4. It acknowledges the plan.
5. Before each create, the Runner changes that resource from PLANNED to CREATING by expected lifecycle version and receives a resource-specific pre-create acknowledgement.
6. The Runner calls Docker outside the database transaction.
7. It persists engine resource ID and identity evidence.
8. The watchdog acknowledges the discovered exact resource.
9. Only then may the target start.

This order covers death before create returns, after create returns but before persistence, and before target start. Deterministic name and label identity allows later discovery without broad scanning.

## 8. Direct target execution

The Runner creates the target container with:

- explicit argv and no shell;
- non-root numeric user;
- read-only root filesystem;
- dropped capabilities and no-new-privileges;
- pids, memory, CPU, wall, output, and file-size limits;
- explicit read-only inputs and declared writable outputs;
- network disabled unless a separately audited profile requires it;
- no Docker socket or host control mount.

Trusted host code starts, waits, times, captures capped stdout and stderr, inspects OOM and resource evidence, and produces ProcessOutcome. Target verdict precedence is deterministic and remains defined by the Judge contract.

## 9. Watchdog behavior

The detached watchdog owns only the sealed plan and stop safety. It stops or kills planned targets on:

- execution deadline;
- parent control-channel EOF;
- explicit cancellation or stop command;
- watchdog policy violation.

It uses bounded Stop, Kill, Wait, Inspect, and Remove operations, records evidence, and performs final exact-identity scans. It remains until planned calls are terminal, no target is running, and required cleanup evidence is settled.

The watchdog cannot schedule a stage, choose a Judge verdict, access the artifact store, continue exports, or inspect unrelated resources.

## 10. Output transfer

Target output lives on a planned volume. After the target is proven stopped, a dedicated transfer container mounts that volume read-only and writes only to a declared trusted staging sink. The artifact writer enforces path, type, count, size, digest, and budget limits before promotion.

An incomplete export after CLI death is never resumed implicitly. Later reconciliation cleans the old execution; a fresh stage attempt may create a new logical operation.

## 11. Narrow restart reconciliation

At stateful command start and before current-stage resume, the reconciler loads unfinished SandboxExecution rows for that run only. For each exact planned resource it:

1. verifies engine identity;
2. resolves deterministic name and canonical labels;
3. compares persisted engine ID and identity evidence when present;
4. inspects and proves current state;
5. stops or kills any remaining untrusted target;
6. waits for stop evidence;
7. removes resources allowed by persisted policy;
8. settles CallTrace, budgets, and sandbox events using expected lifecycle versions;
9. marks the execution CLEANED or returns a typed pending-cleanup result.

A resource absent from the complete persisted plan is never touched. Repeating the procedure is idempotent. The reconciler cannot mutate unrelated run projections.

## 12. Cancellation and terminal states

A cancellation request stops authorization of new work and cancels the execution context. Cleanup uses an independent bounded context so target stop is not abandoned when the user context ends.

The run cannot commit CANCELLED, BLOCKED, READY, or another state promising no live target until all relevant untrusted targets are proven stopped. If cleanup exceeds the command bound, the CLI exits with the documented pending-cleanup code and a later resume repeats reconciliation.

## 13. ProcessOutcome

The outcome decision order is stable:

1. host or identity incompatibility;
2. watchdog or runner safety failure;
3. timeout and forced stop;
4. OOM or resource-limit failure;
5. output-limit failure;
6. signaled or non-zero target exit;
7. checker or protocol outcome;
8. success.

Every outcome carries CallTrace, engine and image identity, plan digest, target container identity, timings, exit and stop evidence, stdout/stderr digests, resource measurements, and artifact declarations as applicable.

## 14. Tests

Retain all Slice 0 tests and add:

- process death while target runs;
- target stop before export;
- death during cleanup;
- watchdog EOF and watchdog death;
- create return before identity persistence;
- late resource discovery by exact name and labels;
- rejection of mismatched engine, plan, label, or scope;
- refusal to touch a similar unrelated container;
- repeatable reconciliation;
- no old export continuation;
- no terminal cancellation before stop proof;
- no Docker or watchdog IPC during SQLite write transactions.
