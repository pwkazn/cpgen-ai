# Idea 与 Statement 契约

2026-09-09 用户调整：首个 MVP 先完成查重通过后的 Solution/数据/Judge/打包闭环，查重未通过或无可行 Idea 先人工复核。变异谱系及其已实现契约保留为暂停研究，待重新设计；不再作为正向闭环的前置要求。当前顺序见 [闭环计划](../superpowers/plans/2026-09-09-mvp-generation-loop.md)。
> Current sample policy: [executed samples](executed-samples.md). Statement outputs are draft placeholders; official answers and explanations are finalized only after independent execution and Judge checks.

当前 V3 workflow 的样例答案不来自 Statement 或模型草稿：Statement 只提交样例输入，Solution/Judge 以 `cpgen.solution-verification/v2` 与 `cpgen.judge-verification/v2` 记录独立执行证据，Package 使用 `cpgen.package/v3` manifest。V1 保持历史语义，V2 的有界 `content_retries` 规则在 V3 中继续有效。


## 1. 输入与输出

供应商只返回版本化的 `IdeaDraftV1` / `StatementDraftV1` 内容草稿；严格解码拒绝额外标识、digest、资源限制或选择关系字段。应用用已验证输入和现有领域构造器生成最终 `IdeaBatch` / `ProblemSpec`，不要求模型计算 SHA-256。Idea 草稿输入提供候选数量与本地派生的 seed 轴；Statement 草稿输入提供完整已解析链。旧完整领域输出提示词版本保留用于已有私有凭据验证。

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

Statement Step 只接受 `StatementInput`，通过 digest 解析同一 request 快照、IdeaBatch、IdeaSelection 和选中的 IdeaCandidate，输出结构化 ProblemSpec。提交前必须证明 selection.request/batch digest 与输入一致、选中的 ID 属于该批次且为 FEASIBLE，并把 request digest、selection digest、语言、资源限制及 required/forbidden constraints 固定到 ProblemSpec revision；链条不完整时不得生成题面。

## 2. 候选与选择策略

- 默认一次生成 4 个候选，可配置范围为 2–8；实际数量、调用预算和策略版本进入 IdeaBatch。
- `len(candidates)` 必须等于 `requested_count`，`candidate_ordinal` 在批次内从 0 连续递增；`idea_id` 由 request digest、有效 seed、批次/变异序号与规范化候选内容确定性派生。`seed_axes` 只能来自版本化 seed 派生策略，不能由重试时钟或未持久化随机数补充。
- 先执行确定性 Schema、required/forbidden、算法复杂度和输入规模可行性检查，REJECTED 候选不得进入主观排序。
- 对 FEASIBLE 候选使用版本化选择策略产生稳定有序列表；相同批次/证据/策略必须得到同一选中 ID。LLM 可以提供理由候选，但不能覆盖确定性淘汰结果。
- `ordered_candidate_ids` 只保存全部 FEASIBLE 候选的稳定排序；IdeaBatch 以 `candidate_ordinal` 保留 FEASIBLE/REJECTED 全量候选。IdeaSelection 还保存理由码和证据 digest，不能只保存最终题意。
- 首个 MVP 没有 FEASIBLE 候选时直接进入 NEEDS_REVIEW，不申请变异配额，不自动创建新候选。

## 3. 变异谱系

以下为暂停的历史设计及已有研究契约，不是首个 MVP 的执行要求；恢复开发前需要重新设计。

