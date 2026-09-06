# Slice 1 Lightweight Local Workflow Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the recoverable Phase 1 core as one foreground Go CLI executor per run, using a minimal SQLite stage projection and CPGen-specific effect ledgers without introducing a workflow-hosting service or general durable runtime.

**Architecture:** A deterministic per-run OS file lock provides single-host execution ownership; short SQLite transactions persist runs, fixed stages, audit events, reviews, budgets, calls, artifacts, and sandbox identities. The concrete typed pipeline is compiled into Go, external I/O occurs outside write transactions, and restart recovery reruns or reconciles only the current domain stage.

**Tech Stack:** Go toolchain 1.24.13 with `go 1.24.0`, `database/sql`, pure-Go `modernc.org/sqlite v1.45.0`, `go.yaml.in/yaml/v3 v3.0.3`, `golang.org/x/sys` file-lock primitives, SHA-256 content addressing, and the existing `docker-direct-v2` Runner/watchdog.

**Spec:** `docs/superpowers/specs/2026-08-31-lightweight-local-workflow-design.md`

## Global Constraints

- Work only in `.worktrees/phase1` on `codex/phase1`; preserve the completed Slice 0 checkpoint and evidence.
- Keep `go 1.24.0`; do not add Temporal, LangGraph, AutoGen, CrewAI, a daemon, task queue, or workflow service.
- Support one host and one foreground executor per run; different runs may execute concurrently.
- The fixed pipeline is compiled into Go. Do not add a heterogeneous Step registry, runtime DAG, `map[string]any`, remote worker, or plugin graph.
- Steps receive immutable `RunView`, typed values, and minimum metered ports. They never receive SQLite, a repository, raw Docker, unrestricted Blob writes, process locks, or mutable run state.
- Use an OS-backed per-run lock for ownership and expected-version SQLite CAS for state consistency. Do not add execution leases, lease heartbeats, fencing epochs, or recovery ownership.
- SQLite write transactions must not span network, Docker, hashing, fsync, link/rename, `OpenVerified`, watchdog IPC, or blocking waits.
- Retry is bounded inside the current domain stage. Unknown external boundaries reconcile the original identity or settle conservatively; they never become a normal retry under a new key.
- Keep `READY` unreachable until Slice 5 atomically binds the same-run verified package occurrence and final quality report.
- Keep the Slice 0 watchdog safety protocol. Startup reconciliation is Docker-specific and may only inspect/stop/kill/wait/remove exact persisted resources.
- GC is explicit maintenance under an exclusive global artifact lock; ordinary stateful commands hold the shared form of that lock.
- Use test-driven development: prove each focused test fails before implementation, make it pass, run the stated regressions, then commit.

---

### Task 1: Reconcile the architecture contract with the lightweight decision

**Files:**
- Create: `scripts/check-slice1-architecture.ps1`
- Create: `docs/adr/0006-lightweight-local-workflow.md`
- Modify: `docs/adr/0001-static-typed-workflow.md`
- Modify: `docs/adr/0002-run-state-review.md`
- Modify: `docs/adr/0004-docker-direct-execution.md`
- Modify: `docs/adr/0005-docker-execution-lifecycle.md`
- Modify: `ARCHITECTURE.md`
- Modify: `docs/design/workflow.md`
- Modify: `docs/design/storage.md`
- Modify: `docs/design/testing.md`
- Modify: `docs/design/cli.md`
- Modify: `docs/design/configuration.md`
- Modify: `docs/design/llm.md`
- Modify: `docs/design/package.md`
- Modify: `docs/design/sandbox.md`
- Modify: `docs/design/similarity.md`
- Modify: `docs/implementation-plan.md`
- Modify: `docs/README.md`
- Modify: `docs/superpowers/specs/2026-08-31-lightweight-local-workflow-design.md`
- Modify: `docs/superpowers/specs/2026-08-31-phase1-mvp-design.md`
- Modify: `docs/traceability.md`
- Modify: `README.md`
- Modify: `TODO.md`

**Interfaces:**
- Consumes: the approved lightweight workflow spec and completed Slice 0 evidence.
- Produces: one non-contradictory documentation contract used by every remaining Slice 1 task.

- [x] **Step 1: Write the documentation-consistency test script**

Create `scripts/check-slice1-architecture.ps1` with strict mode and four groups of assertions:

```powershell
Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$required = @(
  'one foreground executor per run',
  'per-run process lock',
  'fixed pipeline',
  'no workflow-hosting service'
)
$forbidden = @(
  'execution\s+lease',
  'lease[ _-]?epoch',
  'owner[ _-]?epoch',
  'fencing([ _-]?(token|epoch))?',
  '\bPROBING\b',
  '\bQUIESCING\b',
  'startup[ _-]?janitor',
  'recovery[ _-]?intent',
  'observation[ _-]?(ticket|floor)'
)
```

The script discovers and reads `ARCHITECTURE.md`, both README files, every current `docs/adr/*.md` and `docs/design/*.md`, `docs/implementation-plan.md`, both Phase 1 specs, traceability, and TODO instead of relying on a hand-maintained partial list. It requires the approved terms and `Status: Accepted` in the lightweight design and ADR-0006 plus the architecture/implementation/Phase 1 documents. Forbidden-regex scanning excludes those two decision records because they intentionally name rejected alternatives, and ignores only explicitly delimited `Superseded design` sections in older ADRs; it scans every other current normative file completely. It checks that README/TODO name Slice 1 “lightweight local workflow.” Every failure reports the file, line, and matched phrase.

- [x] **Step 2: Run the script and verify failure**

```powershell
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
```

Expected: FAIL because ADR-0006 is absent and current documents still require leases, probing tickets, recovery intents, and a generic janitor.

- [x] **Step 3: Record the superseding ADR**

ADR-0006 must state:

```text
Status: Accepted
Decision: one foreground Go CLI executor per run on one host; OS process lock;
fixed typed pipeline; SQLite projection plus CPGen domain ledgers; no hosted runtime.
Supersedes: ADR-0001 only where it implied a generic Step runtime; ADR-0002
execution modes/leases/probe/recovery machinery; the old Slice 1 durable-engine scope.
Retains: typed inputs/outputs, immutable RunView, restricted ports, ReviewDecision,
Docker watchdog/resource identities, budgets, CallTrace, Blob/occurrence, package gates.
```

Change the approved lightweight design status from “approved direction; awaiting written-spec review” to `Accepted` without altering its decision. Revise ADR-0001 into “Static Typed CPGen Pipeline and Activity Contracts.” Simplify ADR-0002 to the seven run states, fixed-stage retry/resume/review/cancel behavior, and the rule that CANCELLED waits for safe sandbox stop. Amend ADR-0004/0005 so Docker authorization and cleanup use persisted execution/resource identity plus the per-run process lock, with no owner/lease epoch, `PROBING`/`QUIESCING`, takeover, or generic startup janitor. Preserve a short explicitly delimited “Superseded design” note instead of leaving those requirements active.

- [x] **Step 4: Rewrite the architecture and detailed designs**

Apply the approved boundary consistently:

- `ARCHITECTURE.md`: one-host foreground CLI, per-run file lock, fixed stages, short transaction protocol, domain ledgers, narrow watchdog reconciler, no generic runtime.
- `workflow.md`: concrete typed pipeline, stage-local bounded retry, manual resume, current-stage restart, review and cancellation.
- `storage.md`: runs/stages/attempts/events/control/review plus domain ledgers; remove lease/fencing/probe-ticket/recovery-intent DDL.
- `testing.md`: replace distributed-runtime tests with process-lock, stage-boundary crash, idempotency, and narrow Docker recovery tests.
- `cli.md`: retain current commands and exit codes, remove lease/mode language, define process-lock conflicts and restart semantics.
- `configuration.md`: local runtime/storage/lock/accounting values only; no workflow-service endpoint.
- `llm.md`, `similarity.md`, and `package.md`: ordinary stage-local calls/retry/review behavior without probe mode, owner epoch, or lease-bound occurrences.
- `sandbox.md`: `RunID + AttemptID + SandboxExecutionID + logical/plan/scope digest` authorization identity, deterministic resource labels, process-lock ownership, and exact-resource reconciliation; retain the watchdog but remove takeover/epoch/janitor semantics.
- Phase 1 spec, implementation plan, traceability, both README files, and TODO: replace the old Slice 1 scope and acceptance criteria.

- [x] **Step 5: Run documentation consistency and patch checks**

```powershell
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
rg -n -i 'Temporal|LangGraph|AutoGen|CrewAI' ARCHITECTURE.md docs README.md TODO.md
git diff --check
```

Expected: architecture script passes. Framework names occur only in ADR-0006 or the approved design as rejected/currently-unneeded options. Patch check has no output.

- [x] **Step 6: Commit**

```powershell
git add -- scripts/check-slice1-architecture.ps1 docs ARCHITECTURE.md README.md TODO.md
git commit -m "docs(slice-1): adopt lightweight local workflow"
```

### Task 2: Define local lifecycle values and cross-platform process locks

**Files:**
- Create: `internal/domain/workflow.go`
- Create: `internal/domain/workflow_test.go`
- Create: `internal/runlock/lock.go`
- Create: `internal/runlock/lock_unix.go`
- Create: `internal/runlock/lock_windows.go`
- Create: `internal/runlock/lock_test.go`
- Create: `internal/runlock/subprocess_test.go`
- Modify: `internal/domain/id.go`
- Modify: `internal/domain/outcomes.go`
- Modify: `internal/domain/outcomes_test.go`
- Modify: `internal/domain/value_test.go`
- Modify: `internal/adapter/sandbox/docker/process_test.go`

