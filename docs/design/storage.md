# SQLite 投影、领域账本、制品与缓存

状态：ADR-0006 下现行有效

## 1. 范围

SQLite 是固定 CPGen 流水线的本地持久化投影与审计存储。它不是重放引擎，也不是通用调度器。产品专属账本在进程重启后仍保持预算、调用、制品、沙箱、评审与题包的完整性。

## 2. 数据库配置

- 私有运行时目录下仅一个数据库；
- 有序、经校验和验证的迁移；
- foreign_keys=ON, journal_mode=WAL, synchronous=FULL;
- 忙等待超时受本地配置约束；
- 改变状态的事务显式使用 BEGIN IMMEDIATE；
- 注入规范时钟与稳定序列化；
- 写事务内不得进行外部 I/O、哈希、fsync、rename、已验证 Blob 读取、Docker 操作、provider 调用或阻塞等待。

只读命令使用普通快照。改变状态的 run 命令还持有由操作系统支撑的 run 锁。

## 3. 工作流投影

最小表集为：

~~~text
runs(
  run_id PK,
  request_digest,
  config_digest,
  workflow_revision,
  schema_version,
  state,
  current_stage_name,
  current_stage_ordinal,
  run_version,
  active_elapsed_ns,
  active_started_at?,
  last_accounting_heartbeat_at?,
  cancellation_summary?,
  final_package_occurrence_id?,
  final_quality_report_id?,
  created_at,
  updated_at
)

stage_records(
  run_id,
  stage_name,
  stage_ordinal,
  workflow_revision,
  schema_version,
  input_digest,
  output_digest?,
  state,
  attempt_count,
  current_attempt_id?,
  logical_idempotency_key,
  last_error_code?,
  last_error_digest?,
  updated_at,
  PRIMARY KEY(run_id, stage_name)
)

stage_attempts(
  attempt_id PK,
  run_id,
  stage_name,
  ordinal,
  input_digest,
  logical_idempotency_key,
  state,
  output_digest?,
  result_kind?,
  error_code?,
  started_at,
  finished_at?,
  UNIQUE(run_id, stage_name, ordinal)
)

run_events(
  event_id PK,
  run_id,
  run_version,
  event_type,
  payload_digest,
  canonical_payload,
  created_at,
  UNIQUE(run_id, run_version)
)

control_requests(
  request_id PK,
  run_id,
  kind,
  reason_digest,
  state,
  created_at,
  applied_at?,
  UNIQUE(run_id, kind, state)
)

review_decisions(
  decision_id PK,
  run_id,
  expected_run_version,
  workflow_revision,
  stage_name,
  input_digest,
  kind,
  state,
  evidence_digest,
  policy_digest,
  payload_digest,
  created_at,
  applied_at?,
  applied_run_version?
)
~~~

投影变更与其 run 事件在同一事务中提交。run_events 是仅追加的审计流；运行时状态直接从 runs 与 stage_records 读取。

## 4. 预期版本转换

每次转换都接受预期 run 版本与目标当前阶段。一个短事务：

1. 校验 run 状态、当前阶段、版本与取消防护；
2. 校验被引用的 attempt 与领域证据；
3. 恰好应用一次投影更新；
4. 推进 run 版本；
5. 追加恰好一个有序事件；
6. 提交或返回带类型的冲突。

同一操作 ID 可安全重放。重复请求返回此前已提交的结果。调用方绝不通过写入任意状态来修复冲突。

## 5. 预算与调用账本

~~~text
budget_accounts(
  run_id,
  dimension,
  limit_value,
  reserved_value,
  settled_value,
  account_version,
  PRIMARY KEY(run_id, dimension)
)

call_records(
  call_id PK,
  logical_operation_id,
  run_id,
  attempt_id,
  call_kind,
  provider,
  request_digest,
  policy_digest,
  idempotency_key,
  dispatch_state,
  outcome_kind?,
  call_trace_digest?,
  authorized_at,
  completed_at?,
  UNIQUE(logical_operation_id, call_id)
)

