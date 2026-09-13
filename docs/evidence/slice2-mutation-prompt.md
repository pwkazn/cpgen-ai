# Slice 2 mutation provider contract

Status: Accepted

Checkpoint: 2026-09-09 08:06 UTC+8. MUT-02b adds a compiled `idea.mutate/v1` prompt and its single optional format-repair prompt. Its input is `cpgen.idea-mutation-draft-input/v1`; its output remains content-only IdeaDraftV1 under a separate schema digest binding `BindMutation/v1`. Initial Idea and Statement prompt bytes and references are unchanged.

The contract covers selected-parent Similarity mutation and full-batch no-feasible regeneration, preserves frozen request constraints and requests the exact authorized candidate slots. Providers cannot supply grant, identity, lineage or budget fields. Initial draft input/schema references cannot resolve the mutation prompt.

The local HTTP contract fixture consumes one generic claim, receives an invalid original response, performs one bounded format repair and replays the durable result. Two HTTP requests retain separate traces/artifacts and exact token usage; repair and replay leave logical mutation claims at one. Local binding produces the next batch with the recorded intent. This lower-level fixture does not exercise checked route authorization, source history retention or a production mutation attempt.

Focused prompt, strict schema, repair, durable replay and local binding tests pass. Complete normal/vet, full race, Linux command build, architecture, formatting and patch checks pass in the shared sweep recorded in [atomic completion evidence](slice2-atomic-mutation-stage.md). The current preview has no call site selecting this prompt.
