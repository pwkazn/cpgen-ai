# Slice 2 durable candidate collection

Status: Accepted

Checkpoint: 2026-09-09 07:41 UTC+8. MUT-02a separates initial candidate collection from selection without selecting a new workflow revision.

`CollectIdeaCandidates` returns a validated initial batch and its private response occurrences even when every candidate is recorded as REJECTED. `ReadIdeaCandidates` reconstructs the current successful collection from committed private receipts and verifies its semantic output digest. Before commit, the response cannot be read as candidate proof. After database reopening, the full batch and deterministic no-feasible evidence remain unchanged without another provider request.

`ReadIdea` still additionally requires a feasible deterministic selection and the committed Statement input. A retained rejected batch cannot feed Statement. The original preview continues to use `RunIdea`, which returns review for a wholly rejected batch. Its real service regression stops at Idea with one LLM request, zero Similarity requests and no successful collection commit; Resume adds no work.

The collection fixture deliberately commits through the low-level storage primitive to exercise proof retention. It is not evidence of a complete no-feasible mutation route. A compatible future graph must make collection and selection/authorization distinct durable boundaries, preserve source references before invalidation, and acquire quota atomically.

Focused collection/generation-reader/executor tests pass. Complete normal/vet and race sweeps, Linux command build, architecture, formatting and patch checks pass with the exact shared sweep recorded in [artifact and mutation ledger evidence](slice2-artifact-mutation-records.md).
