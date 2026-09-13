# Task 10 fix round 2 report

## Blockers closed

- `InterruptStage` now releases pending artifact writer tokens and their active/releasable pins in the same SQLite transaction that records the interrupted attempt. Finalized tokens with an attached occurrence remain owned by that occurrence.
- `RunService.Resume` now accepts a narrow `RunRecovery` hook and invokes it while the run lock is held, before current-stage recovery. Durable adapters can settle identities left at a process boundary without minting a new call or attempt.
- Crash integration coverage now leaves committed dispatch/artifact/sandbox state in the killed helper and performs recovery through the public `Resume` path. Artifact recovery settles the original physical identity conservatively (`UNKNOWN`) and releases uncommitted writer ownership; an occurrence already committed remains finalized and attached.
- The opt-in Docker crash canary now persists the probe call before entering a real Docker `Ping`, resumes the exact physical identity, and verifies watchdog EOF cleanup removes the named container and control files.
- External-I/O boundary helpers block only after durable preparation, allowing the concurrent SQLite progress assertions to exercise a closed write transaction.

## Verification evidence

- `gofmt` and `git diff --check`: PASS
- `go test ./internal/adapter/storage/sqlite ./internal/application ./internal/integration`: PASS
- `go test ./...`: PASS
- `go test -race ./internal/integration -run 'TestSlice1(Process|Crash|LockBoundary)' -count=1 -timeout=20m`: PASS
- `go test -race ./... -count=1 -timeout=20m`: PASS
- `go test -tags cpgen_slice0_probe ./... -count=1 -timeout=20m`: PASS on rerun (the first run hit a transient Windows `TerminateProcess: Access is denied` in the existing runlock subprocess test; its isolated retry and the complete rerun passed)
- `go test -tags cpgen_slice0_probe ./internal/integration`: PASS; live Docker canaries were capability-skipped because no Docker daemon is available on this host
- `go vet ./...`: PASS
- `go build ./cmd/cpgen`: PASS

The generated `cpgen.exe` test artifact was removed before commit. Live Docker execution remains an environment prerequisite for the tagged crash/watchdog canaries; their tagged code compiles and the non-Docker suite remains green.
