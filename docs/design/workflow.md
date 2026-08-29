# 工作流、Revision 与预算设计

## 1. 范围

本文定义 Orchestrator 的静态工作流、依赖失效、attempt/checkpoint、并发汇合和预算预留协议。状态语义遵循 ADR-0002。

## 2. 标识与 Revision

```go
type RunID string
type StepName string
type AttemptID string
type Revision uint64
type Digest string
```

- 每个 run 有单调递增的 `run_version`，每次提交事务加一。
- `ProblemSpec`、`SolutionBundle`、`TestPlan`、测试集、答案集、Checker 和 Package 各有独立 revision 与内容 digest。
- Step 输入由显式 revision/digest 集合构成；输出证据必须保存完整输入 digest。
- 历史 attempt 永不覆盖，但 current snapshot 只引用与最新输入 digest 完全匹配的成功 attempt。

## 3. MVP 静态工作流

```text
GenerationRequestSnapshot
  -> IdeaBatch -> IdeaSelection
  -> ProblemSpec
  -> SimilarityEvidence -> SimilarityDecision
  -> [ReferenceSolution || BruteSolution]
  -> SolutionCompile -> SampleExecutionSmokeCheck
  -> TestPlan + Generator + Validator
  -> ValidatorCompile -> SampleGate
  -> SmallInputs -> DifferentialGate
  -> FormalInputs -> Answers -> ResourceGate
  -> PrePackageQualityGate
  -> InternalPackage -> Exporters
  -> PackageGate
  -> READY
```

SimilarityDecision 要求变异时不会直接回到 Statement：工作流先按稳定的 `stage_scope_digest` 在 `BEGIN IMMEDIATE` 中取得只计 `max_mutations_per_stage` 的 `mutation_budget_claim`，再写 `IdeaMutationIntent`。生成新 IdeaBatch 后结算实际调用 reservation，再写 `IdeaMutationRecord`，重新执行 feasibility/IdeaSelection，再以新的 `StatementInput` 进入 ProblemSpec。Intent 不预填未来物理 reservation ID；旧 batch/selection 保留为 lineage，但不能选择新 candidate。

`ReferenceSolution` 和 `BruteSolution` 可并行；测试组和答案可按文件并行。每个并行分支只能读取同一个 `RunView` revision，汇合点拒绝混合不同 revision 的结果。

并行分支出现 `Blocked` 时采用 quiesce-first：Orchestrator 先记录目标 blocked step 和原因，但 run 暂时保持 `RUNNING(mode=QUIESCING)`；取消同一并行组其他在途 context，等待其成功提交或完成取消/清理，然后单事务把 run 转为 `BLOCKED`。已经在取消前成功且输入 digest 匹配的 sibling 结果可以保留；内部 quiesce 导致的 Attempt 记为 `CANCELLED(cause=quiesce)`，其 Step 回到 `PENDING`，不能把 run 误设为用户取消。run 成为 BLOCKED 后不接受旧 sibling 的普通提交，只有 cleanup/fencing 事件可写。恢复时只重跑被阻塞或未完成的分支。

## 4. 依赖与失效规则

| 变化 | 必须失效 | 可保留 |
|---|---|---|
| Idea 或题目语义变化 | ProblemSpec 下游全部内容，包括 Similarity、解法、数据、答案和报告 | 历史 attempt/provenance |
| 仅展示字段变化且语义 digest 不变 | Statement Schema/Sample/Package Gate；若 Similarity query 使用完整题面则同时失效 Similarity | 已校验的算法与测试；是否为展示字段由 Schema 路径白名单判定，不能信任用户声明 |
| 输入/输出约束变化 | Similarity、解法、TestPlan、Validator、数据、答案、全部门禁 | 无当前下游证据 |
| 样例变化 | Sample Gate、Statement/Package | 正式测试证据 |
| 标程变化 | Compile、Sample、Differential、所有 `.ans`、Resource、Package | 原始测试输入 |
| 暴力程序变化 | Compile、Differential | 标程答案和正式测试；前提是 ProblemSpec 未变 |
| TestPlan 变化 | Generator 输出、Coverage、Validator 运行、答案、Resource、Package | 解法编译结果 |
| Generator 变化 | 生成测试、Determinism、Validator、Coverage、答案、Resource、Package | 手工固定回归输入 |
| Validator 变化 | 全部正式/小输入重新验证、Validator 正负例门禁、Package | 测试 Blob；被拒绝的当前测试不能进入后续门禁 |
| 默认 Checker 版本变化 | Sample、Differential、答案比较、Package | 输入和程序运行 stdout Blob |
| SPJ 变化（Phase 2） | Checker QA、Sample、Differential、全部输出判决、Package | 输入和程序编译结果 |
| verification profile 变化 | Resource Gate、QualityReport | 正确性门禁结果 |
| Similarity Policy 变化 | SimilarityDecision | 匹配版本仍有效的 SimilarityEvidence |

