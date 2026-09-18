# Design notes

Two problems shaped most of CPGen's architecture. Both come from the same root
cause: **the generator is unreliable, but the consequences of its mistakes are
expensive and hard to undo.** These notes explain how the system keeps those
mistakes bounded and cheap to recover from.

---

## 1. Unknown send boundaries: why "did that request go out?" is the hard question

### The problem

A model call is not a pure function. It costs money, it mutates state on a remote
service, and it can fail at a point where **the caller cannot tell whether the work
happened.**

Consider a POST that times out:

- The provider never received it → retrying is free and correct.
- The provider received it, generated a response, and the *response* was lost →
  retrying pays twice and the system has already been billed once.
- TCP accepted the bytes but the provider crashed before persisting anything →
  a retry may or may not be handled idempotently, depending on the vendor.

From inside the process these three cases are **indistinguishable**. A naive
implementation picks one interpretation and applies it everywhere. Both choices are
wrong in a way that matters:

| Naive choice | Failure mode |
|---|---|
| Always retry | Double-billing on lost responses; budget accounting drifts below reality |
| Never retry | Permanent stalls on transient network faults; requires human intervention for a blip |

CPGen's answer is to **not guess**, and instead to make the boundary an explicit,
typed, first-class value that gets persisted.

### The mechanism

Every physical external call carries a `PhysicalBoundary`
(`internal/domain/budget.go`). Three cases are distinguished:

~~~text
CONFIRMED_NO_SEND   we know, from a specific typed error, the request never left
COMPLETED           we have a response, so the request certainly arrived
UNKNOWN             anything else — the honest answer
~~~

The critical design decision is the **default direction of the fallback**. In
`internal/agent/openai.go`, the transport error path reads:

~~~go
if errors.As(err, &adapterErr) {
    // An injected transport may return a typed policy/transport error
    // carrying the explicit no-send boundary. Preserve that boundary;
    // every other transport error is conservatively treated as sent.
    return nil, 0, "", 0, !adapterErr.ConfirmedNoSend, adapterErr
}
~~~

`!adapterErr.ConfirmedNoSend` means: **only an explicit, typed assertion of
no-send is trusted.** Dial failures, TLS errors, unexpected EOF, canceled contexts,
and anything unrecognized all fall through to `UNKNOWN`. The system never infers
"probably didn't send" from a generic symptom — it requires positive proof.

This is deliberately asymmetric. A false `UNKNOWN` costs a conservative budget
charge and possibly a human look. A false `CONFIRMED_NO_SEND` costs silent
double-spend and, worse, a corrupted audit trail. The first is a nuisance; the
second destroys the property the whole system exists to provide.

### Cost accounting follows the same asymmetry

Once a boundary is `UNKNOWN`, the run may still be charged. Since provider-reported
usage is unavailable by definition, CPGen charges a **documented upper bound**
rather than zero (`internal/agent/openai.go`):

~~~go
// Provider usage is optional. Bytes are a conservative token upper bound
// for accounting purposes (one UTF-8 byte cannot encode more than one
// token), and output is charged at the configured reservation ceiling.
input := int64(requestBytes)
if input < 0 {
    input = 0
}
return port.Usage{InputTokens: input, OutputTokens: maxOutput}, "conservative_upper_bound_v1"
~~~

The `usageSource` string (`conservative_upper_bound_v1`) is persisted alongside the
number. This matters for auditability: a later reader can distinguish "we know the
provider charged X" from "we assumed at most Y," without needing to reconstruct the
original incident.

Budgets are **reserved before** irreversible work and settled monotonically after.
The reservation is what makes the conservative fallback safe — the money is already
set aside, so an `UNKNOWN` settlement can only *release* headroom, never overspend.

### Recovery is graded, not generic

Crash recovery does not apply a single retry policy. `ARCHITECTURE.md` §10 defines
per-stage rules driven by what the ledger says happened:

| Ledger state | Recovery action |
|---|---|
| No external effect authorized | Rerun the stage freely |
| Effect sent, stable identity recorded | Replay or reconcile the *same* provider identity |
| Boundary unknown | Settle conservatively; pause for typed review or fail |
| Blob published, not yet attached | Verify the bytes, then attach or release the writer token |
| Sandbox resources persisted | Reconcile *only* those exact resource identities |
| Result already committed | Advance to the next stage normally |

The same reasoning extends to Docker. `internal/adapter/sandbox/docker/runner.go`
persists a `SandboxExecution` and its complete planned resource set **before**
calling `create`, so a crash mid-create leaves a durable record of what might exist.
A later reconciler can inspect, stop, kill, wait, and remove *only* those named
resources. It cannot start new work or publish artifacts.

### What this buys

- A crash never silently double-charges or silently loses work.
- Recovery predictability comes from state, not from heuristics about error strings.
- The audit trail distinguishes measured facts from conservative assumptions.
- The system fails *closed* toward human review rather than *open* toward
  unaudited spending.

### The tradeoff, stated honestly

Conservative settlement can overcharge a run that actually failed to send, and
`UNKNOWN` outcomes can push a run into review that a human would have judged
"obviously fine, just retry." That friction is accepted on purpose. The alternative
— a system that is usually right about whether money was spent — is not auditable,
and auditability is the product.

---

## 2. Why an LLM must never be the judge of its own output

### The problem

The obvious architecture for "generate a competitive-programming problem" is a
pipeline of model calls: ask for a statement, ask for a solution, ask for test data,
ask a model to check whether the solution passes the data. It demos beautifully.

It is also wrong, for a reason that is structural rather than a matter of model
quality: **the same faculty that produces a plausible wrong answer is the one that
would be asked to detect it.** A model that writes a subtly incorrect reference
solution will, with high probability, also declare that solution correct — it
generated a coherent story and will continue that story. Adding "please double-check"
does not change this, because the error is not carelessness; it is the absence of an
executable ground truth.

For a problem archive the failure is severe and delayed. A wrong reference solution
does not break anything visibly. It silently produces wrong `.ans` files, which then
misjudge every future submission — including correct ones. The damage surfaces months
later, in someone else's contest.

### The mechanism

CPGen's rule is simple to state and load-bearing everywhere:

> **Generative models propose candidates. Deterministic validators decide.**

No stage accepts a model's assertion about correctness. Instead, the pipeline
compiles and executes:

~~~text
Request
  -> Idea        -> Statement   -> Similarity
  -> Solution    -> Data        -> Judge      -> Quality
  -> Package gates -> READY
~~~

`READY` is only reachable when the run atomically references a verified package
occurrence *and* a final quality report. There is no code path that reaches `READY`
from a partially verified state, and the model has no vote.

### Separating "the program misbehaved" from "the answer is wrong"

The subtlest part is that testlib-style tools report **business results through exit
codes**, and non-zero exit codes conventionally mean "error." A validator signals
"this input is invalid" with a non-zero exit. If the sandbox mapped any non-zero exit
to a generic failure, the pipeline could not distinguish these:

- The contestant's solution is wrong → **WA**, a legitimate verdict
- The validator rejected malformed input → **INVALID**, a legitimate verdict
- The checker crashed → **infrastructure failure**, not a verdict at all

ADR-0003 resolves this by layering the outcomes and making the *role* decide the
interpretation, rather than the exit code alone:

~~~text
CompileOutcome   = OK | CE | INFRA_ERROR
ProcessOutcome   = EXITED | SIGNALED | TLE | MLE | OLE | INFRA_ERROR

ValidatorOutcome = VALID | INVALID | VALIDATOR_ERROR
CheckerOutcome   = AC | WA | PE | CHECKER_ERROR
SolutionVerdict  = OK | RE | TLE | MLE | OLE
~~~

with a fixed, versioned exit-code mapping (`testlib_v1`, pinned by digest in the
toolchain manifest) and `INFRA_ERROR` taking **precedence** over the role adapter.
The sandbox itself never produces RE, WA, or PE — it does not know what those mean.
It reports what the process did; the Judge decides what it means.