**Interfaces:**
- Consumes: existing strict enum decoding, `RunID`, `AttemptID`, `Digest`, `ExecutionCause`, and `golang.org/x/sys`.
- Produces: closed run/stage/attempt/review values and `runlock.Manager.Acquire` guards used by all mutating services.

- [x] **Step 1: Write failing lifecycle tests**

Add table-driven tests for these exact closed sets:

```go
type RunState string // CREATED RUNNING BLOCKED NEEDS_REVIEW READY FAILED CANCELLED
type StageState string // PENDING RUNNING SUCCEEDED BLOCKED NEEDS_REVIEW FAILED CANCELLED
type StageAttemptState string // RUNNING SUCCEEDED BLOCKED NEEDS_REVIEW FAILED CANCELLED INTERRUPTED
type ReviewDecisionKind string // REVISE RETRY WAIVE REJECT
type ReviewDecisionState string // PENDING APPLIED REJECTED STALE
```

Test strict JSON rejection, `READY` stage-boundary rejection, state/field matrices, positive ordinals/versions, canonical UTC timestamps, immutable digest validation, and reuse of one closed `ExecutionCause` type. Remove the legacy `quiesce` and `lease_lost` values; update the existing Docker process cancellation table in the same task so the Slice 0 suite still compiles and covers the remaining `user_cancel`, `revision_invalidated`, `step_deadline`, and `run_budget_deadline` causes. Docker cleanup is represented by persisted sandbox lifecycle rather than a workflow execution mode.

- [x] **Step 2: Write failing process-lock tests**

Test these public contracts:

```go
type Mode uint8
const (
	Shared Mode = iota + 1
	Exclusive
)

type Options struct {
	PollInterval time.Duration
}

var ErrBusy = errors.New("process lock is busy")

type Scope uint8
const (
	ScopeRun Scope = iota + 1
	ScopeArtifacts
)

type Manager struct {
	root         *os.Root
	pollInterval time.Duration
	closed       atomic.Bool
}

type Guard struct {
	scope Scope
	runID domain.RunID
	file  *os.File
	held  atomic.Bool
}

func NewManager(root string, options Options) (*Manager, error)
func (m *Manager) AcquireRun(ctx context.Context, runID domain.RunID, mode Mode) (*Guard, error)
func (m *Manager) AcquireArtifacts(ctx context.Context, mode Mode) (*Guard, error)
func (m *Manager) TryAcquireRun(runID domain.RunID, mode Mode) (*Guard, error)
func (m *Manager) TryAcquireArtifacts(mode Mode) (*Guard, error)
func (m *Manager) Close() error
func (g *Guard) Scope() Scope
func (g *Guard) RunID() (domain.RunID, bool)
func (g *Guard) Held() bool
func (g *Guard) Close() error
```

Within one process, require exclusive/exclusive and shared/exclusive conflicts, shared/shared success, `TryAcquire*` returning `ErrBusy` without polling, blocking acquire context cancellation, configured poll interval use, manager/double-guard Close safety, zero-value guard rejection, traversal-proof RunID-derived names, regular single-link final lock files, and two different run IDs proceeding concurrently. Lock files are permanent private coordination files and are never deleted. A helper subprocess acquires exclusive, reports READY, and is force-killed; a second process must then acquire the same lock without a stale timeout.

- [x] **Step 3: Run tests and verify failure**

```powershell
go test ./internal/domain ./internal/runlock -run 'Test(Workflow|Lifecycle|Lock|Subprocess)' -count=1
```

Expected: FAIL because workflow values and run locks do not exist.

- [x] **Step 4: Implement strict lifecycle values**

Reuse the existing `AttemptID` as the stage-attempt identity; do not introduce a synonymous `StageAttemptID`. Add typed IDs for `ReviewDecisionID`, `ControlRequestID`, `CallRecordID`, `ArtifactDeclarationID`, `ArtifactOccurrenceID`, and `CacheReuseRecordID` using the existing lowercase-prefixed ID validation pattern. Add `StageName`, immutable external `RunRequest`, persistence-facing `CreateRunRequest`, `RunSnapshot`, `StageSnapshot`, `StageAttempt`, `BlockedCheckpoint`, and `ReviewDecision` value types with exact `Validate` methods matching the tests.

`RunRequest` contains the exact versioned generation fields already specified in `configuration.md`: schema/mode, brief and normalized tag lists, language/difficulty, required/forbidden features, time/memory/solution language, optional seed, verification profile, export targets, and an integer-only `BudgetLimits` value covering LLM/Similarity calls, tokens and micro-USD, sandbox creates, artifact/package bytes, mutations per stage, and active time. `CreateRunRequest` adds the validated RunID, canonical submitted-request JSON and digest, effective seed, canonical redacted-effective-config JSON and digest, workflow digest, the separately copied `BudgetLimits`, fixed compiled stage sequence, canonical creation time, and idempotency key. The application layer is the only constructor from `RunRequest`; validation requires each stored digest to match its canonical bytes and limits. Keep READY in the vocabulary but return `ErrStageBoundary` from Slice 1 transition validation.

- [x] **Step 5: Implement cross-platform OS locks**

`NewManager` validates a positive configured poll interval and opens an absolute private runtime root through `os.Root`. Lock file names are `<validated-run-id>.lock`; no caller path is accepted. Create each final component once, require it to remain a regular single-link file, and never unlink it. Unix uses `unix.Flock(LOCK_SH|LOCK_EX|LOCK_NB)` and Windows uses `windows.LockFileEx` with or without `LOCKFILE_EXCLUSIVE_LOCK`. `TryAcquire*` makes one nonblocking attempt and returns typed `ErrBusy`; `Acquire*` retries only `ErrBusy` at the configured interval until context cancellation. Keep the file handle open for the guard lifetime and atomically claim Close before unlock/close.

Use a fixed private `artifacts.lock` scope for `AcquireArtifacts`; it is not represented as a fake RunID and cannot be selected by a caller. Both run and artifact guards use the same platform locking implementation.

- [x] **Step 6: Run focused, race, and full tests**

```powershell
gofmt -w internal/domain internal/runlock internal/adapter/sandbox/docker/process_test.go
go test -race ./internal/domain ./internal/runlock ./internal/adapter/sandbox/docker -run 'Test(Workflow|Lifecycle|Lock|Subprocess|ExecutionCause)' -count=1
go test ./... -count=1
go vet ./...
```

Expected: PASS.

- [x] **Step 7: Commit**

```powershell
git add -- internal/domain internal/runlock internal/adapter/sandbox/docker/process_test.go
git commit -m "phase1(slice-1): add local lifecycle and run locks"
```

### Task 3: Persist runs, fixed stages, events, review, cancel, and active time

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Modify: `internal/domain/workflow.go`
- Modify: `internal/domain/workflow_test.go`
- Create: `internal/port/runtime.go`
- Create: `internal/adapter/storage/sqlite/open.go`
- Create: `internal/adapter/storage/sqlite/open_test.go`
- Create: `internal/adapter/storage/sqlite/immediate.go`
- Create: `internal/adapter/storage/sqlite/migrate.go`
- Create: `internal/adapter/storage/sqlite/migrate_test.go`
- Create: `internal/adapter/storage/sqlite/runtime.go`
- Create: `internal/adapter/storage/sqlite/runtime_test.go`
- Create: `internal/adapter/storage/sqlite/review.go`
- Create: `internal/adapter/storage/sqlite/review_test.go`
- Create: `internal/adapter/storage/sqlite/migrations/000001_workflow_core.sql`

**Interfaces:**
- Consumes: Task 2 lifecycle values and an injected `clock.Clock`.
- Produces: ordered migrations, short `BEGIN IMMEDIATE` transactions, immutable run/stage projections, cancel/review commands, and active-time accounting.

- [x] **Step 1: Write failing opener and migration tests**

Use real temporary files and force several simultaneous pooled connections. On every connection assert:

```text
PRAGMA foreign_keys = 1
PRAGMA journal_mode = wal
PRAGMA busy_timeout = 5000
```

Require migration version/name/SHA-256 storage, idempotent reopen, changed-hash rejection, missing-version rejection, `PRAGMA foreign_key_check` with no rows, and `PRAGMA integrity_check = ok`. Hold `BEGIN IMMEDIATE` on store A and require store B to return a typed `storage_busy` within the configured bound. Inject COMMIT/ROLLBACK uncertainty and require the driver connection to be discarded.

- [x] **Step 2: Write failing projection and state tests**

Cover atomic create of run + fixed stage rows + version-1 event; replay with the same idempotency key; different digest rejection; every legal/illegal run and stage transition; direct READY rejection; expected-version conflicts; interrupted RUNNING attempt on resume; one active CANCEL; CANCEL versus terminal commit in both SQLite commit orders; one PENDING review; REVISE/RETRY/WAIVE/REJECT validation and application; event ordering; and reopen persistence.

Active-time tests use a fake clock: heartbeat while RUNNING accumulates once, BLOCKED/NEEDS_REVIEW time is excluded, clean completion closes the interval, crash recovery charges at most `last_accounting_heartbeat_at + heartbeat_interval`, and accumulation caps exactly at the immutable active-time limit with an exhausted result.

- [x] **Step 3: Run tests and verify failure**

```powershell
go test ./internal/adapter/storage/sqlite ./internal/port -run 'Test(Open|Migration|Runtime|Transition|Cancel|Review|ActiveTime)' -count=1
```

