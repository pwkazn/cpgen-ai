# ADR-0002：任务状态、审核与恢复

- 状态：Accepted
- 日期：2026-08-29

## 背景

基础设施暂时不可用与题目内容需要人工判断是两种不同情况。单纯把任务设回 `RUNNING` 也无法解释人工修改、追加预算或豁免门禁，容易反复命中同一失败点。

## 决策

### 任务状态

- `CREATED`：已保存请求，尚未运行。
- `RUNNING`：至少一个步骤可运行、正在运行或等待内部调度。
- `BLOCKED`：Docker、Similarity Service、模型或所选 profile 等外部条件暂时不满足。
- `NEEDS_REVIEW`：内容证据、预算或可豁免门禁需要人工决定。
- `READY`：所选 verification profile 的所有必需门禁和 Package Gate 已通过。
- `FAILED`：不可恢复失败或人工拒绝。
- `CANCELLED`：用户取消。

`RUNNING` 另有受约束的 execution mode：`NORMAL/PROBING/QUIESCING`。`PROBING` 仅用于 BLOCKED resume 后的依赖探测；`QUIESCING` 用于任何 context cause 后收敛在途调用和 SandboxExecution，包括用户取消、并行分支、revision/lease 失效和 step/run budget deadline。二者仍属于 RUNNING，因此共享 execution lease、预算、active wall、fencing 和 CANCEL 规则，不能调度无关普通 Step。

### 步骤与 attempt 状态

StepState：`PENDING/RUNNING/SUCCEEDED/RETRYING/BLOCKED/NEEDS_REVIEW/FAILED/CANCELLED/SKIPPED`。

AttemptKind 为 `STEP|DEPENDENCY_PROBE`。AttemptState：`PENDING/RUNNING/FINISHED/CANCELLED/ABANDONED`。`FINISHED` 表示调用已正常返回并持久化某个 `result_kind(SUCCEEDED|RETRYABLE_FAILURE|BLOCKED|PERMANENT_FAILURE|NEEDS_REVIEW|PROBE_HEALTHY|INTERNAL_ERROR)`，不等于 Step 成功；`PROBE_HEALTHY` 只允许 DEPENDENCY_PROBE attempt，结束后原 Step 仍必须创建新的 STEP attempt。其他业务含义由 result_kind 映射到 StepState/RunState。未分类内部错误以 `FINISHED + result_kind=INTERNAL_ERROR` 留证并使 Step/Run `FAILED`。`ABANDONED` 只用于恢复时标记崩溃前遗留的 attempt，不是 StepState 或 Agent 可返回的结果。旧 attempt 废弃后，Step 按恢复策略进入 `PENDING/RETRYING/RUNNING`。

Orchestrator cause 与状态落点固定如下；程序 `program_hard_deadline` 不属于 context cause，只产生 `ProcessOutcome=TLE`：

| Cause | Attempt/Step | Run |
|---|---|---|
| `user_cancel` | target STOPPED/调用收敛后 Attempt CANCELLED | 先 RUNNING/QUIESCING，唯一可进入终态 CANCELLED |
| `quiesce` | Attempt CANCELLED，Step PENDING | RUNNING/QUIESCING，收敛后去目标状态 |
| `revision_invalidated` | 收敛后 Attempt CANCELLED，Step PENDING | 短暂 RUNNING/QUIESCING 后恢复 NORMAL |
| `lease_lost` | 旧 Attempt 由新 owner ABANDONED | 保持 RUNNING 并恢复 |
| `step_deadline` | 收敛后 Attempt CANCELLED，Step RETRYING；重试耗尽后需审核 | RUNNING/QUIESCING 后 NORMAL 或 NEEDS_REVIEW |
| `run_budget_deadline` | quiesce 全部 Attempt 并结算 | RUNNING/QUIESCING 后 NEEDS_REVIEW，reason=`budget:max_wall_time` |

只有明确的用户 CANCEL control request 才把 Step/Run 提交为终态 CANCELLED。Orchestrator 必须记录具体 cause，不能只按 `context.Canceled` 推断。

### ReviewDecision

```text
ReviewDecision
  review_id, run_id, step_id, attempt_id
  problem_spec_revision, gate_id, policy_version
  evidence_digests[], decision, reason, reviewer, created_at
  input_patch?, config_patch?, budget_delta?, waiver_scope?
  status, applied_at?, applied_run_version?
```

`decision`：

- `REVISE`：必须产生新输入或配置 revision，并按依赖图失效下游证据。
- `RETRY`：必须增加预算或记录已变化的外部条件。
- `WAIVE`：仅适用于 Policy 声明可豁免的主观门禁。
- `REJECT`：决定被 resume 原子应用时，任务进入 `FAILED`。

Waiver 同时绑定 run、ProblemSpec revision、gate、policy version 和 evidence digest。任何绑定项变化后自动失效。Compile、Sandbox 完整性、正式输入合法性和 Package 完整性不可豁免。

ReviewDecision 生命周期为 `PENDING/APPLIED/REJECTED/STALE`。review CLI 只创建 `PENDING` 决定，不直接切换 run；`run resume` 在持有 execution lease 的事务中重新校验并应用。每个 run 最多存在一个 active PENDING 决定。

## 恢复规则

- `BLOCKED` resume 取得 lease 后，以同一事务进入只允许依赖探测的 `RUNNING(mode=PROBING)` 并创建新的 DEPENDENCY_PROBE attempt；探测按正常预算、取消和 active-wall 规则执行。健康后结束 probe attempt 并为检查点记录的原 step 创建新 STEP attempt，失败则结束 probe attempt 后回到 `BLOCKED`。禁止向旧 FINISHED blocked attempt 追加调用。
- `CREATED` run 可由 resume 在取得 lease 并重新校验配置快照后进入 `RUNNING`，用于恢复 create/first-step 之间的崩溃。
- `NEEDS_REVIEW` resume 必须有可应用且带实际输入/预算/policy/waiver 决定的 ReviewDecision，空操作被拒绝。`BLOCKED` resume 不要求调用前证明依赖已经变化；在 `retry_after/backoff` 允许且预算/CANCEL/fencing 守卫通过后，可进入 PROBING 做受计量探测。
- 用户取消会触发 Orchestrator context；run 先处于 RUNNING/QUIESCING，Sandbox 确认在途容器停止后才能提交 CANCELLED。
- 重启时遗留的普通 RUNNING attempt 在 recovery intent 的最终短事务中转为 ABANDONED；active probe 必须同时换父或退出 PROBING，不能先单独 abandon。
- 并行组某分支 BLOCKED 时，先取消并收敛 sibling，再将 run 提交为 BLOCKED；不允许 BLOCKED 后继续接受旧分支的未 fenced 结果。

## 后果

- 审核行为成为可审计领域数据，而非手工改数据库。
- CLI 必须支持 show/revise/retry/waive/reject。
- 状态恢复依赖持久化 step checkpoint 和输入 digest。