The consequence that matters: **a checker crash cannot be laundered into a
content-failure verdict.** It short-circuits to `BLOCKED` or `FAILED` before reaching
the role adapter, and it cannot trigger content "repair" — regenerating a perfectly
good problem because a trusted tool crashed would be a far worse outcome than
stopping.

### The model is not allowed to author its own evidence

This is the sharpest edge of the design, so it is enforced at the read path rather
than trusted at the write path.

When a later stage consumes an earlier result, it re-verifies the committed chain
instead of believing the recorded summary. As `ARCHITECTURE.md` puts it, the verified
read path for solution checking:

> verifies the current successful stage, original source bundles, frozen Docker
> request identities, result receipts, retained streams/programs and completed
> cleanup, then recomputes token comparisons from verified stdout bytes. It rejects
> uncommitted output, changed input/policy, missing source or stdout and unrelated
> extra artifacts. **The model cannot supply this execution evidence.**

In other words, the comparison between expected and actual output is not read from
a report the model could have influenced. It is **recomputed from stdout bytes that
were captured by the trusted runner and content-addressed on write.** If the bytes
are missing or their digest does not match, the read fails closed.

Storage reinforces this. Every artifact is declared before writing, streamed to a
private temp file while hashing, then atomically promoted to its canonical
SHA-256 identity. An *occurrence* binds those bytes to the producing run, stage
attempt, role, and revision — so "the reference solution that this test data was
validated against" is a checkable fact, not a claim in a prompt.

### What this buys

- A hallucinated reference solution produces a **rejected run**, not a corrupt `.ans`
  file. The blast radius of a model error is one run, not one archive.
- Infrastructure faults and content faults are routed differently, so a flaky
  checker never causes a good problem to be regenerated.
- Every acceptance decision traces to an executable artifact — a compiler exit, a
  process outcome, a token comparison, a digest — that a third party can re-run.
- The exported ZIP can be independently revalidated by a fresh CLI invocation,
  because nothing essential lives only in the model's context.

### The tradeoff, stated honestly

This is slower and more expensive than asking a model to check itself, and it needs
a real Docker Engine, a pinned toolchain, and a working testlib setup. It also means
the system *cannot* generate problems whose correctness is not executable — no
proof-based scoring, no partial credit, no subjective statement quality. Those limits
are accepted: a generator that cannot prove its output is correct has no business
writing to a judge's test data.

---

## A note on the abandoned approach

ADR-0006 originally placed a LangGraph-style scheduler in the application layer. It
was removed. Two things went wrong, and both are instructive:

1. **It duplicated the source of truth.** SQLite already persisted stage state;
   the graph library persisted its own checkpoints. Two stores could disagree, and
   reconciling them was strictly harder than having one.
2. **It leaked a provider library into domain contracts.** Flow definitions began
   expressing scheduling concerns in domain types, which is the boundary the port
   layer exists to protect.

The replacement is a **fixed loop compiled into the binary**. Stage order lives in
`internal/workflow/definition.go` and is not editable at runtime; database rows
select a compatible definition but never define graph edges. The type's own doc
comment states the intent directly:

~~~go
// Definition is a compiled compatibility contract, not a configurable graph.
// Persisted names remain unchanged when application constructors evolve.
type Definition struct {
    revision        string
    stages          []domain.StageName
    generation      bool
    preserveAttempt bool
}
~~~

LangChainGo remains confined to `internal/agent`.

The general lesson, and the reason this is recorded rather than quietly reverted:
**flexibility that is not required is a liability.** A runtime-editable graph was
never a product requirement for a single-host, single-process-per-run tool. The
abstraction cost was paid immediately and the benefit was never collected.

For the full account, see
[`docs/evidence/architecture-follow-up-2026-09-14.md`](evidence/architecture-follow-up-2026-09-14.md)
and [ADR-0006](adr/0006-lightweight-local-workflow.md).