Expected: FAIL because SQLite storage and runtime ports are absent.

- [x] **Step 4: Pin compatible dependencies and implement the opener**

First implement `open.go`, `immediate.go`, and `migrate.go` with the `database/sql` and `modernc.org/sqlite` imports so module tidying cannot discard an unused pin. Then run:

```powershell
$env:GOTOOLCHAIN='go1.24.13'
go get modernc.org/sqlite@v1.45.0
go mod tidy
go mod edit -go=1.24.0
Remove-Item Env:GOTOOLCHAIN
```

Implement:

```go
type Config struct {
	Path        string
	BusyTimeout time.Duration
	MaxReaders  int
}

func Open(ctx context.Context, cfg Config) (*Store, error)
func (s *Store) Close() error
func (s *Store) immediate(ctx context.Context, fn func(*immediateTx) error) error
```

Apply connection-level pragmas whenever a physical connection is acquired. `immediate` executes exactly one `BEGIN IMMEDIATE` and one COMMIT or ROLLBACK; busy retry is bounded by context and configured timeout and never falls back to read-then-write.

- [x] **Step 5: Create the minimal workflow migration**

Migration 1 creates:

```text
schema_migrations
runs
stage_records
stage_attempts
run_events
control_requests
review_decisions
```

Use strict CHECK constraints for every enum and state/nullable-field matrix. `runs` stores immutable canonical submitted-request JSON plus digest, effective seed, canonical redacted-effective-config JSON plus digest, workflow digest, every integer budget-limit column, current stage/state/version, active elapsed/start/heartbeat fields, cancel summary, and a nullable `final_package_occurrence_id`. `active_elapsed_ns` is constrained to `0..max_active_time_ns` and every Task 3 update caps at that immutable limit. In Slice 1 the package column is CHECK-constrained to NULL and has no foreign key to a future table; Slice 5 rebuilds the table in a new migration when the parent occurrence exists. `stage_records` is one row per compiled stage name. `stage_attempts` is append-only and uses the existing `AttemptID`. Partial unique indexes allow one active cancel and one PENDING review. READY is rejected until a later migration replaces the guard in Slice 5.

- [x] **Step 6: Implement the runtime port and named commands**

Add and strictly validate the port command/value types in `domain/workflow.go`: `RunFilter`, `RunSummary`, `RunEvent`, `BeginStageCommand`, `FinishStageCommand`, `InterruptStageCommand`, `CancelRequest`, `ControlRequest`, `ActiveTimeCommand`, `ActiveTimeResult`, `CreateReviewRequest`, and `ApplyReviewCommand`. Each mutation command carries the stable identity/version/idempotency/time fields described below; the domain package contains no store or SQL type.

```go
type RuntimeStore interface {
	CreateRun(context.Context, domain.CreateRunRequest) (domain.RunSnapshot, error)
	GetRun(context.Context, domain.RunID) (domain.RunSnapshot, error)
	ListRuns(context.Context, domain.RunFilter) ([]domain.RunSummary, error)
	Events(context.Context, domain.RunID, int64) ([]domain.RunEvent, error)
	BeginStage(context.Context, domain.BeginStageCommand) (domain.StageAttempt, error)
	FinishStage(context.Context, domain.FinishStageCommand) (domain.RunSnapshot, error)
	InterruptStage(context.Context, domain.InterruptStageCommand) (domain.RunSnapshot, error)
	RequestCancel(context.Context, domain.CancelRequest) (domain.ControlRequest, error)
	PendingCancel(context.Context, domain.RunID) (*domain.ControlRequest, error)
	AccountActiveTime(context.Context, domain.ActiveTimeCommand) (domain.ActiveTimeResult, error)
}

type ReviewStore interface {
	CreateReview(context.Context, domain.CreateReviewRequest) (domain.ReviewDecision, error)
	PendingReview(context.Context, domain.RunID) (*domain.ReviewDecision, error)
	ApplyReview(context.Context, domain.ApplyReviewCommand) (domain.RunSnapshot, error)
}
```

Every command carries RunID, expected run version, stable idempotency key, and canonical UTC time. One helper updates the projection and inserts exactly one matching event in the same transaction. In Task 3, `AccountActiveTime` validates and advances only the run interval/projection; Task 4 extends that same named transaction to debit the authoritative ACTIVE_TIME budget account and return a deadline/exhaustion result. No other command may write the active-time fields. Do not expose `SetState`, `SetStage`, or an arbitrary SQL callback through the port.

- [x] **Step 7: Implement review commands**

```go
func (s *Store) CreateReview(ctx context.Context, req domain.CreateReviewRequest) (domain.ReviewDecision, error)
func (s *Store) PendingReview(ctx context.Context, runID domain.RunID) (*domain.ReviewDecision, error)
func (s *Store) ApplyReview(ctx context.Context, command domain.ApplyReviewCommand) (domain.RunSnapshot, error)
```

Review creation requires NEEDS_REVIEW, no active CANCEL, exact snapshot/policy/evidence binding, reviewer, and reason. Apply rechecks binding and expected version. REVISE records exact new input/config digest and invalidated stages; RETRY requires positive budget change or new external-condition digest; WAIVE requires a policy-declared waivable gate; REJECT finishes FAILED. Identical replay is idempotent and changed content is a consistency error.

- [x] **Step 8: Run focused, race, Go 1.24, and full tests**

```powershell
$env:GOTOOLCHAIN='go1.24.13'
gofmt -w internal/domain internal/port internal/adapter/storage/sqlite
go test -race ./internal/domain ./internal/adapter/storage/sqlite ./internal/port -count=1
go test ./... -count=1
go vet ./...
Remove-Item Env:GOTOOLCHAIN
```

Expected: PASS.

- [x] **Step 9: Commit**

```powershell
git add -- go.mod go.sum internal/domain internal/port internal/adapter/storage/sqlite
git commit -m "phase1(slice-1): persist lightweight workflow state"
go test ./internal/domain ./internal/port ./internal/adapter/storage/sqlite -run 'Test(Open|Migration|Runtime|Transition|Cancel|Review|ActiveTime)' -count=1
```

### Task 4: Add CPGen call and budget ledgers without a workflow runtime

**Files:**
- Modify: `internal/domain/call.go`
- Modify: `internal/domain/call_test.go`
- Create: `internal/domain/budget.go`
- Create: `internal/domain/budget_test.go`
- Create: `internal/port/metering.go`
- Create: `internal/adapter/storage/sqlite/migrations/000002_call_budget.sql`
- Create: `internal/adapter/storage/sqlite/metering.go`
- Create: `internal/adapter/storage/sqlite/metering_test.go`
- Create: `internal/application/call_coordinator.go`
- Create: `internal/application/call_coordinator_test.go`
- Modify: `internal/adapter/storage/sqlite/runtime.go`
- Modify: `internal/adapter/storage/sqlite/runtime_test.go`

**Interfaces:**
- Consumes: current RUNNING stage attempt, expected run version, existing `CallTrace`/`MeteredOutcome`, and the run process lock held by the application coordinator.
- Produces: logical call records, physical attempt records, atomic reservations, conservative settlement, and terminal CallTrace projection.

- [x] **Step 1: Write failing schema and budget tests**

Test all Phase 1 dimensions: LLM calls/input tokens/output tokens/cost, Similarity calls/cost, Docker container creates, artifact physical-new bytes, and active time. Direct SQL must reject cross-run/stage/attempt references, negative/overflow values, duplicate physical ordinals, wrong reservation dimensions, terminal-field matrix violations, and result calls outside their logical record. Update `CallTrace` tests so CACHE_HIT has zero physical calls and carries a source `CallRecordID` plus the current cache-hit `CallRecordID`; it must not contain or manufacture cache-source/cache-pin `AttemptCallID` values.

Race two transactions for the final unit of each dimension and require one success. A budget/policy pre-rejection must persist a terminal logical record with `NO_DISPATCH` and zero physical attempts. Replaying one logical or physical idempotency key returns the same record; changing any digest or bound fails.

- [x] **Step 2: Write failing dispatch-boundary tests**

Cover:

```text
PREPARED -> DISPATCHING -> SENT -> COMPLETED
PREPARED -> ABORTED_NO_DISPATCH
DISPATCHING/SENT -> UNKNOWN
```

Only a confirmed no-send or completed retryable response may allocate the next physical ordinal. UNKNOWN must reconcile the original provider/Docker identity or settle its upper bound; it cannot use a new key. Success and every typed `PortFailure` must produce a terminal `MeteredOutcome` whose returned `CallTrace` exactly equals the database projection.

- [x] **Step 3: Run tests and verify failure**

```powershell
go test ./internal/domain ./internal/adapter/storage/sqlite ./internal/application -run 'Test(Budget|CallRecord|PhysicalCall|Dispatch|Settlement|MeteredOutcome)' -count=1
```

Expected: FAIL because the call/budget ledger is absent.

- [x] **Step 4: Create the call/budget migration**

Migration 2 creates:

```text
budget_accounts
call_records
physical_calls
budget_reservations
```