budget_reservations(
  reservation_id PK,
  run_id,
  attempt_id,
  call_id,
  dimension,
  reserved_value,
  settled_value?,
  state,
  UNIQUE(call_id, dimension)
)
~~~

授权在不可逆工作之前预留所有已声明的维度。派发生命周期区分未发送、已发送、已完成、未知与本地完成这几类结果。结算单调，且不得超过配置的账户上限。发送边界未知时保留原调用身份并按保守方式计费。

CallTrace 记录物理的 provider 或 Docker 交互、计时、请求与响应摘要、重试分类以及结果证据。缓存结果有其自身的逻辑调用证据，并引用原始来源调用。

## 6. 制品账本

~~~text
artifact_declarations(
  declaration_id PK,
  run_id,
  attempt_id,
  call_id?,
  role,
  media_type,
  logical_path,
  max_bytes,
  declaration_digest
)

writer_tokens(
  writer_token_id PK,
  declaration_id,
  state,
  expected_digest?,
  expected_size?,
  final_digest?,
  final_size?,
  created_at,
  finalized_at?
)

blobs(
  digest,
  size,
  state,
  canonical_relative_path,
  verified_at?,
  PRIMARY KEY(digest, size)
)

blob_pins(
  pin_id PK,
  writer_token_id,
  digest,
  size,
  state,
  created_at,
  released_at?
)

artifact_occurrences(
  occurrence_id PK,
  run_id,
  attempt_id,
  call_id?,
  declaration_id,
  writer_token_id,
  digest,
  size,
  role,
  logical_path,
  media_type,
  revision_digest,
  provenance_digest,
  created_at
)
~~~

存在外部生产者时，所有来源影子列均为 NOT NULL，复合外键证明同一 run 的归属。occurrence 绑定到原始的产出 attempt 与调用，绝不绑定到临时的进程归属。

## 7. Blob 写入与已验证读取协议

1. 在短事务中声明角色、媒体类型、逻辑路径与大小上限。
2. 创建一次性 writer token 与预算预留。
3. 将字节流写入私有临时文件，同时计算哈希并强制限制。
4. 在数据库事务之外执行 fsync。
5. 原子地发布或去重规范摘要与大小。
6. 在短事务中终结 token、结算字节、固定 Blob 并记录证据。
7. 在阶段提交时创建 run 作用域的 occurrence，并按策略释放或保留 pin。

摘要-大小身份冲突、路径穿越、符号链接逃逸、已验证读取损坏或 token 重用均失败关闭。高信任消费方在使用前重新计算哈希。

## 8. 缓存账本

~~~text
cache_entries(
  cache_key PK,
  kind,
  schema_version,
  policy_digest,
  value_digest,
  expires_at?,
  state
)

cache_entry_sources(
  cache_key,
  source_call_id,
  source_occurrence_id?,
  PRIMARY KEY(cache_key, source_call_id)
)

cache_blob_refs(
  cache_key,
  digest,
  size,
  role,
  PRIMARY KEY(cache_key, digest, size, role)
)

cache_uses(
  cache_use_id PK,
  cache_key,
  run_id,
  attempt_id,
  logical_call_id,
  source_digest,
  created_at
)
~~~

缓存键使用规范的、带版本的输入。命中时检查策略、过期、来源证据与 Blob 就绪状态，然后校验字节以及所属适配器的输出契约，之后才创建当前 run 的逻辑缓存命中调用与重用记录。阶段事务将一个 CACHE_REUSE occurrence 附加到该记录；它不借用来源 writer token，也不计入另一次物理写入。来源调用可以属于同一 run 中更早的阶段/attempt，而当前调用与 occurrence 仍绑定到当前 attempt。跨 run 来源始终被禁止。过期的 provider 健康快照可以用于诊断，但在没有当前 attempt 的新鲜检查时不能解除某个阶段的阻塞。

## 9. 沙箱账本

~~~text
sandbox_executions(
  sandbox_execution_id PK,
  run_id,
  attempt_id,
  logical_operation_id,
  scope_digest,
  plan_digest,
  engine_identity_digest,
  watchdog_control_digest,
  state,
  lifecycle_version,
  created_at,
  cleaned_at?
)