自动失效由依赖边表驱动，不通过手写“重跑后面所有步骤”。每次 `REVISE` 先提交新 revision 和失效事件，再调度新 attempt。

`presentation_only` 不是用户可自由声明的布尔值。MVP 只允许修改 Schema 明确列出的非语义字段，如 `title`、`presentation.story` 和 `presentation.notes`；自由 Markdown statement、输入输出、约束、样例或算法描述变化一律按语义变化处理。若 Similarity adapter 查询的是完整题面，任何被发送文本变化都使 Evidence 失效；只有查询绑定版本的规范化 semantic summary 时，纯展示变化才可保留 Evidence。

## 5. Step 与 attempt

```text
Attempt
  attempt_id, run_id, step_name, ordinal
  attempt_kind(STEP|DEPENDENCY_PROBE)
  lease_epoch
  input_digest, workflow_revision
  state, started_at, finished_at
  result_kind, output_digest?, evidence_digests[]
  blocked_dependency?, review_id?, error_code?
```

Agent 正常返回任一 AgentResult 后 AttemptState 均为 `FINISHED`，再由 `result_kind` 推进 Step/Run；AttemptState 不复刻业务结果枚举。

- `AttemptID` 在调度前生成并持久化，attempt 绑定当前 run lease_epoch。
- 相同 `(run_id, step_name, ordinal)` 唯一。
- Step 提交带 expected `run_version`；版本不匹配时重新读取快照，不能覆盖新 revision。
- `RUNNING` attempt 只有在旧 execution lease 过期且新 owner 取得更高 fencing token 后才能转 `ABANDONED`；若存在幂等完成记录则重放提交，否则创建下一 ordinal。
- 程序 `program_hard_deadline` 只产生 `RunResult.ProcessOutcome=TLE`；`step_deadline` 与 `run_budget_deadline` 是两个不同的 Orchestrator cause。
- context cancellation 必须携带：`user_cancel/quiesce/revision_invalidated/lease_lost/step_deadline/run_budget_deadline`。`step_deadline` 取消当前 Attempt，Step 按策略 RETRYING，预算耗尽后 NEEDS_REVIEW；`run_budget_deadline` quiesce 全部分支并原子进入 `NEEDS_REVIEW(budget=max_wall_time)`；只有 `user_cancel` 映射 RunState CANCELLED。

## 6. BLOCKED 恢复

Checkpoint 保存：

```text
blocked_step, attempt_id, input_digest, dependency_kind
dependency_identity, last_error_digest, last_failure_at
retry_after, probe_policy, min_observation_seq
```

恢复不会复用已经 `FINISHED + result_kind=BLOCKED` 的旧 STEP attempt，也不会在 `BLOCKED` 中旁路计量做网络/容器探测。`resume` 取得新的 execution lease 后，在一个事务中检查 fencing、checkpoint、input revision 和无 active CANCEL，原子执行 `BLOCKED -> RUNNING(mode=PROBING)`，并为同一 blocked step 的下一 ordinal 创建 `attempt_kind=DEPENDENCY_PROBE,state=RUNNING`，写入 `active_probe_attempt_id`；因此不存在“已进入 PROBING 但没有父 attempt”的已提交中间态。该模式只允许 checkpoint 声明的 dependency probe，不调度普通 Step，所有 probe AttemptCall 均绑定新 probe attempt_id。

probe 使用正常 AttemptCall/reservation/writer/fencing 协议，Similarity health 请求计入 `max_similarity_calls`，Docker probe 的每个实际 ContainerCreate 都计入 `max_sandbox_runs`，且整个探测区间计入 active wall time。`UNAVAILABLE` 先结束 probe attempt 为 `FINISHED + result_kind=BLOCKED`，再在同一事务把 run/step 提交回 BLOCKED；`HEALTHY` 结束为 PROBE_HEALTHY，把原 step 置回 PENDING/RETRYING 并切回 NORMAL；`INCOMPATIBLE` 结束为 `PERMANENT_FAILURE` 并把 run 转为 FAILED。三条路径都结算调用并原子清空 active probe，绝不继续旧 blocked/probe attempt。

