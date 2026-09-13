# Slice 2 deterministic mutation content contracts

Status: Accepted

Checkpoint: 2026-09-09 06:52 UTC+8. MUT-01a supplies content contracts and cross-layer claim binding; it does not enable a business route.

## Identity and lineage

`IdeaMutationCore` binds run/workflow/config, exact submitted request and snapshot, source batch/ordinal/count, trigger evidence, selected parent when applicable, shared logical ordinal/limit and the versioned Idea stage scope. Its canonical digest is created before quota acquisition and becomes `MutationClaimRequest.IntentDigest`. `IdeaMutationIntent` separately binds that core to the returned durable grant. This avoids a hash cycle between the future claim ID and the original claim request.

Similarity mutations bind the retained rejection decision digest and a feasible parent in the verified source batch. The application plan constructor also fixes that parent to the committed selection. Parentless no-feasible regeneration binds a deterministic summary of every recorded rejected candidate. That summary reports recorded assessments; it does not establish algorithm correctness or substitute for Judge/Quality evidence. The source must be durably retained by the future route executor.

One shared logical claim produces the frozen 2–8 candidates. The versioned policy allocates eight ordinal slots per claim: child ordinal is `(logical_ordinal - 1) * 8 + candidate_ordinal + 1`. Overflow and exhausted immutable limits are rejected before arithmetic. A new batch uses the source batch ordinal plus one; content binding derives seed axes, candidate identities and frozen negative constraints locally. Initial batch-zero inputs and mutation inputs have distinct schemas.

The mutation input retains exact int64 seeds, source proof and the complete bound intent through canonical JSON reconstruction. Providers still return content-only `IdeaDraftV1`; they cannot set grant, source, seed, identity or lineage fields. Every successful batch needs a fresh deterministic selection before Statement can use it.

## Compatibility and verification

A real SQLite-grant test exposed an important existing protocol distinction: Slice 1 hashes grant receipts in declared Go struct JSON order, while content envelopes use sorted-object canonical JSON. Intent validation preserves the historical grant format. Existing grants and migrations are unchanged.

- Pure contract tests cover exact large seeds, deterministic replay, independent slices, stable unique child ranges, ordinal overflow, changed source/parent/trigger/claim/limit, full no-feasible evidence and strict malformed-envelope rejection.
- The application fixture reads a real committed rejection plan, constructs its core, acquires and exactly replays a low-level SQLite claim, binds the returned grant and constructs the mutation input. This checks the actual receipt hash protocol. No automatic route runs; preview state/version and two LLM/one Similarity HTTP counts remain unchanged.
- Complete normal tests and vet pass: application 56.731 s, SQLite 22.067 s, integration 6.364 s.
- Complete race sweep passes: application 1086.373 s, SQLite 338.342 s, domain 2.506 s, integration 103.677 s.
- Linux amd64 CGO-disabled command build passes, as do the 26-file architecture check, formatting of 318 Go files and patch whitespace.

These are structural contracts, not proof that a supplied grant exists in SQLite or permission to dispatch. Production still needs a checked atomic command, retained historical source references, compatible graph revision, provider execution and final mutation output/settlement records. Later generic mutation-record and artifact-settlement fixes receive separate acceptance; see the [routing plan](../superpowers/plans/2026-09-09-slice2-business-routing.md).
