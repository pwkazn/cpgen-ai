# Idea 与 Statement 契约

2026-09-09 用户调整：首个 MVP 先完成查重通过后的 Solution/数据/Judge/打包闭环，查重未通过或无可行 Idea 先人工复核。Mutation lineage 及其已实现契约保留为暂停研究，待重新设计；不再作为正向闭环的前置要求。当前顺序见 [闭环计划](../superpowers/plans/2026-09-09-mvp-generation-loop.md)。

## 1. 输入与输出

供应商只返回版本化的 `IdeaDraftV1` / `StatementDraftV1` 内容草稿；严格解码拒绝额外标识、digest、资源限制或选择关系字段。应用用已验证输入和现有领域构造器生成最终 `IdeaBatch` / `ProblemSpec`，不要求模型计算 SHA-256。Idea 草稿输入提供候选数量与本地派生 seed axes；Statement 草稿输入提供完整已解析链。旧完整领域输出提示词版本保留用于已有私有凭据验证。

RunView 的 schema 是持久化请求的 `cpgen.request/v1`；Idea 的类型化输入另有 `cpgen.request-snapshot/v1`。两者分别校验，不能互换。

Idea Step 只读取已持久化、已校验的 `GenerationRequestSnapshotV1`（其中包含提交的 `GenerationRequestV1` 与 `effective_seed`），输出：

```text
IdeaBatch
  request_digest, requested_count, candidates[]
  generation_policy_version, effective_seed

IdeaCandidate
  idea_id, candidate_ordinal, seed_axes[], abstract_task, intended_algorithm
  target_complexity, feasibility_status(FEASIBLE|REJECTED)
  feasibility_reasons[], negative_constraints[]
  parent_idea_id?, mutation_reason?, mutation_ordinal?

IdeaSelection
  request_digest, idea_batch_digest, selected_idea_id, selection_policy_version
  ordered_candidate_ids[], reason_codes[], evidence_digests[]

IdeaMutationIntent
  mutation_id, request_digest, source_batch_digest, parent_idea_id?
  trigger_kind(NO_FEASIBLE|SIMILARITY), trigger_evidence_digest
  stage_scope_digest, mutation_ordinal, mutation_reason, mutation_budget_claim_id

IdeaMutationRecord
  mutation_intent_digest, logical_operation_ids[], budget_reservation_ids[]
  budget_settlement_digest, output_batch_digest

StatementInput
  request_snapshot_digest, idea_batch_digest, idea_selection_digest
  selected_idea_id
```

Statement Step 只接受 `StatementInput`，通过 digest 解析同一 request snapshot、IdeaBatch、IdeaSelection 和 selected IdeaCandidate，输出结构化 ProblemSpec。提交前必须证明 selection.request/batch digest 与输入一致、selected ID 属于该 batch 且为 FEASIBLE，并把 request digest、selection digest、语言、资源限制及 required/forbidden constraints 固定到 ProblemSpec revision；链条不完整时不得生成题面。

## 2. 候选与选择策略

- 默认一次生成 4 个候选，可配置范围为 2–8；实际数量、调用预算和 policy version 进入 IdeaBatch。
- `len(candidates)` 必须等于 `requested_count`，`candidate_ordinal` 在 batch 内从 0 连续递增；`idea_id` 由 request digest、effective seed、batch/mutation ordinal 与规范化候选内容确定性派生。`seed_axes` 只能来自版本化 seed derivation policy，不能由重试时钟或未持久化随机数补充。
- 先执行确定性 Schema、required/forbidden、算法复杂度和输入规模可行性检查，REJECTED 候选不得进入主观排序。
- 对 FEASIBLE 候选使用版本化选择 policy 产生稳定有序列表；相同 batch/evidence/policy 必须得到同一 selected ID。LLM 可以提供理由候选，但不能覆盖确定性淘汰结果。
- `ordered_candidate_ids` 只保存全部 FEASIBLE 候选的稳定排序；IdeaBatch 以 `candidate_ordinal` 保留 FEASIBLE/REJECTED 全量候选。IdeaSelection 还保存 reason codes 和 evidence digests，不能只保存最终题意。
- 首个 MVP 没有 FEASIBLE 候选时直接进入 NEEDS_REVIEW，不申请 mutation quota，不自动创建新候选。

## 3. Mutation lineage

以下为暂停的历史设计及已有研究契约，不是首个 MVP 的执行要求；恢复开发前需要重新设计。