`call_records` is the logical Generate/Search/Compile/Run/Probe/cache request and owns terminal DISPATCHED/CACHE_HIT/NO_DISPATCH projection fields. `physical_calls` is one provider request, Docker ping/create, or local artifact session with a stable physical ordinal/idempotency key and state. Composite foreign keys bind record, run, existing `AttemptID`, result call, reservation, dimension, and subkey. Replace `CallTrace.CacheSourceAttemptCallID/CachePinCallID` with `CacheSourceCallRecordID/CacheHitCallRecordID`; DISPATCHED continues to use physical `AttemptCallID`s, while CACHE_HIT requires those two logical IDs and no physical IDs. The migration creates every budget account for existing runs directly from Migration 1's immutable integer limit columns, then checks those accounts against the canonical request snapshot digest; it never reparses mutable configuration or invents a default. For an upgraded v1 database, initialize ACTIVE_TIME consumed from the already capped `runs.active_elapsed_ns` and remaining as the checked subtraction `limit - consumed`; migration tests create real pre-v2 elapsed data and require account/projection equality after upgrade.

- [x] **Step 5: Implement ledger commands**

```go
type CallLedger interface {
	OpenCall(context.Context, domain.OpenCallRequest) (domain.CallRecord, error)
	PrepareCalls(context.Context, domain.PrepareCallsRequest) (domain.PreparedCalls, error)
	BeginDispatch(context.Context, domain.BeginDispatchRequest) (domain.DispatchGrant, error)
	MarkSent(context.Context, domain.DispatchGrant, time.Time) error
	CompletePhysical(context.Context, domain.CompletePhysicalRequest) error
	FinishCall(context.Context, domain.FinishCallRequest) (domain.CallTrace, error)
}
```

Each mutation verifies expected run version/current stage attempt/no active CANCEL in a short transaction. `PrepareCalls` reserves the complete fixed bundle atomically. `BeginDispatch` is a narrow call-ledger CAS, not a workflow-worker claim; it exists only to record the external send boundary. Settlement uses verified integer usage, or the reservation upper bound for missing/contradictory/UNKNOWN usage.

ACTIVE_TIME has one authority after this migration: its `budget_accounts` row. Extend `RuntimeStore.AccountActiveTime` so one `BEGIN IMMEDIATE` transaction conditionally debits that account, copies consumed time into `runs.active_elapsed_ns`, advances start/heartbeat fields, and returns `ActiveTimeResult{Remaining, Deadline, Exhausted}`. Tests require the projection to equal the authoritative account after every success/replay/crash recovery; no second write path to `active_elapsed_ns` remains.

- [x] **Step 6: Implement bounded call coordination**

`application.CallCoordinator` opens one logical call, asks the adapter for a deterministic fixed attempt plan, prepares that whole plan, executes attempts outside SQLite transactions, and finishes the terminal projection. Its retry loop takes a persisted `RetryPolicy{MaxAttempts, InitialBackoff, MaxBackoff, JitterSeedDigest}` and an injected clock; it does not create timer rows or run after the foreground command exits.

- [x] **Step 7: Run focused, race, and full tests**

```powershell
gofmt -w internal/domain internal/port internal/adapter/storage/sqlite internal/application
go test -race ./internal/domain ./internal/adapter/storage/sqlite ./internal/application -run 'Test(Budget|CallRecord|PhysicalCall|Dispatch|Settlement|MeteredOutcome)' -count=1
go test ./... -count=1
go vet ./...
```

Expected: PASS.

- [x] **Step 8: Commit**

```powershell
git add -- internal/domain internal/port internal/adapter/storage/sqlite internal/application
git commit -m "phase1(slice-1): add call and budget ledgers"
```

### Task 5: Implement private Blob CAS and atomic artifact occurrences

**Files:**
- Create: `internal/adapter/storage/sqlite/migrations/000003_artifacts.sql`
- Create: `internal/adapter/storage/blob/store.go`
- Create: `internal/adapter/storage/blob/writer.go`
- Create: `internal/adapter/storage/blob/publish.go`
- Create: `internal/adapter/storage/blob/verify.go`
- Create: `internal/adapter/storage/blob/quarantine.go`
- Create: `internal/adapter/storage/blob/store_test.go`
- Create: `internal/adapter/storage/sqlite/artifact.go`
- Create: `internal/adapter/storage/sqlite/artifact_test.go`
- Create: `internal/application/artifact_session.go`
- Create: `internal/application/artifact_session_test.go`
- Modify: `internal/domain/artifact.go`
- Modify: `internal/domain/value_test.go`
- Modify: `internal/adapter/storage/sqlite/runtime.go`
- Modify: `internal/adapter/storage/sqlite/runtime_test.go`

**Interfaces:**
- Consumes: prepared LOCAL_ARTIFACT_WRITE physical calls and ARTIFACT_BYTES reservations from Task 4.
- Produces: private content-addressed publication, same-handle `OpenVerified`, writer-token settlement, pins, and stage-atomic artifact occurrences.

- [x] **Step 1: Write failing filesystem and relational attack tests**

Cover traversal, absolute/platform paths, symlink/hardlink/FIFO/socket/device input, short write, declared limit+1, same-size corruption, target collision, two publishers of one digest, token double-open, different second finalize, wrong run/stage/call/reservation, role/path/media relabel, non-finalized occurrence, and crashes after OPEN, SEALED, publish, READY, FINALIZED, and before occurrence. Add tagged-union tests that reject an empty occurrence, both variants at once, a NEW_WRITE without the existing writer/reservation/call/pin fields, and a CACHE_REUSE carrying any writer token, reservation, physical call, or pin.

Add a source scan that fails if a low-level Blob put symbol is exported or if a workflow/Step package imports `internal/adapter/storage/blob` directly.

- [x] **Step 2: Run tests and verify failure**

```powershell
go test ./internal/adapter/storage/blob ./internal/adapter/storage/sqlite ./internal/application -run 'Test(Blob|Writer|Artifact|Occurrence|Quarantine|SourceBoundary)' -count=1
```

Expected: FAIL because the formal CAS and artifact tables are absent.

- [x] **Step 3: Create artifact tables**

Migration 3 creates:

```text
artifact_declarations
artifact_writer_tokens
blobs
blob_pins
blob_pin_history
cache_reuse_records
artifact_occurrences
```

Writer tokens use PREPARED/OPEN/SEALED/FINALIZED/RELEASED. A declaration fixes role, logical path, media type, limit, and ARTIFACT_BYTES reservation subkey. Composite foreign keys bind final digest/size, physical call, logical call, run, stage attempt, and reservation.

Create the final cache-ready occurrence shape before migration 3 is ever applied:

```text
NEW_WRITE: writer_token_id non-null, source_occurrence_id null
CACHE_REUSE: writer_token_id null, source_occurrence_id and cache_reuse_record_id non-null
```

`cache_reuse_records` binds current logical call/run/stage, source occurrence/call, digest/size, and a cache-key snapshot. Migration 4 later creates the cache parent tables and installs triggers that validate the cache-key/source snapshot; it never edits migration 3. Both occurrence variants bind digest/size/role/path/media/run/stage/logical-call shadow columns and require a READY Blob.

- [x] **Step 4: Implement the bounded prepared writer**

The exact flow is:

```text
PREPARED --CAS--> OPEN
bounded temp write + SHA-256
fsync temp
short tx: OPEN --CAS--> SEALED + STAGING Blob + ACTIVE pin
same-filesystem no-overwrite os.Link(temp, canonical)
remove temp + fsync parent
open canonical without following links; verify regular, single-link, size, SHA-256
short tx: Blob READY + token FINALIZED
return PendingArtifact
```

If canonical already exists, verify its full bytes using the exact opened handle before deduplication. Never overwrite corrupt bytes. Corruption transitions Blob through QUARANTINING to CORRUPT and moves safe bytes to a fixed private quarantine name.

- [x] **Step 5: Implement prepared artifact sessions**

```go
type PreparedArtifactSession interface {
	Prepare(context.Context, domain.ArtifactDeclarationID) (port.ArtifactWriter, error)
	ReleaseUnused(context.Context) error
}

func NewPreparedArtifactSession(
	ledger port.ArtifactLedger,
	store *blob.Store,
	prepared domain.PreparedCalls,
) (PreparedArtifactSession, error)
```

The constructor clones a fixed declaration/token bundle. `Prepare` consumes each declaration once and cannot allocate calls or budget. `OpenVerified` returns the exact verified handle rewound to byte zero.

Retain the existing `domain.PendingArtifact` as the validated NEW_WRITE payload used by Slice 0. Add an explicit occurrence union:

```go
type PendingOccurrenceKind string // NEW_WRITE CACHE_REUSE

type PendingCacheReuse struct {
	CacheReuseRecordID CacheReuseRecordID
	SourceOccurrenceID ArtifactOccurrenceID
	SourceCallRecordID CallRecordID
	CurrentCallRecordID CallRecordID
	Blob BlobRef
	MediaType string
	Role ArtifactRole
	LogicalPath SafeRelPath
	Provenance ProvenanceCandidate
}

type PendingOccurrence struct {
	Kind PendingOccurrenceKind
	NewWrite *PendingArtifact
	CacheReuse *PendingCacheReuse
}
```

`Validate` enforces exactly one matching branch. CACHE_REUSE has no writer token, reservation, physical call, physical-new bytes, or pin; it proves the source and current logical call IDs plus source occurrence/cache-reuse records instead. Its returned `CallTrace` uses the same source/current `CallRecordID` pair.

- [x] **Step 6: Attach occurrences in the stage-finish transaction**

Extend `FinishStageCommand` with immutable `[]PendingOccurrence`. In the existing short runtime transaction, branch on the validated tag: NEW_WRITE validates a FINALIZED token and ACTIVE pin, inserts the occurrence, settles physical-new artifact bytes, and moves the pin to RELEASABLE; CACHE_REUSE validates the current logical cache-hit call plus source occurrence/reuse record and inserts no writer/pin/reservation fields. Then finish the attempt, update projection, and append the event. Success attaches occurrences; failure, stale input, CANCEL, and interrupted restart settle/release NEW_WRITE state exactly once without attaching current output.

