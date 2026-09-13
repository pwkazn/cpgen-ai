# ADR-0005: Docker Execution Lifecycle and Cross-stop Artifact Transfer

Status: Accepted (amended by ADR-0006)

Date: 2026-08-30

## Context

The CLI process may die while Docker continues running. Output may need to survive target stop without granting the target access to artifact storage. Cleanup therefore needs a detached safety component and exact persisted resource identities.

## Decision

- The protocol remains docker-direct-v2.
- One logical Sandbox operation groups physical CallTrace records. Every real Docker create is budget-reserved and authorized before host I/O.
- Before the first create, the Runner persists SandboxExecution, the complete non-expanding resource plan, engine identity digest, deterministic names and labels, call roles, deadlines, and watchdog control digest.
- The watchdog receives the sealed plan, subscribes to engine events, performs the resource-kind baseline scan, and acknowledges each planned create before it is issued.
- After create returns, the Runner persists the engine resource identity before start. Target start is forbidden until the matching resource acknowledgement is durable.
- The detached watchdog stops or kills planned targets on deadline, owner-channel EOF, or parent death. It remains until all planned calls are terminal, final scans show no running targets, and cleanup evidence is settled.
- On a later CLI start or resume, a narrow sandbox reconciler inspects and cleans only exact identities recorded for that run. It cannot schedule stages, continue a prior export, publish artifacts, or mutate unrelated runs.
- The foreground CLI process lock controls authorization of new work. Cleanup uses persisted SandboxExecution and resource lifecycle versions and is safe to repeat.
- Target output is copied after target stop through a dedicated trusted transfer role, verified, and then promoted through the artifact writer. Watchdog code never creates Judge verdicts or publishes package artifacts.
- A run waits for proof that every untrusted target has stopped before committing CANCELLED, BLOCKED, READY, or another state that promises no target is active.

## Consequences

- CLI death cannot leave an unbounded target.
- Late-created resources remain discoverable through deterministic identities.
- Recovery is limited to exact Docker resources and repeatable evidence settlement.
- Export and artifact publication never continue implicitly after process death.
- The protocol retains Slice 0 tests for watchdog EOF, deadlines, kill escalation, deterministic labels, and evidence provenance.

## Superseded design

<!-- Superseded design: begin -->
The previous lifecycle described owner lease epochs, cleanup takeover, RUNNING/QUIESCING, and a startup janitor. ADR-0006 replaces those generic ownership and workflow concepts with the per-run process lock plus a narrow exact-resource reconciler.
<!-- Superseded design: end -->

## References

- ADR-0004
- docs/design/sandbox.md
- docs/evidence/slice0-verification.md