每次变异在任何 LLM 派发前先按 `stage_scope_digest=H(run_id, workflow_revision, stage_name, policy_version)` 取得配额，并创建不可变 `IdeaMutationIntent`。`mutation_budget_claim_id` 只占用 `max_mutations_per_stage` 的逻辑配额，不指向尚不存在的物理调用预留。`trigger_kind=SIMILARITY` 时证据必须是触发的 SimilarityDecision；`NO_FEASIBLE` 时证据是上一批确定性可行性报告，且 parent 可为空。新候选保存适用的 `parent_idea_id`、`mutation_reason` 和同 parent（无 parent 时同 request/源批次）唯一序号。新 IdeaBatch 产生后，再以 intent digest、实际产生的逻辑操作/预留 ID、结算快照 digest 和输出批次 digest 创建不可变 `IdeaMutationRecord`；失败 intent 由 attempt/预算终态留证，不伪造输出记录。新批次必须重新执行可行性筛选和确定性 IdeaSelection，随后才能生成 Statement；旧的选择结果不能选择新的 Idea。禁止原地覆盖候选，也不能复制第三方题面。

`IdeaMutationCore` 在申请前冻结上述输入并计算 `core_digest`；该值对应通用账本的 `MutationClaimRequest.IntentDigest`。取得持久化授权后，再用 Core 与完整授权构造 `IdeaMutationIntent` 及其独立摘要，避免 claim ID 和 intent digest 相互依赖。既有 Slice 1 授权摘要保持结构体声明顺序的 JSON 协议；新内容摘要使用排序对象的 canonical JSON。

`idea-mutation-policy/v1` 为每个逻辑申请固定分配 8 个子序号槽，候选变异序号为 `(logical_ordinal - 1) * 8 + candidate_ordinal + 1`，计算前检查上界。一次申请仍只消耗一个 CONTENT/METADATA 共享逻辑配额；批次保留冻结的 2–8 候选数量。`cpgen.idea-mutation-draft-input/v1` 保存原始快照、源批次、绑定 intent、下一个批次序号和本地派生的子候选 seed 轴，与初始 batch-zero 输入严格区分。契约的结构校验不证明授权已持久化；执行层必须在原子命令中重新核对其授权和当前状态。实现与完整验收见 [变异契约证据](../evidence/slice2-mutation-contracts.md)。

## 4. Statement 一致性

- ProblemSpec 的样例、约束、输入输出和目标算法都是结构化字段；Markdown 只由这些字段渲染。
- Statement revision 必须引用选中的 Idea、IdeaBatch/IdeaSelection digest、选择策略和 request digest；ProblemSpec 自身不承载自由变化的 agent 理由。
- 样例属于 `InputOrigin=SAMPLE`；生成/导入测试分别为 `GENERATED_TEST/IMPORTED_TEST`。正式 Sample Gate 发现样例与 Validator 冲突时，由 Statement + Data/Validator 联合诊断。

## 5. 验收

持久化读取先恢复提交的原始 request JSON 和创建时的有效 seed，再读取当前仍为 SUCCEEDED 的阶段及其最新成功 attempt。已失效阶段中的历史成功记录不得作为当前输入。私有响应的 NEW_WRITE 或 CACHE_REUSE 实例必须绑定当前调用与原始生产者；缓存索引过期或失效不改变已提交实例的身份。

应用层 GenerationReader 根据冻结的草稿输入、prompt/schema、输出限制、采样和提供方/修复策略重建原始调用计划，再验证已提交的原始响应或一次格式修复响应 Blob。领域构造器恢复 IdeaBatch/ProblemSpec，并核对阶段 output digest；重新派生的 IdeaSelection 还必须匹配 Idea 已提交的 Statement 输入 digest。读取过程不得重新调用模型、补发未知请求、重置 seed 或依赖内存中的旧对象。该路径的实现与验收状态见 [已提交输入证据](../evidence/slice2-committed-generation-inputs.md)。

- manual/random 条件、标签允许列表、required/forbidden 冲突和 seed 持久化使用表驱动测试。
- 固定有效 seed/Fake LLM 得到稳定 IdeaBatch、IdeaSelection 和变异谱系。
- REJECTED 候选不能被选择；无可行候选且预算耗尽进入 NEEDS_REVIEW。
- 选中的 Idea、选择证据或 request digest 变化会使 Statement 及全部下游 revision 失效。