若在上述原子转移前崩溃，run 仍为 BLOCKED；事务提交后崩溃则 run 为 RUNNING/PROBING 且必有 RUNNING probe attempt。新 owner 不能单独提交旧 probe ABANDONED；必须在一个延迟 FK 事务中插入新 RUNNING probe、切 active pointer 后再 abandon 旧 probe，或原子退出 PROBING 并清空 pointer。

## 7. Metered StepServices

Agent 不直接拿到 LLM/Sandbox/Similarity 客户端，而调用带计量代理。每一次真实发出的 LLM/Similarity 请求和每一次 Docker 容器创建都必须有独立 reservation，并通过 attempt ID 与逻辑调用 ID 关联；attempt 启动本身不预留所有未来调用。HTTP 重试循环位于 Metered proxy 控制下，Sandbox 固定 plan 也由其一次性预授权；禁止底层 adapter 在计量层以下静默重试或自行扩张预算：

```go
type MeteredLLM interface {
	Generate(ctx context.Context, req GenerateRequest) (MeteredOutcome[GenerateResponse], error)
}

type MeteredSimilarity interface {
	Search(ctx context.Context, req SimilaritySearchRequest) (MeteredOutcome[SimilarityEvidence], error)
}

type DependencyProbeRequest struct {
	Kind       DependencyKind
	Identity   DependencyIdentity
	Policy     ProbePolicyRef
}

type DependencyProbeEvidence struct {
	CallTrace     CallTrace
	Status        ProbeStatus // HEALTHY | UNAVAILABLE | INCOMPATIBLE
	Capabilities  CapabilitySnapshot
	PhysicalKind  ProbePhysicalKind
}

type MeteredOutcome[T any] struct {
	Value     *T
	Failure   *PortFailure
	CallTrace CallTrace
}

type PortFailure struct {
	Code       PortFailureCode
	Class      FailureClass // RETRYABLE | BLOCKED | INCOMPATIBLE | REJECTED | UNKNOWN
	Evidence   []PendingArtifact
	RetryAfter *time.Time
}

type CapabilityAttemptOutcome struct {
	CallID       AttemptCallID
	Capabilities *CapabilitySnapshot
	Failure      *PhysicalPortFailure
}

type CallTrace struct {
	LogicalOperationID       string
	DispatchKind             DispatchKind // DISPATCHED | CACHE_HIT | NO_DISPATCH
	ResultAttemptCallID      *AttemptCallID
	PhysicalAttemptCallIDs   []AttemptCallID
	CacheSourceAttemptCallID *AttemptCallID
	CachePinCallID           *AttemptCallID
	DecisionSourceCallID     *AttemptCallID
}

type MeteredDependencyProber interface {
	Probe(ctx context.Context, req DependencyProbeRequest) (DependencyProbeEvidence, error)
}

type MeteredSandbox interface {
	Compile(ctx context.Context, req CompileRequest) (MeteredOutcome[CompileResult], error)
	Run(ctx context.Context, req RunRequest) (MeteredOutcome[RunResult], error)
}

type MeteredArtifactSink interface {
	Prepare(ctx context.Context, declaration ArtifactDeclaration) (ArtifactWriter, error)
	PinExisting(ctx context.Context, blob BlobRef, declaration ArtifactDeclaration) (PendingArtifact, error)
}
```

`MeteredOutcome` 要求 Value/Failure 恰有一个；预期的 429/5xx、熔断、依赖不可用、协议不兼容和 profile 拒绝都返回 typed Failure + CallTrace，不能只丢一个普通 error。`MeteredDependencyProber` 的 UNAVAILABLE/INCOMPATIBLE 同样必须返回完整 evidence/trace。Go `error` 只表示 context cause、无法分类的内部错误或编程错误；若此前已有物理 dispatch，内部错误也必须先把 operation 收敛为 UNKNOWN 并把 trace 存入诊断 evidence。