- [x] **Step 7: Run focused, race, cross-platform, and full tests**

```powershell
gofmt -w internal/adapter/storage/blob internal/adapter/storage/sqlite internal/application
go test -race ./internal/adapter/storage/blob ./internal/adapter/storage/sqlite ./internal/application -run 'Test(Blob|Writer|Artifact|Occurrence|Quarantine|SourceBoundary)' -count=1
go test ./... -count=1
go vet ./...
$env:GOOS='linux'
$env:GOARCH='amd64'
$env:CGO_ENABLED='0'
go test -exec 'cmd.exe /c exit 0' ./internal/adapter/storage/blob
Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED
```

Expected: PASS.

- [x] **Step 8: Commit**

```powershell
git add -- internal/domain internal/adapter/storage/blob internal/adapter/storage/sqlite internal/application
git commit -m "phase1(slice-1): add verified artifact ledger"
go test ./internal/domain ./internal/adapter/storage/blob ./internal/adapter/storage/sqlite ./internal/application -run 'Test(Blob|Writer|Artifact|Occurrence|Quarantine|SourceBoundary)' -count=1
```

### Task 6: Add cache provenance, mutation accounting, and explicit Blob maintenance

**Files:**
- Create: `internal/domain/cache.go`
- Create: `internal/domain/mutation.go`
- Create: `internal/port/cache.go`
- Create: `internal/port/mutation.go`
- Create: `internal/adapter/storage/sqlite/migrations/000004_cache_mutation.sql`
- Create: `internal/adapter/storage/sqlite/cache.go`
- Create: `internal/adapter/storage/sqlite/cache_test.go`
- Create: `internal/adapter/storage/sqlite/mutation.go`
- Create: `internal/adapter/storage/sqlite/mutation_test.go`
- Create: `internal/adapter/storage/blob/gc.go`
- Create: `internal/adapter/storage/blob/gc_test.go`
- Create: `internal/application/cache.go`
- Create: `internal/application/cache_test.go`
- Create: `internal/application/artifact_maintenance.go`
- Create: `internal/application/artifact_maintenance_test.go`

**Interfaces:**
- Consumes: READY verified Blobs, source artifact occurrences/call records, the shared/exclusive global artifact lock, and current stage budget limits.
- Produces: verified cache reuse with full provenance, non-oversold mutation claims, and explicit crash-safe Blob maintenance.

- [x] **Step 1: Write failing cache, mutation, and GC tests**

Cover cache key/source digest mismatch, cross-run source forgery, invalidated/expired entry, corrupt/missing Blob, verified reuse occurrence, cache hit with zero physical call, mutation limit exhaustion, concurrent final mutation claim, duplicate intent, record with unrelated logical call/reservation/output occurrence, GC versus a shared artifact lock, unreferenced Blob deletion, referenced Blob preservation, and crashes before/after trash rename and metadata commit.

- [x] **Step 2: Run tests and verify failure**

```powershell
go test ./internal/adapter/storage/sqlite ./internal/adapter/storage/blob ./internal/application -run 'Test(Cache|CacheReuse|Mutation|GC|ArtifactMaintenanceLock)' -count=1
```

Expected: FAIL because migration 4 and maintenance operations are absent.

- [x] **Step 3: Create cache and mutation tables**

Migration 4 creates:

```text
cache_entries
cache_entry_sources
cache_blob_refs
mutation_accounts
mutation_claims
mutation_intents
mutation_records
mutation_record_operations
mutation_record_reservations
```

A cache entry binds its key/kind/policy/input/source call and complete Blob set. Add triggers that validate each migration-3 cache reuse record against the exact cache entry/source/Blob set. A mutation claim contains run, stable stage-scope digest, source-batch digest, ordinal, limit snapshot, kind, and intent digest; it contains no future call, reservation, writer token, or output occurrence. A successful record later lists the actual operations/reservations and output occurrence through composite foreign keys.

- [x] **Step 4: Implement verified cache reuse**

```go
type CacheStore interface {
	Lookup(context.Context, domain.CacheLookup) (domain.CacheCandidate, bool, error)
	CommitReuse(context.Context, domain.CommitCacheReuse) (domain.PendingCacheReuse, error)
	Invalidate(context.Context, domain.CacheKey, domain.InvalidationCause) error
}
```

The application holds the shared artifact lock across lookup, `OpenVerified`, and reuse preparation. It opens one logical call, records `CACHE_HIT` with no physical calls/reservations for external-call counts, verifies every Blob, then returns a CACHE_REUSE payload containing the source occurrence/reuse/current logical-call identities and no NEW_WRITE fields. `FinishStage` wraps it in `PendingOccurrence{Kind: CACHE_REUSE}` and atomically creates the current-run occurrence while finishing the stage. Verification failure invalidates the entry and returns a typed failure without attaching output.

- [x] **Step 5: Implement mutation authorization**

```go
type MutationAuthorizer interface {
	ClaimMutation(context.Context, domain.MutationClaimRequest) (domain.MutationGrant, error)
	RecordMutation(context.Context, domain.MutationRecordRequest) error
}
```

Claim uses one overflow-safe conditional update and immutable intent insert. Record requires the same run/scope/source/ordinal/kind and proves every actual logical operation, reservation, and output occurrence. Identical replay is idempotent; any drift is a consistency error. Failed attempts retain the claim/intent and call evidence but create no mutation record.

- [x] **Step 6: Implement explicit Blob maintenance**

```go
type ArtifactMaintenance struct {
	locks    *runlock.Manager
	metadata port.GCMetadataStore
	blobs    *blob.Store
}

func NewArtifactMaintenance(*runlock.Manager, port.GCMetadataStore, *blob.Store) (*ArtifactMaintenance, error)
func (m *ArtifactMaintenance) CollectGarbage(context.Context) (domain.GCReport, error)
func (m *ArtifactMaintenance) ReconcileTrash(context.Context) (domain.GCReport, error)
```

The application service acquires the exclusive global artifact guard internally, keeps the unexported local guard alive for the complete operation, and releases it only after all metadata/filesystem phases return; callers cannot close it between a held check and use. A narrow `GCMetadataStore` plans and commits metadata but never manipulates paths. In a short transaction, mark only READY, unreferenced, unpinned Blobs DELETING. Outside the transaction, rename canonical bytes to a deterministic private trash name and fsync both parents. A final transaction removes metadata or records repair state. `ReconcileTrash` handles every database/file ordering idempotently. It never scans or removes a path outside the private artifact root.

- [x] **Step 7: Run focused, race, and full tests**

```powershell
gofmt -w internal/domain internal/port internal/adapter/storage/sqlite internal/adapter/storage/blob internal/application
go test -race ./internal/adapter/storage/sqlite ./internal/adapter/storage/blob ./internal/application -run 'Test(Cache|CacheReuse|Mutation|GC|ArtifactMaintenanceLock)' -count=1
go test ./... -count=1
go vet ./...
```

Expected: PASS.

- [x] **Step 8: Commit**

```powershell
git add -- internal/domain internal/port internal/adapter/storage/sqlite internal/adapter/storage/blob internal/application
git commit -m "phase1(slice-1): add cache and maintenance ledgers"
```

### Task 7: Persist Docker resource identity and add narrow startup reconciliation

**Files:**
- Create: `internal/domain/sandbox_execution.go`
- Create: `internal/port/sandbox_lifecycle.go`
- Create: `internal/port/sandbox_lifecycle_test.go`
- Create: `internal/adapter/storage/sqlite/migrations/000005_sandbox_execution.sql`
- Create: `internal/adapter/storage/sqlite/sandbox_execution.go`
- Create: `internal/adapter/storage/sqlite/sandbox_execution_test.go`
- Create: `internal/adapter/sandbox/docker/reconciler.go`
- Create: `internal/adapter/sandbox/docker/reconciler_test.go`
- Modify: `internal/domain/id.go`
- Modify: `internal/port/sandbox.go`
- Modify: `internal/port/sandbox_test.go`
- Modify: `internal/port/sandbox_plan.go`
- Modify: `internal/port/sandbox_plan_test.go`
- Modify: `internal/port/sandbox_probe_factory.go`
- Modify: `internal/adapter/sandbox/docker/plan.go`
- Modify: `internal/adapter/sandbox/docker/plan_test.go`
- Modify: `internal/adapter/sandbox/docker/runner.go`
- Modify: `internal/adapter/sandbox/docker/runner_test.go`
- Modify: `internal/adapter/sandbox/docker/process_runner_test.go`
- Modify: `internal/adapter/sandbox/docker/spec.go`
- Modify: `internal/adapter/sandbox/docker/transfer.go`
- Modify: `internal/adapter/sandbox/docker/transfer_test.go`
- Modify: `internal/adapter/sandbox/docker/watchdog_test.go`
- Modify: `internal/probe/harness.go`
- Modify: `internal/probe/harness_test.go`
- Modify: `internal/probe/ledger.go`
- Modify: `internal/probe/ledger_test.go`
- Modify: `internal/watchdog/control.go`
- Modify: `internal/watchdog/service.go`
- Modify: `internal/watchdog/service_test.go`

**Interfaces:**
- Consumes: Task 4 prepared Docker physical calls, existing immutable `ContainerPlan`, engine identity, watchdog protocol, and current run/stage identity.
- Produces: durable SandboxExecution/resource/control evidence before Create and a cleanup-only exact-identity reconciler.

