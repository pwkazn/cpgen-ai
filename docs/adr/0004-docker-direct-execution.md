# ADR-0004: Run Target Programs Directly in an Isolated Docker cgroup

Status: Accepted (amended by ADR-0006)

Date: 2026-08-30

## Context

Judge measurements must describe the untrusted target rather than a shell, helper, or exporter. Docker remains outside target code, and the host-side runner must retain authoritative timing, output, stop, and resource evidence.

## Decision

- A trusted Go Runner outside the container creates and starts each target directly with an explicit argv and no shell.
- The target is the measured cgroup process. Transfer helpers, keepers, and exporters are separate roles and never contribute target verdict measurements.
- Every Docker action belongs to a persisted SandboxExecution and exact planned resource identity. Authorization binds RunID, AttemptID, SandboxExecutionID, logical operation, scope, plan, and engine identity.
- The foreground CLI holds the per-run process lock while it may authorize new Docker work. Database lifecycle versions reject stale or duplicate commands within that process.
- CPU, wall time, memory, pids, stdout, stderr, exit status, OOM, and stop evidence are collected by trusted host code.
- Target containers have no Docker socket, no network unless a specific profile permits it, a read-only root filesystem, explicit mounts, dropped capabilities, no-new-privileges, non-root credentials, and strict resource limits.
- Cross-stop output transfer, detached watchdog behavior, and exact-resource cleanup follow ADR-0005.

## Consequences

- Judge evidence refers to the target itself.
- Shell injection and wrapper-accounting ambiguity are removed.
- Platform capability checks may reject a host that cannot produce the required evidence.
- Cleanup authorization depends on persisted execution and resource identity, not process ancestry.

## Superseded design

<!-- Superseded design: begin -->
The former wording tied Runner calls to a distributed fencing claim. ADR-0006 replaces that ownership mechanism with the OS-backed run lock and expected lifecycle versions while retaining exact authorization identity.
<!-- Superseded design: end -->

## References

- ADR-0005
- docs/design/sandbox.md
- docs/evidence/slice0-verification.md