每次变异在任何 LLM dispatch 前先按 `stage_scope_digest=H(run_id, workflow_revision, stage_name, policy_version)` 取得配额，并创建不可变 `IdeaMutationIntent`。`mutation_budget_claim_id` 只占用 `max_mutations_per_stage` 的逻辑配额，不指向尚不存在的物理调用 reservation。`trigger_kind=SIMILARITY` 时 evidence 必须是触发的 SimilarityDecision；`NO_FEASIBLE` 时 evidence 是上一批确定性 feasibility report，且 parent 可为空。新候选保存适用的 `parent_idea_id`、`mutation_reason` 和同 parent（无 parent 时同 request/source batch）唯一 ordinal。新 IdeaBatch 产生后，再以 intent digest、实际产生的 logical operation/reservation IDs、结算快照 digest 和 output batch digest 创建不可变 `IdeaMutationRecord`；失败 intent 由 attempt/预算终态留证，不伪造 output record。新 batch 必须重新执行可行性筛选和确定性 IdeaSelection，随后才能生成 Statement；旧 selection 不能选择新 idea。禁止原地覆盖候选，也不能复制第三方题面。

`IdeaMutationCore` 在申请前冻结上述输入并计算 `core_digest`；该值对应通用账本的 `MutationClaimRequest.IntentDigest`。取得持久化 grant 后，再用 core 与完整 grant 构造 `IdeaMutationIntent` 及其独立摘要，避免 claim ID 和 intent digest 相互依赖。既有 Slice 1 grant 摘要保持结构体声明顺序的 JSON 协议；新内容摘要使用排序对象的 canonical JSON。

`idea-mutation-policy/v1` 为每个逻辑 claim 固定分配 8 个子序号槽，候选 mutation ordinal 为 `(logical_ordinal - 1) * 8 + candidate_ordinal + 1`，计算前检查上界。一次 claim 仍只消耗一个 CONTENT/METADATA 共享逻辑配额；批次保留冻结的 2–8 候选数量。`cpgen.idea-mutation-draft-input/v1` 保存原始 snapshot、source batch、bound intent、下一个 batch ordinal 和本地派生的子候选 seed axes，与初始 batch-zero 输入严格区分。契约的结构校验不证明 grant 已持久化；执行层必须在原子命令中重新核对其授权和当前状态。实现与完整验收见 [mutation contract evidence](../evidence/slice2-mutation-contracts.md)。

## 4. Statement 一致性

- ProblemSpec 的样例、约束、输入输出和 intended algorithm 都是结构化字段；Markdown 只由这些字段渲染。
- Statement revision 必须引用 selected idea、IdeaBatch/IdeaSelection digest、selection policy 和 request digest；ProblemSpec 自身不承载自由变化的 agent 理由。
- 样例属于 `InputOrigin=SAMPLE`；生成/导入测试分别为 `GENERATED_TEST/IMPORTED_TEST`。正式 Sample Gate 发现样例与 Validator 冲突时，由 Statement + Data/Validator 联合诊断。

## 5. 验收

持久化读取先恢复提交的原始 request JSON 和创建时的 effective seed，再读取当前仍为 SUCCEEDED 的阶段及其最新成功 attempt。已失效阶段中的历史成功记录不得作为当前输入。私有响应的 NEW_WRITE 或 CACHE_REUSE occurrence 必须绑定当前调用与原始生产者；缓存索引过期或失效不改变已提交 occurrence 的身份。

application 的 GenerationReader 根据冻结的草稿输入、prompt/schema、输出限制、采样和 provider/repair policy 重建原始调用计划，再验证已提交的原始响应或一次格式修复响应 Blob。领域构造器恢复 IdeaBatch/ProblemSpec，并核对阶段 output digest；重新派生的 IdeaSelection 还必须匹配 Idea 已提交的 Statement input digest。读取过程不得重新调用模型、补发未知请求、重置 seed 或依赖内存中的旧对象。该路径的实现与验收状态见 [committed-input evidence](../evidence/slice2-committed-generation-inputs.md)。

- manual/random 条件、标签 allowlist、required/forbidden 冲突和 seed 持久化使用 table-driven tests。
- 固定 effective seed/Fake LLM 得到稳定 IdeaBatch、IdeaSelection 和 mutation lineage。
- REJECTED 候选不能被选择；无可行候选且预算耗尽进入 NEEDS_REVIEW。
- selected idea、selection evidence 或 request digest 变化会使 Statement 及全部下游 revision 失效。