`CallTrace` 的数组按 `attempt_calls.physical_ordinal` 严格递增且不得重复。`DISPATCHED` 要求 physical 列表非空、`ResultAttemptCallID` 属于同一 `CallOperation` 且包含在列表中，cache/decision-source 为空；成功时 result 指向产生值的 attempt，全部 retry 失败时指向使 policy 停止的首个不可重试 attempt，或最后一个已完成的可重试 attempt；若物理边界不确定而禁止安全重试，operation/PortFailure 为 UNKNOWN，result 指向造成不确定性的 call。`CACHE_HIT` 要求本次 physical 列表为空、result/decision-source 为空、当前 `CachePinCallID` 属于本 logical operation 且是 `CACHE_PIN/PROBE_CACHE_PIN+COMPLETED_NO_DISPATCH`，`CacheSourceAttemptCallID` 指向 cache value 的原始 producer。`NO_DISPATCH` 用于熔断、预算/策略预拒绝：physical/result/cache 字段为空，可选 `DecisionSourceCallID` 指向历史失败观察的 producer；它不能伪装成 cache hit。所有端口共用这一 Schema/校验器，不能各自定义近似 trace。

Step 只依赖 `MeteredLLM/MeteredSimilarity/MeteredSandbox`，不会接触 `DispatchAuthorization`。`MeteredDependencyProber` 只注入 Orchestrator 的 PROBING 路径，不提供给普通 Step。计量代理成功认领物理 `AttemptCall` 后，才把密封授权与请求分别传给底层 adapter。

`ProbePolicyRef` 固定每种依赖允许的物理动作和预算映射：Similarity health HTTP 计入 `max_similarity_calls`；LLM provider health/model HTTP 计入 `max_total_llm_calls`，只有 policy 显式允许时才可用最小 Generate；Docker Engine ping 不创建容器时只记录 AttemptCall，canary 的每个 ContainerCreate 都计入 `max_sandbox_runs`。每个 dependency identity + capability policy 的物理调用在 dispatch authorization 时分配单调 observation ticket，结果只能填写原 ticket；提交 BLOCKED 时把当时 counter 的 `last_seq+1` 保存为 `min_observation_seq`，从而排除故障前已授权但迟到完成的观察。只有同 identity、同 capability/profile policy、状态 HEALTHY 且 seq 达到 floor 的 CapabilitySnapshot，才能通过受 PROBING scope 约束的 `PROBE_CACHE_PIN/COMPLETED_NO_DISPATCH` 返回；普通 CACHE_PIN 在 PROBING 禁止。wall clock 只审计，不参与新鲜度判定。所有真正的 HTTP/ContainerCreate dispatch 使用新 AttemptCall，Metered proxy 将其组装成 CallTrace。

`doctor` 使用不绑定 run 的只读诊断端口，不扣 run budget，也不能生成 `DependencyProbeEvidence`、写入可供 PROBE_CACHE_PIN 使用的 capability observation/cache、更新 checkpoint 或推动 `BLOCKED -> RUNNING`。

LLM/Similarity/Sandbox adapter 的响应体、日志和输出 Blob 都只能写入该 run-scoped sink。每个物理调用的 raw response、stdout/stderr 和固定 Outputs 必须在 dispatch authorization 事务中按声明上限一次性 `Prepare` writer/reservation，不能等响应到达后再新建预算；未产生的输出最终 RELEASED。Adapter 只可流式写这些既有 writer。Sink 执行单文件/总量限制，并返回带 AttemptCall/reservation/FINALIZED writer token/ACTIVE pin、实际大小和 dedup/physical-new measurement 的 `PendingArtifact`；最终 `SETTLED/RELEASED` 由 Orchestrator 随 attempt 成功/失败事务提交，Sink 不提前重复结算，也不创建 ArtifactOccurrence。

cache hit 通过 `PinExisting`，顺序不能倒置：先在一个 `BEGIN IMMEDIATE` 守卫事务中检查 fencing、`RunState=RUNNING`、execution mode/current scope、无 active CANCEL、cache/规范 source/Blob/预算，然后原子创建 `CACHE_PIN/PROBE_CACHE_PIN + COMPLETED_NO_DISPATCH` AttemptCall、`cache_pin_use`、原 role/path/media declaration、零物理新增字节 reservation、FINALIZED writer token、永久 pin identity 与 ACTIVE live pin；capability probe 还必须由组合 FK 绑定达到 checkpoint floor 的 HEALTHY observation。事务提交后才对该 pin 指向的 Blob 调用 `OpenVerified`。cache entry 随后即使过期，GC 也必须因 pin 而跳过该 Blob。校验失败则使 cache/Blob 失效并收敛 reservation/pin，不得创建 occurrence；FINALIZED token 仅保留预期 digest 审计且不可复用。cache pin 不含外部 call-count reservation且永不能 dispatch。

### 7.1 预算账户