- [x] **Step 1: Write failing persistence-order and crash tests**

Instrument a fake Engine, call ledger, and watchdog channel. Require the execution, complete planned resource set, engine identity digest, deterministic names/labels/call roles, watchdog control reference/token digest, and deadlines to commit before the first Create. The sealed authorization identity is exactly `RunID + AttemptID + SandboxExecutionID + LogicalOperationID + ScopeDigest + PlanDigest + EngineIdentityDigest`; physical grants add only call ID/resource ordinal/role. Tests reject any `OwnerID`, `LeaseEpoch`, takeover token, or owner-derived label. For each resource assert:

```text
PLANNED
-> CREATING (database CAS)
-> watchdog pre-create ACK
-> BeginDispatch (call-ledger CAS)
-> Engine Create/mkdir
-> MarkSent/CompletePhysical
-> persisted engine ID/identity evidence
-> resource ACK
-> STARTED or cleanup phase
```

Inject failure before/after every boundary. Cover late Create, duplicate ID, wrong labels, engine mismatch, watchdog EOF/death, Create/Start/export uncertainty, target not proven stopped, cleanup retry, and two goroutines attempting one resource transition.

- [x] **Step 2: Write failing reconciler capability tests**

The public reconciler interface must expose only:

```go
type SandboxReconciler interface {
	ReconcileRun(context.Context, domain.RunID) (domain.SandboxReconcileReport, error)
}
```

Source and behavior tests reject Create, Start, Exec, Copy/export, artifact publication, broad label listing, or unrelated-run mutation. Exact persisted ID/name/labels/engine identity may be inspected/stopped/killed/waited/removed. Wrong or missing identity yields an auditable manual-cleanup blocker rather than a broad sweep.

- [x] **Step 3: Run tests and verify failure**

```powershell
go test ./internal/adapter/storage/sqlite ./internal/adapter/sandbox/docker ./internal/watchdog ./internal/port -run 'Test(SandboxExecution|ResourceLifecycle|WatchdogPersistence|Reconciler|CleanupOnly)' -count=1
```

Expected: FAIL because persistent sandbox lifecycle and the narrow reconciler are absent.

- [x] **Step 4: Create sandbox lifecycle tables**

Migration 5 creates:

```text
sandbox_executions
sandbox_resources
sandbox_watchdog_controls
sandbox_precreate_acks
```

Add `SandboxExecutionID` to the existing ID vocabulary and remove `OwnerID` after every Slice 0 sandbox/probe caller has migrated. Execution rows bind logical call, run, existing `AttemptID`, sandbox execution ID, scope/plan/engine-identity digests, lifecycle/cleanup version, state, and deadlines. Resource rows bind plan index, kind, role, physical call where applicable, deterministic name, expected labels digest, engine/cgroup identity evidence, creation nonce, version, and monotone phase. Deterministic labels contain run/attempt/sandbox-execution/logical-operation/call/resource-role identities and digests, never an owner or epoch. Control rows store only token digest and an internal process-owned record reference, never the raw token.

- [x] **Step 5: Implement the lifecycle recorder**

```go
type SandboxLifecycleRecorder interface {
	PrepareExecution(context.Context, domain.PrepareExecutionRequest) (domain.SandboxExecution, error)
	RecordWatchdogArmed(context.Context, domain.WatchdogArmed) error
	BeginResourceCreate(context.Context, domain.BeginResourceCreate) (domain.PreCreateRequest, error)
	RecordPreCreateACK(context.Context, domain.PreCreateACK) error
	AdvanceResource(context.Context, domain.AdvanceResourceRequest) (domain.SandboxResource, error)
	MarkCleanupPending(context.Context, domain.MarkCleanupPendingCommand) (domain.SandboxExecution, error)
	FinishCleanup(context.Context, domain.FinishCleanupCommand) (domain.SandboxExecution, error)
}
```

Every request/command carries the exact sandbox execution/resource ID, expected lifecycle version, stable idempotency key, and canonical time. Every method is a named short transaction with current stage/CANCEL guards where new work is involved. Cleanup settlement remains permitted after cancellation. There is no owner epoch, takeover lease, generic intent, or arbitrary state setter.

- [x] **Step 6: Wire Runner and watchdog around persisted phases**

Materialize and fsync the process-owned control file outside SQLite, commit its reference/digest, start the detached watchdog, and persist initial full-plan ACK. Before each Create, persist CREATING, obtain/validate pre-create ACK outside SQLite, persist the ACK, and call Task 4 `BeginDispatch`. Invoke Engine Create outside all SQLite transactions; on a confirmed return use the same grant for `MarkSent`/`CompletePhysical`, inspect exact identity, and persist it with a resource-version CAS before Start. A crash after send but before settlement leaves DISPATCHING/UNKNOWN for exact-identity reconciliation, never a fresh call key. Any uncertain boundary or watchdog failure forbids new Create/Start/export and marks cleanup pending.

- [x] **Step 7: Implement the narrow reconciler**

Under the per-run execution lock, load unfinished executions and reconcile each exact resource outside a write transaction. Persist each observation/cleanup result in a separate short transaction. Incomplete old operations are stopped, marked INTERRUPTED, and rerun by the application under a new `AttemptID` and logical call. A fully committed result may be replayed without another Docker action. The detached watchdog owns only its sealed resource plan and stop/kill cleanup after process loss; it never writes run/stage state, and resume performs no ownership takeover.

- [x] **Step 8: Run focused, tagged, race, and full tests**

```powershell
gofmt -w internal/domain internal/port internal/adapter/storage/sqlite internal/adapter/sandbox/docker internal/watchdog
go test -race ./internal/adapter/storage/sqlite ./internal/adapter/sandbox/docker ./internal/watchdog ./internal/port ./internal/probe -run 'Test(SandboxExecution|ResourceLifecycle|WatchdogPersistence|Reconciler|CleanupOnly|AuthorizationIdentity)' -count=1
go test -tags=cpgen_slice0_probe ./internal/adapter/sandbox/docker ./internal/probe -count=1
go test ./... -count=1
go vet ./...
```

Expected: PASS.

- [x] **Step 9: Commit**

```powershell
git add -- internal/domain internal/port internal/adapter/storage/sqlite internal/adapter/sandbox/docker internal/probe internal/watchdog
git commit -m "phase1(slice-1): persist sandbox cleanup evidence"
go test ./internal/domain ./internal/port ./internal/adapter/storage/sqlite ./internal/adapter/sandbox/docker ./internal/probe ./internal/watchdog -run 'Test(SandboxExecution|ResourceLifecycle|WatchdogPersistence|Reconciler|CleanupOnly|AuthorizationIdentity)' -count=1
```

### Task 8: Assemble the fixed typed Fake pipeline and local coordinator

**Files:**
- Create: `internal/domain/run_view.go`
- Create: `internal/domain/run_view_test.go`
- Create: `internal/domain/agent_result.go`
- Create: `internal/domain/agent_result_test.go`
- Create: `internal/workflow/step.go`
- Create: `internal/workflow/capabilities.go`
- Create: `internal/workflow/slice1.go`
- Create: `internal/workflow/slice1_test.go`
- Create: `internal/workflow/source_boundary_test.go`
- Create: `internal/adapter/fake/workflow.go`
- Create: `internal/adapter/fake/workflow_test.go`
- Create: `internal/application/run_service.go`
- Create: `internal/application/run_service_test.go`
- Create: `internal/application/active_time.go`
- Create: `internal/application/active_time_test.go`

**Interfaces:**
- Consumes: runtime/call/artifact/cache/mutation/sandbox ports, per-run and shared artifact guards, injected clock, and Docker-specific reconciler.
- Produces: a compile-time-connected Fake pipeline and `RunService.Generate/Resume/Cancel` behavior that survives restart.

- [x] **Step 1: Write failing type and source-boundary tests**

Define the exact interface:

```go
type Step[I any, O any] interface {
	Name() domain.StageName
	Run(context.Context, domain.RunView, I) (domain.AgentResult[O], error)
}
```

Compile-time tests require the concrete constructor:

```go
func NewSlice1Pipeline(
	prepare Step[domain.Slice1Input, domain.Slice1Prepared],
	exercise Step[domain.Slice1Prepared, domain.Slice1Evidence],
	checkpoint Step[domain.Slice1Evidence, domain.Slice1Checkpoint],
) (Slice1Pipeline, error)
```

Define three non-interchangeable constructor inputs: `PrepareCapabilities{LLM port.MeteredLLM, Artifacts port.MeteredArtifactSink}`, `ExerciseCapabilities{Sandbox port.MeteredSandbox, Artifacts port.MeteredArtifactSink, Blobs port.VerifiedBlobReader}`, and `CheckpointCapabilities{Similarity port.MeteredSimilarity, Cache port.CacheStore, Mutations port.MutationAuthorizer}`. Concrete Fake step constructors accept only their own value and store no shared service superset. Source scans fail if `internal/workflow` or `internal/adapter/fake` imports SQLite, runlock, Docker adapter, Blob implementation, review store, process locks, repositories, dispatch constructors, or defines a generic `Services` type. Fail if any heterogeneous `[]Step`, runtime registry, or `map[string]any` pipeline appears.

- [x] **Step 2: Write failing coordinator lifecycle tests**

Using real file SQLite/Blob roots and Fake ports, drive:

```text
create -> RUNNING prepare -> exercise -> NEEDS_REVIEW(slice1_fixture_complete)
BLOCKED -> resume -> new same-stage retry attempt -> metered dependency revalidation -> continue
NEEDS_REVIEW + REVISE/RETRY/WAIVE/REJECT -> resume
RUNNING interrupted process -> reconcile -> INTERRUPTED old attempt -> rerun
active CANCEL -> cleanup -> CANCELLED
```

Assert one run executor lock, shared artifact lock, immutable RunView copies, exact stage input/output digests, deterministic event order, stable idempotency keys, no READY, and no external I/O during SQLite write transactions. A BLOCKED resume creates a fresh attempt whose first authorized operation revalidates the checkpoint's exact dependency through its ordinary metered port and current policy; a stale health/capability cache cannot unblock it, failure closes the new attempt back to BLOCKED, and success may continue the stage without introducing a PROBING mode. Start a foreground-only cancel poller, insert CANCEL from a second handle, require it to cancel the root execution context with `CauseUserCancel`, stop authorizing new calls, settle/clean exact in-flight sandbox work, and stop the poller before `RunService` returns. Test active-time exhaustion through the Task 4 authoritative account with the same cleanup-before-state-transition behavior.

- [x] **Step 3: Run tests and verify failure**

```powershell
go test ./internal/domain ./internal/workflow ./internal/adapter/fake ./internal/application -run 'Test(RunView|AgentResult|Slice1Pipeline|SourceBoundary|RunService|Resume|Cancel|ActiveTime)' -count=1
```

Expected: FAIL because the typed pipeline and coordinator are absent.

- [x] **Step 4: Implement immutable workflow values and per-step capabilities**

`RunView` stores IDs, workflow/config/request digests, state/current stage/version, budget snapshot, and committed artifact references; constructors clone byte slices/maps and accessors return copies. `AgentResult[O]` has exactly one outcome: success value, retryable typed failure, blocked checkpoint, review request, permanent failure, or cancellation evidence.

There is no runtime `Services` parameter. Construct each concrete Fake step once with its distinct capability value; because the types have disjoint fields, Prepare cannot compile against Sandbox/Similarity, Exercise cannot compile against LLM/Cache/Mutation, and Checkpoint cannot compile against Sandbox/LLM/raw artifact writes. The supplied metered implementations already encapsulate Task 4 authorization and cannot expose raw dispatch constructors.

- [x] **Step 5: Implement the concrete pipeline**

`Slice1Pipeline` stores three differently typed fields, not a slice. Its methods expose one typed stage at a time and validate fixed names/revision. The Fake adapters derive every output from input/config digests and can deterministically request artifact, cache, mutation, sandbox, BLOCKED, review, retry, failure, and cancel paths.

- [x] **Step 6: Implement `RunService`**

```go
type RunService interface {
	Generate(context.Context, domain.RunRequest) (domain.RunSnapshot, error)
	Resume(context.Context, domain.RunID) (domain.RunSnapshot, error)
	Cancel(context.Context, domain.CancelRequest) (domain.RunSnapshot, error)
}
```

Generate assigns RunID, acquires exclusive run and shared artifact locks, creates the fixed projections, starts the cancel/accounting pollers, and runs until pause/terminal. Resume acquires the same locks, reconciles sandbox state, interrupts any leftover RUNNING attempt, applies pending review or starts a new same-stage retry attempt for a BLOCKED run. That fresh attempt must revalidate the checkpoint dependency through the normal metered port before any ordinary stage operation; only current evidence can continue, while failure returns to BLOCKED. There is no probe mode or dependency-probe attempt. Trash reconciliation remains an explicit exclusive-maintenance operation and never runs under the shared workflow lock. Cancel first inserts the control request without waiting for the execution lock; if `TryAcquireRun` returns `ErrBusy`, the active executor's configured poller delivers cancellation. If it acquires the lock, Cancel reconciles sandbox and finishes cancellation itself.

- [x] **Step 7: Implement active-time accounting**

The coordinator starts injected-clock accounting and cancel pollers only while executing RUNNING work and joins both before releasing locks. Each accounting tick calls Task 4's single-transaction `AccountActiveTime`; the authoritative budget result sets the next deadline and exhaustion outcome, while `runs.active_elapsed_ns` remains only its same-transaction projection. The heartbeat does not own the run or authorize work. On crash recovery, charge through `last heartbeat + one configured interval`, capped by the immutable run limit; exhaustion cancels new work and reaches NEEDS_REVIEW only after sandbox cleanup settles.

- [x] **Step 8: Run focused, race, and full tests**

```powershell
gofmt -w internal/domain internal/workflow internal/adapter/fake internal/application
go test -race ./internal/domain ./internal/workflow ./internal/adapter/fake ./internal/application -run 'Test(RunView|AgentResult|Slice1Pipeline|SourceBoundary|RunService|Resume|Cancel|ActiveTime)' -count=1
go test ./... -count=1
go vet ./...
```

Expected: PASS.

- [x] **Step 9: Commit**

```powershell
git add -- internal/domain internal/workflow internal/adapter/fake internal/application
git commit -m "phase1(slice-1): run fixed local workflow"
```

### Task 9: Extend the existing CLI with strict local configuration and lifecycle commands

**Files:**
- Create: `internal/config/config.go`
- Create: `internal/config/config_test.go`
- Create: `internal/application/bootstrap.go`
- Create: `internal/application/bootstrap_test.go`
- Modify: `internal/cli/run.go`
- Modify: `internal/cli/run_test.go`
- Modify: `cmd/cpgen/main.go`

**Interfaces:**
- Consumes: Task 8 RunService, Task 3 read/review stores, existing doctor/watchdog commands, and strict YAML configuration.
- Produces: the Phase 1 `generate/run/review/config` CLI contract with stable human/JSON output and exit codes.

- [x] **Step 1: Write failing strict-config tests**

Decode only these Slice 1 sections:

```yaml
storage:
  state_root: C:/absolute/project/.cpgen
sqlite:
  busy_timeout: 5s
  max_readers: 4
runtime:
  lock_poll_interval: 25ms
  control_poll_interval: 100ms
  accounting_heartbeat: 1s
  cleanup_wait: 10s
fake_workflow:
  scenario: review
```

Derive database, artifact, runtime-lock, temp/quarantine/trash, and work paths as fixed children of the one canonical absolute `state_root`; they are not separately configurable. Reject unknown/duplicate keys, trailing YAML documents, relative/root/symlink-alias state roots, invalid duration relationships, zero/negative values, unsupported Fake scenarios, inline credentials, and values whose canonical effective-config digest changes across identical loads. `config effective --redact` must not expose secrets or raw environment values.

- [x] **Step 2: Write failing CLI contract tests**

Cover:

```text
config validate
config effective --redact
doctor
generate --request request.yaml
run list [--state]
run show
run events [--after-version]
run resume
run cancel --reason
review show
review revise --step --patch --reviewer --reason
review retry --budget-patch --reviewer --reason
review waive --gate --evidence --reviewer --reason
review reject --reviewer --reason
```

All stateful/config commands use one explicit global form, `cpgen --config <path> <command> ...`; the path is resolved relative to the caller's current directory, canonicalized before reading, and never discovered from ambient directories or environment. `help`, `version`, the existing fully-flagged `doctor`, and internal `sandbox-watchdog --control` remain config-independent. Test help, missing/duplicate `--config`, unknown command, malformed IDs, nonexistent run, immediate typed lock conflict, invalid state, strict versioned JSON envelope, stdout/stderr separation, redaction, deterministic ordering, and exit codes 0/2/3/4/5/6/7/8/9/10. `generate` with the Slice 1 review fixture exits 6. Cleanup timeout exits 10 only when the run remains RUNNING and exact SandboxExecution rows are CLEANUP_PENDING under an armed watchdog.

- [x] **Step 3: Run tests and verify failure**

```powershell
go test ./internal/config ./internal/application ./internal/cli ./cmd/cpgen -run 'Test(Config|Bootstrap|CLI|Generate|RunCommand|ReviewCommand)' -count=1
```

Expected: FAIL because config/bootstrap/lifecycle commands are absent.

- [x] **Step 4: Implement strict configuration**

Implement the config package with a `go.yaml.in/yaml/v3` import first, then pin it so `go mod tidy` retains the dependency:

```powershell
$env:GOTOOLCHAIN='go1.24.13'
go get go.yaml.in/yaml/v3@v3.0.3
go mod tidy
go mod edit -go=1.24.0
Remove-Item Env:GOTOOLCHAIN
```

Use `yaml.Node` to detect duplicate keys before typed decode. Apply explicit defaults, reject unknown fields, canonicalize the single absolute state root without creating it, derive all children internally, and validate all durations. Return field-qualified typed errors. Do not add workflow-service, remote-worker, provider-secret, or arbitrary Docker option fields.

- [x] **Step 5: Implement stateful bootstrap**

```go
func Bootstrap(context.Context, config.Config) (*Application, error)

type Application struct {
	Runs        RunService
	Runtime     port.RuntimeStore
	Reviews     port.ReviewStore
	Maintenance *ArtifactMaintenance
	Locks       *runlock.Manager
}

func (a *Application) Close() error
```

This Slice 1 bootstrap is explicitly Fake-only: it creates private derived roots, opens/migrates SQLite, constructs run locks with configured `Options`, Blob/domain ledgers, artifact maintenance, and the Fake pipeline. Its narrow sandbox reconciler returns an empty report only when no unfinished real SandboxExecution exists and otherwise fails closed. The real Docker reconciler is constructed only by Task 10's opt-in harness from an already validated explicit `dockersandbox.Config`; the stateful Slice 1 CLI does not pretend to run a real Docker workflow. Bootstrap starts no daemon or background worker. `Application.Close` joins any foreground pollers and idempotently closes stores/roots/lock manager in reverse construction order.