sandbox_resources(
  resource_id PK,
  sandbox_execution_id,
  ordinal,
  kind,
  call_role,
  deterministic_name,
  expected_labels_digest,
  engine_resource_id?,
  identity_evidence_digest?,
  state,
  lifecycle_version,
  UNIQUE(sandbox_execution_id, ordinal)
)

sandbox_events(
  sandbox_event_id PK,
  sandbox_execution_id,
  resource_id?,
  lifecycle_version,
  event_type,
  evidence_digest,
  created_at
)
~~~

SandboxExecution 与完整的、不扩张的计划资源集在 Docker create 之前提交。授权绑定 RunID、AttemptID、SandboxExecutionID、LogicalOperationID、ScopeDigest、PlanDigest 与 EngineIdentityDigest。物理授予只增加 call ID、资源序号与角色。

确定性名称与标签加上引擎身份，使看门狗与后续对账器能够检查和清理确切的资源。清理命令使用预期的生命周期版本，并在取消后仍然有效。它们不能授权目标启动、继续导出，也不能触碰持久化计划中不存在的资源。

## 10. 题包账本

~~~text
packages(
  package_id PK,
  manifest_digest,
  tree_digest,
  created_at
)

package_occurrences(
  package_occurrence_id PK,
  package_id,
  run_id,
  relation,
  state,
  verification_receipt_id?,
  final_quality_report_id?,
  created_at,
  verified_at?
)

verification_receipts(
  verification_receipt_id PK,
  package_id,
  structure_digest,
  semantic_digest,
  toolchain_digest,
  created_at
)
~~~

题包 occurrence 只有在结构门禁与语义门禁通过后才变为 VERIFIED。一个延迟的同一 run 约束要求处于 READY 的 run 引用带有最终质量报告的 VERIFIED occurrence。发布与 READY 投影更新原子提交。

## 11. 评审与控制完整性

每个 run 至多存在一个活跃的取消请求与一个活跃的 PENDING 评审决定。取消插入是前台执行器持有 run 锁期间唯一允许在没有 run 锁的情况下执行的变更命令；该事务只插入请求。持锁进程观察并应用它。

评审创建仅在 NEEDS_REVIEW 中允许，并记录预期版本与证据绑定。应用评审在手动恢复期间、持有 run 锁的情况下进行。

## 12. 重启对账

恢复时，具名的幂等操作只检查当前 run 及其领域账本：

- 关闭或重放当前阶段 attempt；
- 对账原始 provider 调用身份；
- 校验已发布的 Blob 状态与 writer token；
- 保守地结算预留；
- 停止并清理确切的 SandboxExecution 资源；
- 应用已提交的阶段结果，或启动新的 attempt。

不存在其行用于调度任意恢复工作的数据库表。每个操作在提交前校验预期的 run 或生命周期版本。

对于真实 LLM 阶段组合，M22 保留不可变的逻辑开启与完成命令及其调用记录。每个有界回执与相应的调用转换原子提交，并在读取时对照既有命令摘要进行校验。应用层阶段绑定账本可以在活跃时间记账之后刷新乐观版本，同时重放完全相同的原始逻辑命令。它在每次变更时检查当前 RUNNING 阶段 attempt，只重试有界的数据库版本冲突，绝不重试 provider I/O。没有保留命令元数据的旧式调用需要其确切的原始命令才能被采纳。该桥接是组合的前置条件；被中断的 attempt 对账仍必须在任何新 attempt 之前解析已派发的身份。

## 13. 垃圾回收

GC 是显式的维护命令。有状态的 run 命令持有共享的全局制品锁；GC 取得其独占形式，只选择未被引用的 READY Blob，在短事务中重新检查引用，在事务之外将字节移动到私有回收站，然后记录删除。崩溃恢复可以幂等地还原或完成回收站条目。

## 14. 测试

必需的测试覆盖迁移、外键、预期版本冲突、投影与事件的原子性、预算竞争、重复调用结算、Blob 路径穿越与损坏、缓存来源、题包 READY 约束、每个持久化边界的崩溃注入，以及验证在 SQLite 写事务内不执行任何外部 I/O。