| 维度 | 预留单位 | 结算依据 |
|---|---|---|
| LLM calls | 1 次 | 实际是否发出请求 |
| LLM cost/tokens | PricingPolicyRef × 版本化 token 上界 × 最大输出，整数向上取整 | 校验后的 Provider usage × 固定费率；缺失/矛盾时使用预留上限 |
| Similarity calls | 1 次 | HTTP 请求是否发出 |
| Sandbox containers | 1 次 | 每个实际 ContainerCreate；transfer/keeper/target/export 均计入 |
| Artifact bytes | 声明输出上限 | 实际安全提升大小 |
| Wall time | 不做逐调用预留 | Orchestrator 按 `RUNNING` 且被有效执行/恢复 lease 覆盖的单调时钟区间累计 |

### 7.2 Reservation 状态

`RESERVED/SETTLED/RELEASED/UNKNOWN`。

AttemptCall 另使用 `AUTHORIZED/DISPATCHING/SENT/COMPLETED/UNKNOWN/ABORTED_NO_DISPATCH/COMPLETED_NO_DISPATCH`。后两者不是预算状态：`ABORTED_NO_DISPATCH` 表示经传输/容器边界确认物理动作从未发生，`COMPLETED_NO_DISPATCH` 只用于 NORMAL 的 CACHE_PIN 或 PROBING 的 PROBE_CACHE_PIN。

1. 每次逻辑调用先在 `BEGIN IMMEDIATE` 守卫事务中幂等创建 `CallOperation(OPEN)`，只检查 fencing、`RunState=RUNNING`、scope/CANCEL 和 logical idempotency key，不预留物理预算。若熔断、预算或策略在 dispatch 前拒绝，则在同一或紧邻事务将 operation 终结为 `FAILED + NO_DISPATCH`，不创建 AttemptCall/reservation，并返回无 physical/result/cache 字段的 CallTrace。需要真实调用时，再在第二个 `BEGIN IMMEDIATE` 事务中检查完整 execution capability 与预算余额，原子创建带 `capability_scope_digest` 的 AttemptCall、其全部 call/cost/artifact reservations 与预声明 writer tokens，并将 call 置 `AUTHORIZED`。HTTP/local 单物理调用创建一条；Sandbox 根据不可扩张 ContainerPlan 在同一事务为全部计划 ContainerCreate 创建多条 AUTHORIZED row，任一额度不足则只终结已有 OPEN operation 为 NO_DISPATCH，不产生任何物理 row。`NORMAL` 允许 current Step 调用，`PROBING` 只允许 checkpoint 指定 dependency/probe policy 的 call kind，`QUIESCING` 拒绝所有新授权；事务提交后 Sink 才按 token 建立随机临时 writer。
2. 执行者在含 fencing/scope/CANCEL 的守卫事务中以 CAS 唯一 claim `AttemptCall AUTHORIZED -> DISPATCHING`；只有成功者允许发出一次外部请求/创建一次容器，随后记录 SENT/COMPLETED。Sandbox aggregate capability 只允许按 plan 顺序在每次 ContainerCreate 紧前 claim 对应 row。CANCEL 若先提交则步骤 1 失败；若在 AUTHORIZED 后、不可逆 dispatch 前到达，确认未发送/未创建后 CAS 为 `ABORTED_NO_DISPATCH` 并释放 writers/reservations。已经越过边界则立即尝试 context cancellation/资源收敛，不得新增输出 reservation；状态不确定必须 UNKNOWN。DISPATCHING/SENT 崩溃按 UNKNOWN 或 Provider idempotency 恢复，绝不从任一单维度预算行重新 dispatch。
3. 收到可验证 usage 后结算差额。
4. 请求明确未发出时 AttemptCall 转 `ABORTED_NO_DISPATCH` 且 reservation 转 `RELEASED`；普通缓存命中走 CACHE_PIN，fresh probe cache 命中走 PROBE_CACHE_PIN，二者均为 COMPLETED_NO_DISPATCH；其他退款只改变 reservation 结算，不伪造 call 状态。
5. 崩溃导致是否调用未知时转 `UNKNOWN`，MVP 按预留上限保守计费；人工对账后才能调整。

并行 Step 共享数据库预算账户，但不能共享内存计数器。Artifact bytes 在写入临时 Blob 和提升前都检查上限。