- [x] **Step 6: Extend the existing CLI parser**

Preserve `help`, `version`, `doctor`, and the internal `sandbox-watchdog` command. Parse the single global `--config` before stateful commands and add explicit parsers for the commands above; do not add a second CLI package. Review mutation commands use `TryAcquireRun` and map `ErrBusy` immediately to code 4. Cancel inserts its request before its own `TryAcquireRun`; `ErrBusy` means the foreground poller owns delivery, not a CLI failure. JSON output contains exactly one object with schema version, status, data or typed error, and run version.

- [x] **Step 7: Run focused, race, and full tests**

```powershell
gofmt -w internal/config internal/application internal/cli cmd/cpgen
go test -race ./internal/config ./internal/application ./internal/cli ./cmd/cpgen -run 'Test(Config|Bootstrap|CLI|Generate|RunCommand|ReviewCommand)' -count=1
go test ./... -count=1
go vet ./...
```

Expected: PASS.

- [x] **Step 8: Commit**

```powershell
git add -- go.mod go.sum internal/config internal/application internal/cli cmd/cpgen
git commit -m "phase1(slice-1): expose local workflow CLI"
```

### Task 10: Prove process-crash recovery and formal Docker persistence

**Files:**
- Create: `internal/integration/slice1_process_test.go`
- Create: `internal/integration/slice1_crash_test.go`
- Create: `internal/integration/slice1_lock_boundary_test.go`
- Create: `internal/integration/slice1_docker_test.go`
- Create: `internal/integration/slice1_docker_crash_test.go`
- Create: `internal/integration/testenv_test.go`
- Modify: `internal/probe/package.go`
- Modify: `internal/probe/package_test.go`

**Interfaces:**
- Consumes: public CLI/application APIs and real file/Docker adapters only.
- Produces: end-to-end evidence for local ownership, restart, budgets/artifacts, external-I/O transaction boundaries, and watchdog cleanup.

- [x] **Step 1: Write subprocess lock and restart tests**

Launch real `cpgen` helper subprocesses against one database/runtime/artifact root. Verify one executor per run, automatic lock release after force-kill, two different runs progressing concurrently, read-only show/events during execution, cancel delivery to a live owner, cancel cleanup after owner death, and review command conflict while the executor holds the run lock.

- [x] **Step 2: Write durable-boundary crash tests**

Add named failpoints after each committed boundary for run create, stage begin, budget reserve, DISPATCHING, SENT, physical completion, Blob SEALED/published/READY, occurrence commit, stage finish, and sandbox resource phases. Force-kill the helper at each failpoint, run `cpgen run resume`, and assert:

```text
no half projection/event
no duplicate logical or physical idempotency key
no budget over-settlement
no current occurrence from an interrupted/stale attempt
no leaked active writer/pin after convergence
UNKNOWN is not retried under a new key
old Docker operation performs no new Create/Start/export
```

- [x] **Step 3: Write the external-I/O lock-boundary test**

Block fake network send, Docker create/inspect/remove, Blob hash/fsync/link/rename/OpenVerified, watchdog IPC, and sandbox reconciliation after their durable preparation commits. While each is blocked, use another SQLite connection and a different run to begin/finish a stage and append events within the busy bound. Fail if any write transaction spans the blocked operation.

- [x] **Step 4: Write the opt-in real Docker A+B canaries**

Under `CPGEN_RUN_DOCKER_CANARY=1` and `cpgen_slice0_probe`, construct the real Docker reconciler/Runner directly from the existing explicit validated `dockersandbox.Config` fixture, then run the fixed A+B compile/run through persisted call/budget, SandboxExecution/watchdog, prepared artifact, occurrence, and cleanup ledgers. This opt-in harness is separate from the Fake-only Slice 1 CLI bootstrap. Add three force-kill cases: target running; target stopped before export; cleanup in progress. Add watchdog process death and control EOF. Resume must reconcile exact resources, interrupt the old stage attempt, and rerun under new stage/call IDs when needed.

After every canary assert zero Slice 1 containers/volumes, no unclean sandbox rows, exact terminal CallTrace/budget/artifact digests, and an empty Docker label query for the test run.

- [x] **Step 5: Run the integration suite**

```powershell
go test -race ./internal/integration -run 'TestSlice1(Process|Crash|LockBoundary)' -count=1 -timeout=20m
$env:CPGEN_RUN_DOCKER_CANARY='1'
go test -tags=cpgen_slice0_probe ./internal/integration -run 'TestSlice1Docker(AB|Crash|WatchdogFailure)' -count=1 -v -timeout=45m
Remove-Item Env:CPGEN_RUN_DOCKER_CANARY
```

Expected: PASS. If Docker is unavailable, the canaries report an explicit capability skip; all non-Docker integration tests still pass.

- [x] **Step 6: Run complete static and race verification**

```powershell
gofmt -w cmd internal
gofmt -l cmd internal
go test ./... -count=1
go vet ./...
go test -race ./... -count=1
go test -tags=cpgen_slice0_probe ./... -count=1
go vet -tags=cpgen_slice0_probe ./...
go test -race -tags=cpgen_slice0_probe ./... -count=1
```

Expected: `gofmt -l` has no output and every command passes.

- [x] **Step 7: Commit**

```powershell
git add -- internal/integration internal/probe
git commit -m "phase1(slice-1): prove local crash recovery"
```

### Task 11: Audit Slice 1 boundaries and record the stage checkpoint

**Files:**
- Create: `docs/evidence/slice1-verification.md`
- Modify: `README.md`
- Modify: `TODO.md`
- Modify: `docs/traceability.md`
- Modify: `docs/superpowers/plans/2026-08-31-slice1-lightweight-local-workflow.md`

**Interfaces:**
- Consumes: the complete Slice 1 implementation and fresh verification output.
- Produces: a clean, reviewable Slice 1 stage checkpoint and the starting boundary for Slice 2.

- [x] **Step 1: Run architecture and source-boundary audits**

```powershell
pwsh -NoProfile -File scripts/check-slice1-architecture.ps1
rg -n 'go.temporal.io|langgraph|autogen|crewai' go.mod go.sum internal cmd
rg -n 'database/sql|modernc.org/sqlite|internal/runlock|adapter/storage|adapter/sandbox/docker' internal/workflow internal/adapter/fake
rg -n -i 'SetState|SetStage|type\s+Services\b|execution[ _-]?lease|lease[ _-]?epoch|owner[ _-]?epoch|\bOwnerID\b|fencing([ _-]?(token|epoch))?|Cause(LeaseLost|Quiesce)|\bPROBING\b|\bQUIESCING\b|recovery[ _-]?intent|observation[ _-]?(ticket|floor)|startup[ _-]?janitor' internal cmd -g '*.go' -g '!*_test.go'
rg -n -i 'TBD|TODO|implement later|fill in|appropriate error|handle edge|similar to' internal cmd
```

Expected: architecture script passes; no hosted-runtime dependency, forbidden Step imports, generic state setter, discarded runtime table/type, or implementation placeholder appears. Test fixture strings are documented individually if a negative test intentionally contains one.

- [x] **Step 2: Verify database and filesystem integrity**

For every retained integration fixture DB, run migration checksum verification, `PRAGMA foreign_key_check`, and `PRAGMA integrity_check`. Under the exclusive artifact maintenance lock, scan canonical/temp/quarantine/trash roots and require every occurrence to reference verified READY bytes, no canonical file to be non-regular or multi-link, and no untracked temporary/trash file after reconciliation.

- [x] **Step 3: Run the final Go 1.24 and cross-platform gate**

```powershell
$env:GOTOOLCHAIN='go1.24.13'
go version
go env GOVERSION GOOS GOARCH CGO_ENABLED
gofmt -l cmd internal
go test ./... -count=1
go vet ./...
go test -race ./... -count=1
$env:GOOS='linux'
$env:GOARCH='amd64'
$env:CGO_ENABLED='0'
go test -exec 'cmd.exe /c exit 0' ./...
Remove-Item Env:GOOS,Env:GOARCH,Env:CGO_ENABLED,Env:GOTOOLCHAIN
git diff --check
git status --short
```

Expected: Go 1.24.x, no formatting output, all commands pass, and only planned evidence/status files remain uncommitted.

- [x] **Step 4: Record evidence and update status**

`docs/evidence/slice1-verification.md` records exact versions/commands/results, migration hashes, database integrity, process-lock crash proof, failpoint matrix, Docker capabilities/cleanup, artifact scan, architecture audit, and every Slice 1 commit ID. Mark Slice 1 complete in README/TODO/traceability, set Slice 2 as next, and check every completed box in this plan.

- [x] **Step 5: Commit the Slice 1 checkpoint**

```powershell
git add -- docs/evidence/slice1-verification.md README.md TODO.md docs/traceability.md docs/superpowers/plans/2026-08-31-slice1-lightweight-local-workflow.md
git commit -m "phase1(slice-1): complete lightweight local workflow"
```

- [x] **Step 6: Re-run post-checkpoint verification**

```powershell
$env:GOTOOLCHAIN='go1.24.13'
go test ./... -count=1
go vet ./...
go test -race ./... -count=1
Remove-Item Env:GOTOOLCHAIN
git status --short
```

Expected: PASS and a clean worktree.