- LLM/Similarity 一次逻辑调用的每个 HTTP retry 都有独立 physical ordinal/AttemptCall/reservations/dispatch idempotency key，并共享不可变 logical operation/input；429、5xx 或连接失败只影响该物理 attempt 的结算。Sandbox 的 ContainerPlan 不可扩张，计划内不追加 ContainerCreate retry；可重试故障收敛后创建新的 logical operation/plan。两种路径都不允许 adapter 内部重试绕过 `max_total_llm_calls/max_similarity_calls/max_sandbox_runs`。
- run 内为取得/刷新 capability snapshot 发出的 Similarity health HTTP 请求同样计入 `max_similarity_calls`；`doctor` 不属于 run，不扣 run budget，但记录独立诊断指标。有效本地 health snapshot/cache hit 不产生物理 call。
- Metered proxy 先查缓存；有效 cache hit 不消耗 LLM/Similarity call 或 Sandbox run 次数，但记录独立 cache-hit 指标，并以非物理 CACHE_PIN AttemptCall 创建 PendingArtifact/provenance，不能留下无父 reservation。
- `max_wall_time` 是 run active wall time：只累计 RUNNING 且被有效 execution/recovery lease 覆盖的区间。剩余额度触发 `run_budget_deadline`，必须 quiesce、结算并进入 NEEDS_REVIEW，不能用同一个 `deadline` 与 step 重试混淆。并行调用共享区间；单个 `step_deadline` 和程序 `program_hard_deadline` 另行限制。崩溃补计最多到旧 lease expiry，定义版本和时钟来源进入 QualityReport/provenance。
- `max_artifact_bytes` 按该 run 新写入 BlobStore 的物理字节增量结算；复用已有 Blob 不重复扣减。每次输出仍受逻辑单文件/总输出上限，即使 Blob 已缓存。
- lease 丢失时 Metered proxy 禁止新调用并取消 context；已在途且无法确认是否完成的 reservation 变为 `UNKNOWN`，由新 owner 保守结算。
- execution owner 的 heartbeat goroutine 同时轮询持久化的 CANCEL control request；收到后取消根 context、等待所有分支和容器收敛，再由当前 fencing token 提交唯一终态。control request 不是第二条工作流写通道。
- 新建 reservation/dispatch authorization、普通 attempt/current snapshot、review/budget patch 以及 `READY/FAILED` 终态提交都在同一 SQLite 写事务中检查 active CANCEL。CANCEL 先提交则取消优先且普通结果只能成为诊断 Blob；仅已有 reservation 的结算/资源清理可继续。终态先提交则 CANCEL 请求不能被接受。

## 8. ReviewDecision 应用

- `REVISE`：应用 JSON Merge Patch 或专用领域 patch，产生新 revision，然后执行依赖失效。
- `RETRY`：先提交预算增量或外部条件变化记录，再创建新 attempt。
- `WAIVE`：仅改变对应 Quality Gate 的评估输入；不修改原始 evidence。
- `REJECT`：在 resume 应用事务中提交事件并把任务转为 `FAILED`。

Quality Gate 只读取同时匹配 run、ProblemSpec revision、gate、policy version 和 evidence digest 的 active waiver。

所有 review 命令只创建 `PENDING` 决定。`run resume` 取得 lease 后重新校验绑定：有效则原子应用并标记 `APPLIED`；绑定已变化则标记 `STALE` 并保持 `NEEDS_REVIEW`；决定本身违反策略则标记 `REJECTED`。重复 resume 根据 review ID 和 applied_run_version 幂等返回。

## 9. 打包顺序

```text
PrePackage Gates PASSED
  -> build package in staging
  -> exporter(s)
  -> Package Gate reverse-read + hash validation
  -> atomic publish
  -> READY
```

Package attempt 受 `max_package_attempts` 限制。可修复的模板/路径错误回退 Package Agent；Schema 不兼容或制品缺失进入 `FAILED`，主观发布风险才进入 `NEEDS_REVIEW`。

正式目录 rename 成功但 `packages + READY` 事务尚未提交时，attempt 仍是 `RUNNING`。恢复必须重新读取磁盘并执行 Package Gate；只有 package ID 和全部当前 revision 绑定一致时才能幂等补交事务。

## 10. 验收条件

- 并行两个 LLM Step 无法共同越过一次调用预算。
- 修改标程后旧答案、Differential 和 Resource 证据不再出现在 current snapshot。
- `BLOCKED` 从原 step 恢复。
- NEEDS_REVIEW 的空操作 resume 被拒绝；BLOCKED resume 在 retry_after/backoff 和预算守卫允许时可进入受计量 PROBING。
- 旧 waiver 在 evidence 或 ProblemSpec revision 变化后失效。
- Package Gate 失败时 run 不得成为 `READY`。
