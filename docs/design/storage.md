# SQLite、制品与缓存设计

## 1. 范围

MVP 使用单个 SQLite 数据库保存运行元数据，使用内容寻址目录保存 Blob。数据库是状态、引用和审计事实来源；文件系统 Blob 不携带唯一 provenance。

## 2. SQLite 配置

- 启用 foreign keys。
- 使用 WAL 模式和有限 busy timeout。
- 需要在“检查守卫 + 写入”之间线性化的 execution lease 获取、CANCEL 插入、attempt/current snapshot 和终态提交使用 `BEGIN IMMEDIATE`（或等价的单写者事务封装）；遇到 busy 按有限策略重试后返回分类冲突，不能退化为先读后写。
- 一个 run 只允许持有有效 lease/fencing token 的一个逻辑 writer；跨 run 可以并发。
- 所有时间同时保存 UTC wall clock 和需要时的 monotonic duration。
- Schema 通过顺序 migration 管理，禁止启动时自动猜测字段。

## 3. 核心表

### runs

```text
run_id PK, state, execution_mode?, relation(GENERATED|DERIVED|VERIFICATION),
run_version, workflow_revision
request_snapshot_json, request_snapshot_digest, effective_config_json, effective_config_digest,
current_problem_revision
verification_profile, created_at, updated_at
blocked_step?, active_probe_attempt_id?,
active_probe_kind? CHECK(active_probe_kind IS NULL OR active_probe_kind=DEPENDENCY_PROBE),
active_probe_state? CHECK(active_probe_state IS NULL OR active_probe_state=RUNNING),
active_review_id?, ready_package_occurrence_id?,
ready_package_status? CHECK(ready_package_status IS NULL OR ready_package_status=VERIFIED),
UNIQUE(run_id, relation),
CHECK(
  (state=RUNNING AND execution_mode IN (NORMAL, PROBING, QUIESCING))
  OR (state<>RUNNING AND execution_mode IS NULL)
),
CHECK(
  (execution_mode=PROBING AND active_probe_attempt_id IS NOT NULL
    AND active_probe_kind=DEPENDENCY_PROBE AND active_probe_state=RUNNING)
  OR
  (COALESCE(execution_mode, NONE)<>PROBING AND active_probe_attempt_id IS NULL
    AND active_probe_kind IS NULL AND active_probe_state IS NULL)
),
CHECK(
  (state=READY AND ready_package_occurrence_id IS NOT NULL
    AND ready_package_status=VERIFIED)
  OR
  (state<>READY AND ready_package_occurrence_id IS NULL
    AND ready_package_status IS NULL)
),
FOREIGN KEY(run_id, active_probe_attempt_id, active_probe_kind, active_probe_state)
  REFERENCES attempts(run_id, attempt_id, attempt_kind, state)
  DEFERRABLE INITIALLY DEFERRED,
FOREIGN KEY(run_id, ready_package_occurrence_id, ready_package_status)
  REFERENCES package_occurrences(run_id, package_occurrence_id, status)
  DEFERRABLE INITIALLY DEFERRED
```

`execution_mode` 仅在 `state=RUNNING` 时可为 `NORMAL/PROBING/QUIESCING`，其他状态必须为空。`PROBING` 只允许 blocked checkpoint 声明的依赖探测，`QUIESCING` 只允许在途工作收敛；两者不能调度普通新 Step。

`request_snapshot_json/effective_config_json` 是创建 run 时同一事务写入的不可变规范 JSON；后者已去密，只保存 endpoint/provider/key-ref 等可恢复配置，不保存 secret 值。两者的 digest 由数据库写入层重算并与 snapshot/domain 引用核对，后续 resume 不回读当前全局配置。因为内容内联在 run 行中，它们不受 Blob GC 影响。

### run_leases

```text
run_leases(run_id PK FK, owner_id?, lease_epoch,
           heartbeat_at?, expires_at?, acquired_at?, released_at?)
```

- 每个 run 的 lease 行/epoch 计数器创建后永不因普通释放而删除。释放只清空 owner 和有效期并写 `released_at`，`lease_epoch` 必须终身单调递增，禁止回绕或复用，避免旧 owner 的 ABA 提交。
- 每个 CLI 调度进程启动时生成随机 `owner_id`；`generate/resume` 调度前在事务中获取 lease，租约空闲或已过期时将 `lease_epoch` 原子加一并写入 owner。首次获取从固定初值开始。
- 活跃 lease 存在时第二个 CLI 返回状态冲突，不能执行恢复。
- owner 每隔固定间隔 heartbeat；默认 TTL/interval 由配置给出，且 TTL 明显大于 interval。
- 所有 run/step/attempt/budget/review/package 提交都携带 `(owner_id, lease_epoch)`，数据库以 fencing token 拒绝旧 owner。
- owner 发现 lease 丢失后立即取消根 context，不再发起 Metered 调用。
- run 进入 `BLOCKED/NEEDS_REVIEW/READY/FAILED/CANCELLED` 且所有在途工作已收敛后，owner 停止 heartbeat 并释放 lease；暂停态和终态不长期占用执行租约。
- 唯一的 RUNNING 主动释放例外是 CLI 的 cleanup handoff：必须先确认 run 为 QUIESCING、没有新 dispatch 能力、全部未停止 SandboxExecution 已持久化为 CLEANUP_PENDING 且 watchdog ACK 有效，再在同一 fencing 事务追加 `OwnerYieldedForCleanup` 并释放 lease。run 保持 RUNNING/QUIESCING；后续 owner 取得更高 epoch 后只能 cleanup-only TAKEOVER/收敛，旧 operation 不再 ContainerCreate/Start/export，不能调度普通 Step。不得把普通长任务用此机制伪装成后台 daemon。

### control_requests

```text
control_requests(request_id PK, run_id FK, kind(CANCEL), reason,
                 requested_by, requested_at, status,
                 acknowledged_at?, acknowledged_lease_epoch?)
```

`CANCEL` 是 execution lease 的唯一控制面例外：第二个 CLI 可以幂等插入取消请求，但不能借此修改 run/step/attempt。插入事务必须确认 run 仍是 `CREATED/RUNNING/BLOCKED/NEEDS_REVIEW`，数据库约束保证同一 run 最多一个 active CANCEL；若终态事务先提交，请求插入失败。活跃 owner 的 heartbeat goroutine 同时轮询请求并取消根 context；清理在途容器后，由该 owner 在 fencing 事务中把任务提交为 `CANCELLED`。若 owner 已失联，取消命令等待 lease 过期、取得更高 epoch、清理旧容器后再完成取消。这样既允许跨 CLI 取消，又不会产生第二个工作流 writer。

除 heartbeat、取消清理和“已有 reservation/资源”的收敛外，所有带 lease 的领域写事务（新建 reservation/dispatch authorization、attempt/current snapshot、review decision、预算增量、暂停态、package 和 `READY/FAILED` 终态）都必须在同一个 `BEGIN IMMEDIATE` 事务中断言：fencing token 有效、run 为可运行状态且不存在 active CANCEL。新 AttemptCall 还要检查 execution-mode capability：`NORMAL` 只允许 current scheduler/Step，`PROBING` 只允许 call kind、dependency identity 和 probe-policy digest 均匹配 blocked checkpoint 的 dependency probe，`QUIESCING` 禁止任何新授权；probe 内所有输出 writer 必须随该 probe call 预声明，不能另开普通 artifact session。CANCEL 插入先提交后不得新建 reservation 或授权物理调用；事务只能结算已有 reservation、保存受限取消诊断并走 `CANCELLED`。取消终态事务把尚未应用的 ReviewDecision 标为 `STALE(cause=run_cancelled)`。若 `READY/FAILED` 先提交，后续 CANCEL 插入因终态守卫失败。两者以 SQLite 写事务提交顺序形成唯一线性化结果，禁止出现“取消已接受但随后 READY”。

### events

```text
event_id PK, run_id FK, run_version, event_type
payload_json, actor, created_at
UNIQUE(run_id, run_version)
```

### steps / attempts

```text
steps(run_id, step_name, state, current_attempt_id, input_digest, output_digest)
attempts(attempt_id PK, run_id, step_name, ordinal,
         attempt_kind(STEP|DEPENDENCY_PROBE), lease_epoch, state,
         input_digest, result_kind, started_at, finished_at,
         dependency_identity?, probe_policy_digest?,
         output_digest, error_code,
         UNIQUE(run_id, attempt_id),
         UNIQUE(run_id, attempt_id, lease_epoch),
         UNIQUE(run_id, attempt_id, attempt_kind),
         UNIQUE(run_id, attempt_id, attempt_kind, state),
         UNIQUE(run_id, step_name, ordinal))

probe_authorizations(probe_authorization_id PK, run_id, probe_attempt_id,
                     lease_epoch, dependency_identity_digest,
                     capability_policy_digest, checkpoint_digest, created_at,
                     UNIQUE(probe_authorization_id, run_id, probe_attempt_id, lease_epoch),
                     FOREIGN KEY(run_id, probe_attempt_id, lease_epoch)
                       REFERENCES attempts(run_id, attempt_id, lease_epoch))
```

`attempts.state` 使用 ADR-0002 的调用生命周期枚举；`FINISHED` 必须带非空 `result_kind`，`PENDING/RUNNING/CANCELLED/ABANDONED` 不得伪装为 Step 成功。DEPENDENCY_PROBE 使用同一 blocked step 的下一唯一 ordinal，但不覆盖已 FINISHED 的旧 STEP attempt；probe 的所有 AttemptCall 均以新 probe attempt_id 为父。active probe 使用 SQLite 可实现的同行 CHECK + `DEFERRABLE INITIALLY DEFERRED` 四列组合 FK：shadow columns 固定 `DEPENDENCY_PROBE/RUNNING`，父唯一键包含 kind/state。probe 开始/结束可在一个事务内同时修改 run 与 attempt，约束只在提交时检查，完全由 SQLite 原生 CHECK/FK 实现。

`probe_authorizations` 是不可变的探测 scope 父记录。它只能在 `BLOCKED -> RUNNING/PROBING`、创建新 RUNNING DEPENDENCY_PROBE attempt 和 active pointer 的同一 `BEGIN IMMEDIATE` 事务中插入；版本化 trigger 同时核对 run/current active probe、checkpoint identity/policy/digest、lease/fencing 和无 CANCEL。历史记录不 FK 到之后会变化的 attempt state；probe 结束只关闭 attempt，不改写授权父记录。

### mutation claims

```text
mutation_budget_accounts(run_id, stage_scope_digest, limit_value,
                          claimed_value, settled_value, version,
                          PRIMARY KEY(run_id, stage_scope_digest))
mutation_budget_claims(claim_id PK, run_id, stage_scope_digest, source_batch_digest,
                       mutation_ordinal, limit_snapshot,
                       dimension CHECK(dimension=MAX_MUTATIONS_PER_STAGE),
                       state(CLAIMED|SETTLED|RELEASED), created_at, settled_at?,
                       UNIQUE(run_id, stage_scope_digest, mutation_ordinal, claim_id),
                       FOREIGN KEY(run_id, stage_scope_digest)
                         REFERENCES mutation_budget_accounts(run_id, stage_scope_digest))
idea_mutation_intents(mutation_id PK, run_id, request_digest, source_batch_digest,
                      stage_scope_digest,
                      parent_idea_id?, trigger_kind, trigger_evidence_digest,
                      mutation_ordinal, mutation_reason, mutation_budget_claim_id FK,
                      intent_digest, created_at,
                      UNIQUE(run_id, mutation_budget_claim_id),
                      UNIQUE(mutation_id, run_id, intent_digest),
                      FOREIGN KEY(run_id, stage_scope_digest, mutation_ordinal,
                                  mutation_budget_claim_id)
                        REFERENCES mutation_budget_claims(run_id, stage_scope_digest,
                                                          mutation_ordinal, claim_id),
                      UNIQUE(run_id, intent_digest))
idea_mutation_records(mutation_id PK, run_id, mutation_intent_digest,
                      logical_operation_ids_json, budget_reservation_ids_json,
                      budget_settlement_digest, output_batch_digest, created_at,
                      UNIQUE(mutation_id, run_id),
                      UNIQUE(mutation_id, run_id, mutation_intent_digest),
                      FOREIGN KEY(mutation_id, run_id, mutation_intent_digest)
                        REFERENCES idea_mutation_intents(mutation_id, run_id, intent_digest))
idea_mutation_record_operations(mutation_id, run_id, logical_operation_id, attempt_id,
                                PRIMARY KEY(mutation_id, run_id, logical_operation_id, attempt_id),
                                FOREIGN KEY(mutation_id, run_id)
                                  REFERENCES idea_mutation_records(mutation_id, run_id),
                                FOREIGN KEY(logical_operation_id, run_id, attempt_id)
                                  REFERENCES call_operations(logical_operation_id, run_id, attempt_id))
idea_mutation_record_reservations(mutation_id, run_id, logical_operation_id, attempt_id,
                                  reservation_id, call_id,
                                  PRIMARY KEY(mutation_id, run_id, reservation_id),
                                  FOREIGN KEY(mutation_id, run_id, logical_operation_id, attempt_id)
                                    REFERENCES idea_mutation_record_operations(mutation_id, run_id,
                                                                                logical_operation_id, attempt_id),
                                  FOREIGN KEY(reservation_id, call_id, run_id, attempt_id)
                                    REFERENCES budget_reservations(reservation_id, call_id, run_id, attempt_id))
```

`mutation_budget_accounts` 以稳定的 `stage_scope_digest` 表示整条变异链的阶段范围，而不是每个 source batch 各自计数。创建 claim 必须在 `BEGIN IMMEDIATE` 中锁定该 account，检查 `claimed_value + 1 <= limit_value`，递增计数并写入带 `limit_snapshot`/ordinal 的不可变 claim；并发 intent 不能超卖。claim 创建于任何 LLM dispatch 之前，但不冒充 `budget_reservations`，也不携带未来物理 call ID。`IdeaMutationRecord` 只在输出 batch 成功后写入，列出实际产生的 logical operation/reservation ID 以及结算快照 digest；提交前的版本化校验器/trigger 对 canonical JSON 数组与两个关系表逐项比对，且关系表通过组合 FK 保证同一 run、同一 operation 及其 reservation，避免跨 run 或无关调用串链。失败 intent 由其 AttemptCall/预算终态留证，claim 按实际消耗结算或释放，不生成 record。claim、intent、record 均不可原地重写，新的变异必须使用新的 ordinal。

### call_operations / attempt_calls / budget

以下为关系与约束的示意 DDL；实际 migration 按依赖顺序先建 `attempts`/`call_operations` 与 mutation account/intent，再建 `attempt_calls`、预算和制品表，最后建 mutation record/relationship 表并补充需要父表存在的组合 FK/trigger。

```text
call_operations(logical_operation_id PK, run_id, attempt_id,
                logical_ordinal, operation_kind, input_digest, logical_idempotency_key,
                terminal_state(OPEN|SUCCEEDED|FAILED|UNKNOWN),
                dispatch_kind?(DISPATCHED|CACHE_HIT|NO_DISPATCH),
                result_attempt_call_id?, cache_source_attempt_call_id?,
                cache_pin_call_id?, decision_source_call_id?,
                outcome_digest?, failure_code?, created_at, completed_at?,
                UNIQUE(run_id, attempt_id, logical_ordinal),
                UNIQUE(run_id, logical_idempotency_key),
                UNIQUE(logical_operation_id, run_id, attempt_id),
                FOREIGN KEY(run_id, attempt_id)
                  REFERENCES attempts(run_id, attempt_id),
                FOREIGN KEY(result_attempt_call_id, logical_operation_id)
                  REFERENCES attempt_calls(call_id, logical_operation_id)
                  DEFERRABLE INITIALLY DEFERRED,
                FOREIGN KEY(cache_pin_call_id, logical_operation_id)
                  REFERENCES attempt_calls(call_id, logical_operation_id)
                  DEFERRABLE INITIALLY DEFERRED,
                FOREIGN KEY(cache_source_attempt_call_id)
                  REFERENCES attempt_calls(call_id),
                FOREIGN KEY(decision_source_call_id)
                  REFERENCES attempt_calls(call_id))
attempt_calls(call_id PK, logical_operation_id, run_id, attempt_id,
              call_kind(LLM_HTTP|SIMILARITY_HTTP|DOCKER_ENGINE_PING|
                        DOCKER_CONTAINER_CREATE|LOCAL_ARTIFACT_WRITE|
                        CACHE_PIN|PROBE_CACHE_PIN),
              container_role?(IMPORT|KEEPER|TARGET|EXPORT), physical_ordinal,
              call_digest, capability_scope_digest, dispatch_idempotency_key, dispatch_state,
              owner_id, lease_epoch, authorized_mode(NORMAL|PROBING), probe_authorization_id?,
              created_at, updated_at,
              UNIQUE(call_id, run_id, attempt_id),
              UNIQUE(call_id, logical_operation_id),
              UNIQUE(call_id, logical_operation_id, call_kind, container_role),
              UNIQUE(call_id, run_id, attempt_id, lease_epoch),
              UNIQUE(call_id, logical_operation_id, run_id, attempt_id, lease_epoch),
              UNIQUE(call_id, call_kind),
              UNIQUE(logical_operation_id, physical_ordinal),
              UNIQUE(run_id, dispatch_idempotency_key),
              CHECK(call_kind<>CACHE_PIN OR authorized_mode=NORMAL),
              CHECK(call_kind<>PROBE_CACHE_PIN OR authorized_mode=PROBING),
              CHECK((authorized_mode=PROBING AND probe_authorization_id IS NOT NULL)
                    OR (authorized_mode=NORMAL AND probe_authorization_id IS NULL)),
              CHECK((call_kind=DOCKER_CONTAINER_CREATE AND container_role IS NOT NULL)
                    OR (call_kind<>DOCKER_CONTAINER_CREATE AND container_role IS NULL)),
              CHECK(
                (call_kind IN (CACHE_PIN,PROBE_CACHE_PIN)
                  AND dispatch_state=COMPLETED_NO_DISPATCH)
                OR
                (call_kind NOT IN (CACHE_PIN,PROBE_CACHE_PIN)
                  AND dispatch_state<>COMPLETED_NO_DISPATCH)
              ),
              FOREIGN KEY(logical_operation_id, run_id, attempt_id)
                REFERENCES call_operations(logical_operation_id, run_id, attempt_id),
              FOREIGN KEY(run_id, attempt_id, lease_epoch)
                REFERENCES attempts(run_id, attempt_id, lease_epoch),
              FOREIGN KEY(probe_authorization_id, run_id, attempt_id, lease_epoch)
                REFERENCES probe_authorizations(probe_authorization_id, run_id,
                                                probe_attempt_id, lease_epoch))
budget_accounts(run_id, dimension, limit_value, reserved_value, settled_value, version)
budget_reservations(reservation_id PK, call_id FK, run_id, attempt_id,
                    dimension, subkey, reserved, settled, state,
                    idempotency_key, created_at, updated_at,
                    UNIQUE(call_id, reservation_id),
                    UNIQUE(reservation_id, call_id, run_id, attempt_id),
                    UNIQUE(call_id, reservation_id, dimension, subkey),
                    UNIQUE(call_id, dimension, subkey),
                    UNIQUE(run_id, dimension, idempotency_key),
                    FOREIGN KEY(call_id, run_id, attempt_id)
                      REFERENCES attempt_calls(call_id, run_id, attempt_id))
artifact_declarations(declaration_id PK, call_id NOT NULL, reservation_id NOT NULL,
                      reservation_dimension NOT NULL CHECK(reservation_dimension=ARTIFACT_BYTES),
                      role NOT NULL, logical_path NOT NULL, media_type NOT NULL, declaration_digest NOT NULL,
                      UNIQUE(declaration_id, call_id, reservation_id),
                      UNIQUE(declaration_id, role, logical_path, media_type),
                      UNIQUE(call_id, declaration_digest),
                      FOREIGN KEY(call_id, reservation_id, reservation_dimension, declaration_digest)
                        REFERENCES budget_reservations(call_id, reservation_id, dimension, subkey))
artifact_writer_tokens(writer_token_id PK, call_id FK, reservation_id FK,
                       declaration_id,
                       reservation_dimension CHECK(reservation_dimension=ARTIFACT_BYTES),
                       reservation_subkey, declaration_digest,
                       state(PREPARED|OPEN|SEALED|FINALIZED|RELEASED),
                       final_digest?, final_size?, created_at, updated_at,
                       CHECK(reservation_subkey=declaration_digest),
                       CHECK((state IN (SEALED,FINALIZED) AND final_digest IS NOT NULL AND final_size IS NOT NULL)
                             OR (state IN (PREPARED,OPEN) AND final_digest IS NULL AND final_size IS NULL)
                             OR state=RELEASED),
                       UNIQUE(call_id, declaration_digest),
                       UNIQUE(call_id, reservation_id),
                       UNIQUE(writer_token_id, call_id, reservation_id),
                       UNIQUE(writer_token_id, call_id, reservation_id, final_digest, final_size),
                       UNIQUE(writer_token_id, declaration_id, call_id, reservation_id,
                              state, final_digest, final_size),
                       FOREIGN KEY(call_id, reservation_id, reservation_dimension, reservation_subkey)
                         REFERENCES budget_reservations(call_id, reservation_id, dimension, subkey),
                       FOREIGN KEY(declaration_id, call_id, reservation_id)
                         REFERENCES artifact_declarations(declaration_id, call_id, reservation_id))
```

`call_operations` 是公共 `CallTrace.LogicalOperationID` 的持久化父记录；一个业务层 Generate/Search/Compile/Run/Probe 只占一个 logical ordinal，但可因 HTTP retry 或 Docker import/keeper/target/export 产生多个按 `physical_ordinal` 排序的 `attempt_calls`。operation 先以 OPEN 插入，预算/熔断/策略预拒绝可直接把它终结为 `FAILED + NO_DISPATCH`，此路径不产生 AttemptCall；需要 dispatch 时才在后续守卫事务中创建物理 row。最终 Value/PortFailure、CallTrace 投影、outcome digest 与 terminal state 在一次短事务中完成；版本化 CHECK/trigger 执行与公共 CallTrace 相同的 DISPATCHED/CACHE_HIT/NO_DISPATCH 字段矩阵。`ResultAttemptCallID` 和 cache pin 必须属于本 operation；cache/decision source 可以来自历史 operation。terminal row 是恢复重建 CallTrace 的规范来源，禁止从“最后一条日志”猜结果。这样 provenance 不会把逻辑请求、物理尝试、失败和缓存挂载混成一个 ID。

logical idempotency key 用于重放同一业务调用；每个物理 row 另有由 logical key + physical ordinal 派生且可稳定重放的 dispatch key。只有传输边界明确未发生/已失败后才可增加 physical ordinal；状态 UNKNOWN 时必须以原 call/dispatch key 对账或保守结算，不能换 key 伪装为普通 retry。

`attempt_calls` 是一个 LLM/Similarity HTTP attempt、一次真实 Docker Engine ping/ContainerCreate、本地 artifact-write session 或非物理 cache-pin session 的唯一持久化主体；组合 FK 保证它与 logical operation、所有 reservation 都属于同一 run/attempt/producer lease epoch。所有 call-count、token/cost、artifact-byte reservation 和 writer token 都通过 FK 绑定它。`capability_scope_digest` 固定授权时的 run_version、execution_mode、current Step/input 或 blocked checkpoint dependency/probe policy。`dispatch_state` 为 `PENDING/AUTHORIZED/DISPATCHING/SENT/COMPLETED/UNKNOWN/ABORTED_NO_DISPATCH/COMPLETED_NO_DISPATCH`。物理调用与本地写 session 在同一个 `BEGIN IMMEDIATE` 事务原子创建/重放 call row、全部预算行与 writer tokens，并置 AUTHORIZED；执行者必须用 CAS 唯一 claim `AUTHORIZED -> DISPATCHING`，并重新核对 scope/fencing/context，失败不得发出物理动作。若 CANCEL/fence/context 在不可逆 dispatch 前失效，当前 owner 或恢复 owner在确认请求未发送/容器未创建后以 CAS 转 `ABORTED_NO_DISPATCH`，释放 reservations/tokens；是否发出不确定则必须 `UNKNOWN`。崩溃在 DISPATCHING/SENT 时按 UNKNOWN/Provider idempotency 恢复，禁止把各预算行单独当作 dispatch 权威。只有 `CACHE_PIN`（NORMAL）与 `PROBE_CACHE_PIN`（PROBING）可直接成为 `COMPLETED_NO_DISPATCH`，且永远不能被 dispatch claim。

逻辑 `CallOperation` 父行可在物理授权前独立幂等创建；因此没有 AttemptCall 的预算/熔断/策略预拒绝仍有可恢复的 terminal projection（`FAILED + NO_DISPATCH`）。上段“原子创建 call row”仅指需要真实 dispatch 的 AttemptCall 及其预算/token，不得回退为预算不足时零持久化。

同一 call/reservation/declaration/writer 唯一键的重复请求必须返回原记录；若 call digest、dimension、额度、attempt 或声明摘要不一致则为一致性错误。规范 `artifact_declarations` 固定 role/path/media type，并绑定唯一 ARTIFACT_BYTES reservation；Writer token 使用固定 shadow column `reservation_dimension=ARTIFACT_BYTES`，以组合 FK 绑定同 declaration/reservation。SQLite 会直接拒绝 writer 引用调用次数/费用 reservation、其他声明的字节预算或事后重标 role，不能只依赖应用检查。若 CANCEL 在线性化上晚于 AUTHORIZED，该 call 已属于“在途”，owner 仍应尽快用 context 取消并按实际/UNKNOWN 结算；一次 retry 必须使用新的 operation ordinal/call_id，不得重置或复用旧 call authorization。

Writer token 是一次性能力：`PREPARED -> OPEN` 以 CAS 领取，只有领取者可写临时文件；关闭时以 CAS `OPEN -> SEALED` 固定唯一 digest/size 并建立 pin，Blob 发布并完整校验后以 CAS `SEALED -> FINALIZED`。相同 token + digest + size 的 finalize 可幂等重放，不同 digest/size 是一致性错误；失败/未使用 token 转 `RELEASED`，终态不得再次打开。一个 token 最多对应一个 PendingArtifact 和一个 active/recovery pin。

### sandbox_executions / sandbox_resources

```text
sandbox_executions(
  logical_operation_id PK, run_id, attempt_id, producer_lease_epoch,
  protocol, operation_kind(COMPILE|RUN|PROBE), profile_digest,
  resource_plan_digest NOT NULL, engine_identity_digest NOT NULL,
  phase(CREATED|ARMED|TARGET_STARTED|STOPPING|TARGET_STOPPED|EXPORTING|ALL_STOPPED|CLEANUP_PENDING|CLEANED),
  result_call_id?, program_hard_deadline_utc?, watchdog_safety_deadline_utc?,
  watchdog_armed_duration_ns?, trigger_cause?,
  watchdog_token_digest, watchdog_control_ref, cleanup_error_digest?, created_at, updated_at,
  UNIQUE(logical_operation_id, resource_plan_digest, engine_identity_digest),
  FOREIGN KEY(logical_operation_id, run_id, attempt_id)
    REFERENCES call_operations(logical_operation_id, run_id, attempt_id),
  FOREIGN KEY(result_call_id, logical_operation_id, run_id, attempt_id, producer_lease_epoch)
    REFERENCES attempt_calls(call_id, logical_operation_id, run_id, attempt_id, lease_epoch)
)
sandbox_resources(
  logical_operation_id FK, resource_plan_digest NOT NULL, resource_ordinal, resource_kind(CONTAINER|VOLUME|CGROUP),
  resource_role(IMPORT|KEEPER|TARGET|EXPORT|INPUT|OUTPUT|RELEASE_PARENT),
  deterministic_name NOT NULL, expected_labels_digest NOT NULL, engine_identity_digest NOT NULL,
  cgroup_relative_path?, creation_nonce?, owner_lease_epoch?, engine_resource_id?,
  create_call_id?, create_call_kind?,
  state(PLANNED|CREATING|CREATED|ACKED|RUNNING|STOPPED|REMOVED|UNKNOWN),
  created_at, updated_at,
  PRIMARY KEY(logical_operation_id, resource_ordinal),
  UNIQUE(resource_kind, engine_resource_id),
  UNIQUE(engine_identity_digest, resource_kind, deterministic_name),
  FOREIGN KEY(logical_operation_id, resource_plan_digest, engine_identity_digest)
    REFERENCES sandbox_executions(logical_operation_id, resource_plan_digest, engine_identity_digest),
  FOREIGN KEY(create_call_id, logical_operation_id, create_call_kind, resource_role)
    REFERENCES attempt_calls(call_id, logical_operation_id, call_kind, container_role),
  CHECK((resource_kind=CONTAINER AND create_call_id IS NOT NULL
           AND create_call_kind=DOCKER_CONTAINER_CREATE
           AND resource_role IN (IMPORT,KEEPER,TARGET,EXPORT))
        OR (resource_kind<>CONTAINER AND create_call_id IS NULL
           AND create_call_kind IS NULL))
)
```

`expected_labels_digest` 为所有资源的非空规范字段；对 CGROUP 使用 profile 定义的零值，因为其真正身份由相对路径、creation nonce、owner epoch 和 inode/device 证据组成。

`SandboxExecution` 与完整 `sandbox_resources(PLANNED)` 必须在任何 Docker 资源创建前写入；execution、每个 resource、sealed authorization、watchdog control record 都固定同一 `resource_plan_digest` 与 `engine_identity_digest`，并拒绝替换/降级。每个资源的确定性 name、固定 labels/identity digest 和对应 call/role 都已知；Docker `CONTAINER/VOLUME` 由 Engine identity + name/labels 归属，`CGROUP` 另保存受限父树下的规范相对路径、creation nonce 和 owner epoch。watchdog 在初始 ACK 前取得该不可扩张计划、订阅 Engine events 并完成按 kind-specific identity 的基线扫描；Runner 每次 Create/mkdir 前再把对应 row CAS 为 CREATING 并取得 watchdog pre-create ACK，Create/mkdir 返回后才补 engine ID 或 inode/device 证据、CREATED/ACKED。这样 CLI 在请求中、返回后写 DB 前或 ACK 前崩溃，watchdog 仍能按 Docker name/labels 或固定 cgroup identity 发现并收敛迟到资源。每个 CONTAINER 资源绑定自己实际消费的 AttemptCall，volume/cgroup 创建不计 `max_sandbox_runs` 但仍持久化。数据库只保存 watchdog token digest 与内部 control-record 引用；原 token 位于 owner-only ACL 的本机记录，不进入 artifact/log/container。

新 recovery owner 的 `TAKEOVER` 只转移 Stop/Kill/Wait/Inspect/Remove 与账务收敛 custody，绝不签发旧 operation 的 ContainerCreate/Start/export capability；未完成物理计划的旧 attempt 一律清理后 ABANDONED，再以新 logical operation/epoch 重跑。若旧 operation 的所有物理 dispatch 和 PendingArtifact 已在崩溃前完整持久化，恢复可无 Docker 新动作地重放最终数据库提交。`CLEANUP_PENDING` 是执行控制状态，不是 Judge outcome；对应 run 必须保持 `RUNNING(mode=QUIESCING)`，直到不可信 target 安全停止并完成可安全延期的资源义务分类。

wall-time account 额外保存 `active_elapsed_ns`、`active_wall_definition_version` 和最后一次计量 heartbeat。计费区间必须同时满足 run 为 `RUNNING` 且有有效 execution/recovery lease；lease 已过期到新 lease 取得前即使数据库状态仍是 `RUNNING` 也不累计。状态/lease 边界与累计值更新在同一事务；崩溃时保守补计最后 heartbeat 到旧 lease expiry，新 owner 从 recovery lease 获取时重新开始，不能把新进程 monotonic 起点当作零，也不能把无 lease 的停机时间计入。

### review_decisions

保存 ADR-0002 的完整字段，并包含 `status(PENDING/APPLIED/REJECTED/STALE)`、`applied_at` 和 `applied_run_version`。`waiver_active` 不单独手改，由绑定 revision/evidence/policy 是否匹配计算。数据库约束保证每个 run 最多一个 active PENDING 决定。review CLI 只在 `NEEDS_REVIEW` 暂停态获取一个短期 execution lease，以 CAS 方式写入决定和事件，随后立即释放；真正的领域变更仍由下一次 `run resume` 在新的执行租约下应用。

### blobs / artifact_occurrences

```text
blobs(digest PK, size, state(STAGING|READY|DELETING|QUARANTINING|CORRUPT),
      delete_claim_id?, delete_claimed_at?,
      created_at, verified_at?, corruption_evidence_digest?,
      UNIQUE(digest, size))
blob_pin_identities(pin_id PK, writer_token_id UNIQUE, call_id, reservation_id,
                    run_id, attempt_id, producer_lease_epoch,
                    lifecycle_state(LIVE|RETIRED), created_at,
                    UNIQUE(pin_id, lifecycle_state, writer_token_id, call_id, reservation_id,
                           run_id, attempt_id, producer_lease_epoch),
                    FOREIGN KEY(writer_token_id, call_id, reservation_id)
                      REFERENCES artifact_writer_tokens(writer_token_id, call_id, reservation_id),
                    FOREIGN KEY(call_id, run_id, attempt_id, producer_lease_epoch)
                      REFERENCES attempt_calls(call_id, run_id, attempt_id, lease_epoch))
pending_blob_pins(pin_id PK, pin_lifecycle_state CHECK(pin_lifecycle_state=LIVE),
                  digest, size, run_id, attempt_id, call_id,
                  reservation_id, writer_token_id,
                  producer_lease_epoch, custodian_owner_id, custodian_lease_epoch,
                  state(ACTIVE|RECOVERING|RELEASABLE),
                  heartbeat_at, expires_at, created_at,
                  FOREIGN KEY(pin_id, pin_lifecycle_state, writer_token_id, call_id,
                              reservation_id, run_id, attempt_id, producer_lease_epoch)
                    REFERENCES blob_pin_identities(pin_id, lifecycle_state, writer_token_id,
                              call_id, reservation_id, run_id, attempt_id, producer_lease_epoch)
                    DEFERRABLE INITIALLY DEFERRED,
                  FOREIGN KEY(digest, size) REFERENCES blobs(digest, size),
                  FOREIGN KEY(call_id, run_id, attempt_id, producer_lease_epoch)
                    REFERENCES attempt_calls(call_id, run_id, attempt_id, lease_epoch),
                  FOREIGN KEY(writer_token_id, call_id, reservation_id, digest, size)
                    REFERENCES artifact_writer_tokens(writer_token_id, call_id, reservation_id,
                                                       final_digest, final_size))
artifact_occurrences(artifact_id PK, digest, size, run_id FK, step_name,
                     attempt_id FK, writer_token_id UNIQUE, declaration_id,
                     call_id, reservation_id,
                     producer_lease_epoch,
                     writer_token_state CHECK(writer_token_state=FINALIZED),
                       logical_path, role, media_type,
                      provenance_json, created_at,
                      UNIQUE(artifact_id, run_id, role),
                      UNIQUE(attempt_id, role, logical_path, digest),
                     FOREIGN KEY(digest, size) REFERENCES blobs(digest, size),
                     FOREIGN KEY(call_id, run_id, attempt_id, producer_lease_epoch)
                       REFERENCES attempt_calls(call_id, run_id, attempt_id, lease_epoch),
                     FOREIGN KEY(writer_token_id, declaration_id, call_id, reservation_id,
                                 writer_token_state, digest, size)
                       REFERENCES artifact_writer_tokens(writer_token_id, declaration_id,
                                                          call_id, reservation_id, state,
                                                          final_digest, final_size),
                     FOREIGN KEY(declaration_id, role, logical_path, media_type)
                       REFERENCES artifact_declarations(declaration_id, role,
                                                        logical_path, media_type))
```

上述 provenance/FK 子列在实际 migration 中全部为 `NOT NULL`，包括 declaration 的所有字段、所有组合 FK shadow columns 以及 `capability_observations` 的 PENDING producer；终态 observation 只有在明确表示本地无 producer 的规则下才能为空，物理 observation 必须同时绑定 logical operation 与 AttemptCall。这样避免 SQLite 因任一子列为 NULL 而跳过复合 FK。`producer_lease_epoch` 不可变；恢复只能更新 `custodian_*`，不能把旧调用伪装为新 epoch 产生。同一字节 Blob 可以对应多个 ArtifactOccurrence，但每个 occurrence 的 role/path/media type 必须通过 declaration 组合 FK 与 writer token 的原始声明一致，禁止把普通输出重标为 manifest/receipt/report。pin 的组合 FK 强制其 `(digest,size)` 等于 token 已封存的 `(final_digest,final_size)`；occurrence 进一步通过固定 `writer_token_state=FINALIZED` 的组合 FK，保证只能引用该 token 最终发布的同一 Blob。缓存命中也产生完整的 declaration/FINALIZED token/pin 链和 `PendingArtifact`，最终由 Orchestrator 创建新的 occurrence，以记录本次 run 的来源链。

### cache_entries

```text
cache_entries(cache_key PK, cache_kind, component_version, value_json,
              blob_digests_json, created_at, expires_at?, environment_digest?,
              UNIQUE(cache_key, cache_kind))
cache_entry_sources(cache_key PK, cache_kind, source_call_id, source_logical_operation_id,
                    UNIQUE(cache_key, cache_kind, source_call_id),
                    FOREIGN KEY(cache_key, cache_kind)
                      REFERENCES cache_entries(cache_key, cache_kind),
                    FOREIGN KEY(source_call_id, source_logical_operation_id)
                      REFERENCES attempt_calls(call_id, logical_operation_id))
cache_blob_refs(cache_key FK, digest, PRIMARY KEY(cache_key, digest))
```

`cache_blob_refs` 是完整性失效和 GC 的规范引用，`blob_digests_json` 只可作为序列化快照，不能成为唯一索引。

依赖能力缓存另有单调观察序列，避免用可回拨 wall clock 证明“故障后已恢复”：

```text
dependency_observation_counters(
  dependency_identity_digest, capability_policy_digest, last_seq,
  PRIMARY KEY(dependency_identity_digest, capability_policy_digest)
)
capability_observations(
  dependency_identity_digest, observation_seq, capability_policy_digest,
  status(PENDING|HEALTHY|UNAVAILABLE|INCOMPATIBLE|ABORTED|UNKNOWN),
  snapshot_digest?, producer_logical_operation_id?, producer_call_id?,
  authorized_at, observed_at?,
  PRIMARY KEY(dependency_identity_digest, capability_policy_digest, observation_seq),
  UNIQUE(dependency_identity_digest, capability_policy_digest, observation_seq,
         status, producer_call_id),
  CHECK((status=PENDING AND producer_logical_operation_id IS NOT NULL)
        OR (status<>PENDING AND
            ((producer_logical_operation_id IS NULL AND producer_call_id IS NULL)
             OR (producer_logical_operation_id IS NOT NULL AND producer_call_id IS NOT NULL)))),
  FOREIGN KEY(producer_logical_operation_id)
    REFERENCES call_operations(logical_operation_id),
  FOREIGN KEY(producer_call_id, producer_logical_operation_id)
    REFERENCES attempt_calls(call_id, logical_operation_id)
)
capability_cache_entries(
  cache_key PK, cache_kind CHECK(cache_kind=CAPABILITY), source_call_id,
  dependency_identity_digest,
  capability_policy_digest, observation_seq,
  observation_status CHECK(observation_status=HEALTHY),
  UNIQUE(cache_key, cache_kind, source_call_id, dependency_identity_digest,
         capability_policy_digest, observation_seq, observation_status),
  FOREIGN KEY(cache_key, cache_kind, source_call_id)
    REFERENCES cache_entry_sources(cache_key, cache_kind, source_call_id),
  FOREIGN KEY(dependency_identity_digest, capability_policy_digest,
              observation_seq, observation_status, source_call_id)
    REFERENCES capability_observations(dependency_identity_digest,
              capability_policy_digest, observation_seq, status, producer_call_id)
)
cache_pin_uses(
  pin_call_id PK, pin_call_kind CHECK(pin_call_kind IN (CACHE_PIN,PROBE_CACHE_PIN)),
  cache_key, cache_kind, source_call_id,
  dependency_identity_digest?, capability_policy_digest?,
  observation_seq?, observation_status?,
  FOREIGN KEY(pin_call_id, pin_call_kind)
    REFERENCES attempt_calls(call_id, call_kind),
  FOREIGN KEY(cache_key, cache_kind, source_call_id)
    REFERENCES cache_entry_sources(cache_key, cache_kind, source_call_id),
  FOREIGN KEY(cache_key, cache_kind, source_call_id, dependency_identity_digest,
              capability_policy_digest, observation_seq, observation_status)
    REFERENCES capability_cache_entries(cache_key, cache_kind, source_call_id,
              dependency_identity_digest, capability_policy_digest,
              observation_seq, observation_status),
  CHECK((cache_kind=CAPABILITY AND dependency_identity_digest IS NOT NULL
         AND capability_policy_digest IS NOT NULL AND observation_seq IS NOT NULL
         AND observation_status=HEALTHY)
        OR (cache_kind<>CAPABILITY AND dependency_identity_digest IS NULL
            AND capability_policy_digest IS NULL AND observation_seq IS NULL
            AND observation_status IS NULL)),
  CHECK(pin_call_kind<>PROBE_CACHE_PIN OR cache_kind=CAPABILITY)
)
```

每次物理 health/probe 调用在 dispatch authorization 事务中先为 identity + policy 原子递增 counter、分配不可变 observation ticket，并把 `PENDING` row 绑定 producer logical operation；单 HTTP/ping 同时绑定其预授权 call，Docker 多容器 probe 在结果事务填入同 operation 的决定性 result call。结果事务只 CAS 填写原 ticket，不能在响应到达后再取序号。造成 BLOCKED 的本地 capability failure 在同一短事务分配并结束无 producer operation 的 ticket。提交 BLOCKED 时读取当时 counter 的 `last_seq`，把 `min_observation_seq=last_seq+1` 与 blocker 一起提交；因此所有阻塞前已授权但迟到提交的健康结果都低于 floor。Capability cache value 必须通过规范表引用 HEALTHY observation 与同一 source call。NORMAL cache hit 只接受仍在 TTL 内、policy 匹配、状态 HEALTHY 且 sequence 等于该 identity + policy 当前 `last_seq` 的 snapshot；同 policy 更新的 PENDING/UNAVAILABLE/INCOMPATIBLE/UNKNOWN 会使更早 healthy cache 不可命中，但不同 capability policy 的观察互不污染。`authorized_at/observed_at` 只用于 TTL/审计，不能替代序列 floor。

cache hit 不能直接裸返回 BlobRef。`PinExisting` 必须先在同一个 `BEGIN IMMEDIATE` 中检查当前 fencing/状态/CANCEL、cache 未过期、规范 `cache_blob_refs/cache_entry_sources`、Blob 为 `READY` 和预算，并幂等创建 `call_kind=CACHE_PIN, dispatch_state=COMPLETED_NO_DISPATCH` 的 AttemptCall、`cache_pin_uses`、零物理新增字节 artifact reservation、原声明 role 的 declaration/`FINALIZED` writer token 与 ACTIVE pin；提交后才以该 pin 调用 `OpenVerified` 并返回 PendingArtifact。CACHE_PIN 不创建 call-count/cost reservation，也不能被 dispatch，所以 cache hit 不扣 LLM/Similarity/Sandbox 调用次数，但其 cache/source、预算、fencing、CANCEL、pin 和 occurrence 仍有完整父链。cache 的并发过期不撤销已经提交的 pin；校验失败则使 cache/Blob 失效并收敛失败 reservation/pin，不创建 occurrence，FINALIZED token 只保留为该失败 cache-pin session 的预期 digest 审计记录，不能再消费。

PROBING 只能使用独立 `PROBE_CACHE_PIN`：创建短事务必须通过不可变 `probe_authorizations` 核对 current probe/checkpoint/fencing/CANCEL，并让 `cache_pin_uses` 组合 FK 绑定同 cache key/source call、同 identity/policy、状态 HEALTHY 且 `observation_seq >= min_observation_seq` 的 capability cache row。它不扣外部 call-count，但创建自己的 AttemptCall、declaration、零 physical-new artifact reservation、FINALIZED token 和 ACTIVE pin；普通 CACHE_PIN 在 PROBING 必须被 Schema/guard 拒绝。若没有足够新的健康观察，必须物理 probe，不能用阻塞前的旧快照证明依赖已恢复。

### packages / package_occurrences

```text
packages(package_id PK, manifest_digest, created_at)

package_occurrences(package_occurrence_id PK, package_id FK, run_id FK,
                    relation(GENERATED|VERIFICATION|DERIVED), package_revision,
                    status(IMPORTED|VERIFIED),
                    verification_profile, environment_digest,
                     manifest_artifact_id, manifest_role CHECK(manifest_role=PACKAGE_MANIFEST),
                     receipt_artifact_id?, receipt_role?,
                     final_quality_artifact_id?, final_quality_role?,
                     location_kind(PUBLISHED|OBSERVED), safe_path,
                     created_at, UNIQUE(run_id, package_revision),
                     UNIQUE(run_id, package_occurrence_id, status),
                     FOREIGN KEY(run_id, relation) REFERENCES runs(run_id, relation),
                     FOREIGN KEY(manifest_artifact_id, run_id, manifest_role)
                       REFERENCES artifact_occurrences(artifact_id, run_id, role),
                     FOREIGN KEY(receipt_artifact_id, run_id, receipt_role)
                       REFERENCES artifact_occurrences(artifact_id, run_id, role),
                     FOREIGN KEY(final_quality_artifact_id, run_id, final_quality_role)
                       REFERENCES artifact_occurrences(artifact_id, run_id, role),
                     CHECK(
                       (status=IMPORTED AND receipt_artifact_id IS NULL
                         AND receipt_role IS NULL
                         AND final_quality_artifact_id IS NULL
                         AND final_quality_role IS NULL)
                       OR
                       (status=VERIFIED AND receipt_artifact_id IS NOT NULL
                         AND receipt_role=PACKAGE_VERIFICATION_RECEIPT
                         AND final_quality_artifact_id IS NOT NULL
                         AND final_quality_role=FINAL_QUALITY_REPORT)
                     ))
```

`packages` 保存内容身份，同一不可变 package ID 可以被多个独立 verification run 引用。`package_occurrences` 保存某个 run 对该包的生成、派生或只读验证关系；其 `relation` 必须与 `runs.relation` 相同。外部 verify 在 StructuralGate/import 事务后先写 `IMPORTED`，此时 receipt/final quality 必须为空。完整门禁通过后同一 fencing 事务用一条 UPDATE 填入 receipt/最终质量并转为 `VERIFIED`。`runs` 的同行 READY CHECK 与延迟组合 FK 共同保证：READY 必须且只能引用同 run 的 VERIFIED occurrence；READY 的 state/execution_mode/pointer/status shadow 必须在同一条 UPDATE 中修改，因为 CHECK 不可延迟。

## 4. Blob 写入协议

所有 run 产物由 `MeteredArtifactSink` 执行以下协议；内容寻址 backend 的低层 put 仅在 Sink 实现包内私有可见，adapter、Package/Judge/Sandbox 等其他正式组件都不能取得。GC/migration/integrity scanner 使用不含 run 写入能力的窄维护接口，不得创建 run provenance：

1. 在对应物理调用 dispatch authorization 事务中校验全部声明的 role/media type/逻辑大小上限并创建 artifact-byte reservations 与组合 FK writer tokens；临时文件只能在 token CAS `PREPARED -> OPEN` 成功后建立在 artifact root 内，不能使用模型给出的绝对路径。独立生成的本地制品也必须在开始写入前经同样的无 CANCEL 守卫事务 Prepare。
2. token owner 流式写入并同时计算 SHA-256、字节数和输出限制；其他并发 open/finalize 被 CAS 拒绝。
3. `fsync` 临时文件并得到 digest/size；取得每 digest 的内部发布锁。在小事务中验证 token/call/reservation/declaration/attempt/lease 组合绑定，CAS `OPEN -> SEALED` 固定 digest/size，建立/复用 `blobs(state=STAGING)` 和唯一 writer-token ACTIVE `pending_blob_pin`。该动作是已有资源收敛，不创建 occurrence/current output。
4. 若目标路径已存在，必须从已打开句柄流式重算完整 SHA-256 并核对 size/digest，不能只比较大小；匹配才丢弃临时文件并记录 `physical_new_bytes=0`。不存在时使用不可覆盖的原子原语（如 `renameat2(RENAME_NOREPLACE)` 或平台等价实现）发布，禁止普通 overwrite rename，并 flush 文件/父目录。
5. 发布/验证成功后将 Blob 标为 `READY`，CAS `SEALED -> FINALIZED`；返回带 call/writer-token/pin/reservation ID/measurement 的 `PendingArtifact`。相同 digest/size 的 finalize 可重放，不同内容失败关闭。ArtifactOccurrence 和 reservation `SETTLED/RELEASED` 在步骤成功、失败或 stale-result 事务中建立/更新，并把 pin 转为 `RELEASABLE`。写入中途失败或取消时关闭 writer、移除随机临时文件并收敛 token/pin/reservation；崩溃导致未知则由 recovery 接管。

若已有目标的实际 digest 不匹配路径声明，立即返回 `artifact_integrity_error`：在 digest 锁和事务中先把 Blob CAS 为 `QUARANTINING` 并使 `cache_blob_refs` 关联 cache 失效，使新 PinExisting/dedup/OpenVerified 全部失败关闭；再将损坏字节移动到 artifact root 内固定 quarantine（或在无法安全移动时原地封锁），保存本地证据并转为 `CORRUPT`。崩溃时 reconciliation 从 QUARANTINING 继续；不能用刚生成的临时文件静默掩盖损坏，显式 repair/reconciliation 验证后才可恢复该 digest。

所有读取使用 `OpenVerified(BlobRef)` 或等价流式复制接口：从不跟随链接的句柄核对普通文件、size 和完整 SHA-256，再把同一受控句柄/已验证副本交给调用方。至少 Sandbox 输入/可执行文件、cache 恢复、Package 构建/导入和 provenance 导出必须重新验证；发现缺失/损坏按上述 CORRUPT 流程失败关闭。

Blob 路径：`artifacts/sha256/<first-two>/<full-digest>`。逻辑文件名只存在 occurrence/package manifest 中。

## 5. 原子提交顺序

### 开始 attempt

单事务：

1. `SELECT run_version` 并检查状态。
2. 检查 lease owner/epoch 未被 fence。
3. 创建 attempt `RUNNING`，记录 lease_epoch。
4. 追加 `AttemptStarted` event。
5. 更新 step、run_version 和 runs.updated_at。

上述普通路径用于 `attempt_kind=STEP`。DEPENDENCY_PROBE 的开始必须与 `BLOCKED -> RUNNING(mode=PROBING)`、`active_probe_attempt_id` 和 wall-time 起点在同一事务完成，不能先切状态再调用本节另一个事务；事务前崩溃保持完整 BLOCKED，事务后崩溃必有可恢复 probe attempt。

### 完成 attempt

Blob 已在事务外发布为 `READY` 且由 ACTIVE pin 保护后，单事务：

1. 以 owner/lease_epoch fencing 和 expected run_version/CAS 检查 current input digest，并检查没有 active CANCEL；取消清理事务使用独立分支。
2. 验证每个 PendingArtifact 的 Blob/ACTIVE pin/FINALIZED writer token/artifact reservation/AttemptCall/attempt/lease 完整绑定及唯一 digest/size，写 ArtifactOccurrence。
3. 结算 reservation，并把对应 pin 转为 `RELEASABLE`。
4. 更新 attempt/step 状态和 current output digest。
5. 追加领域 event。
6. 更新 run snapshot、run state 和 run_version。

STEP attempt 才能写 current step output/成功状态。DEPENDENCY_PROBE completion 可提交 probe evidence/occurrence，但 `PROBE_HEALTHY` 只把原 step 置回 `PENDING/RETRYING` 并切回 NORMAL，`BLOCKED` 则回到暂停态；两者都清空 active probe，不能把探测误记为原 Step 成功。

CAS 失败时不能把输出直接接到 current snapshot：若仅因并行 sibling 推进 run_version 且该 Step 的 current input digest 未变，重新读取后可用同一 attempt/result digest 幂等重试提交；若输入已失效，则在当前 fencing token 下走 stale-result 事务，结算 reservation、把 Attempt 标为 `CANCELLED(cause=revision_invalidated)`，并选择性记录不进入 current snapshot 的诊断 occurrence。无法取得有效 fencing token 时由新 owner 恢复结算；Blob 可保持无引用并随后由 GC 回收。任何路径都不能漏结算或重复结算。

## 6. 重复提交与幂等

- 外部调用、attempt 提交和 reservation 均有稳定 idempotency key。
- 重复提交同 attempt + result digest 返回原提交结果。
- 同 attempt 提交不同 result digest 是一致性错误，任务进入 `FAILED` 并保留两份证据。
- `ArtifactOccurrence` 使用 `(attempt_id, role, logical_path, digest)` 唯一键避免重复引用。

## 7. 崩溃恢复

恢复禁止跨 Docker、网络、文件哈希或 `OpenVerified` 持有 SQLite 写事务。使用持久化 intent 的三阶段协议：

```text
recovery_intents(
  recovery_id PK, run_id FK, owner_id, lease_epoch,
  old_owner_id?, old_lease_epoch, expected_run_version, scope_digest,
  state(CLAIMED|RECONCILING|READY_TO_COMMIT|COMPLETED|STALE),
  created_at, updated_at
)
recovery_items(
  recovery_id FK, item_kind, item_id, observed_state,
  evidence_digest?, resolution?, updated_at,
  PRIMARY KEY(recovery_id, item_kind, item_id)
)
```

使用 partial unique index 保证每个 run 最多一个 `CLAIMED/RECONCILING/READY_TO_COMMIT` intent；历史 intent 不 FK 到可变的 current lease 行，每次写入都在短事务中重验 lease。

1. **Claim 短事务**：`BEGIN IMMEDIATE` 检查旧 lease 已过期，或存在已提交的 `OwnerYieldedForCleanup` 且 lease owner 为空；随后取得更高 epoch并确认 run 仍可恢复。新 epoch 先以 CAS 把该 run 所有较旧 epoch 的 `CLAIMED/RECONCILING/READY_TO_COMMIT` intent 置 `STALE(cause=superseded_recovery)`，再插入新的 CLAIMED intent，二者与 partial-unique guard 在同一事务完成；因此 recovery owner 连续崩溃不会永久卡住后继接管。若无 active CANCEL，插入普通 intent；若有 active CANCEL，则原子切入/保持 `RUNNING(mode=QUIESCING)` 并创建只允许停止、结算和清理旧资源的 cancel-recovery intent，禁止任何新 dispatch。原 run 若为 PROBING，cancel claim 在同一事务清空 active-probe pointer，但保留旧 probe 为 RUNNING，待无锁 reconcile 后再安全标记 CANCELLED；因此既满足 runs CHECK/FK，也不在容器停止前伪造完成。两条路径都把旧 ACTIVE pin 转为 RECOVERING并更新 `custodian_*`，写 `RecoveryStarted` 后立即提交。
2. **无锁 Reconcile**：事务外启动 Docker label/event reconciliation、收敛 `SandboxExecution`、核验 transport/container evidence、调用 `OpenVerified`。每个观察结果只用独立短事务写 `recovery_items`，绝不等待外部 I/O 时持有数据库写锁。
3. **Commit 短事务**：重新验证 owner/epoch、intent、expected run_version、scope 与 CANCEL，核对 recovery_items 后收敛 AttemptCall/reservation/token/pin，重放完成结果或 abandon attempt，更新 step/run/event，并把 intent 置 COMPLETED；fencing/version 已变则置 STALE，不应用旧证据。

active DEPENDENCY_PROBE 的恢复必须在同一个延迟 FK 事务中完成“插入新 RUNNING probe ordinal → active pointer 切到新 probe → 旧 probe ABANDONED”，或原子退出 PROBING、清空 pointer 并把旧 probe 收敛为 ABANDONED/CANCELLED。禁止单独提交旧 probe ABANDONED，因为提交时 active-probe FK 必然失效。

旧进程即使稍后恢复，也因 lease_epoch 过期无法提交；它在下一次 heartbeat/调用前检查时必须取消自身。

若 Package attempt 已把目录原子 rename 到正式路径、但提交 `package identity + occurrence + READY` 前崩溃，恢复流程把该目录视为待认领发布结果：重新执行 Package Gate，核对 package ID、run/revision/profile 和当前 PrePackage 证据后，再幂等完成数据库事务。不匹配的目录不得覆盖或接入 run，记录冲突并进入 `FAILED`；清理交给显式 reconciliation，不在恢复路径中递归删除未知目录。

## 8. 孤儿 Blob 回收

pin 的一次性身份永久存在于不引用 Blob 的 `blob_pin_identities`。live pin 只存在于 `pending_blob_pins`；历史表定义为 `blob_pin_history(pin_id PK, pin_lifecycle_state CHECK=RETIRED, digest,size,run_id,attempt_id,call_id,reservation_id,writer_token_id,producer_lease_epoch,final_state=RELEASABLE,release_reason,created_at,retired_at)`，并以与 live 表相同的 `DEFERRABLE INITIALLY DEFERRED` 完整组合 FK 指向 identity 的 RETIRED 状态，但不 FK 到 `blobs`。

- Blob 进入 STAGING 前建立持久化 pin，不能只依靠固定 grace period 猜测活跃写入。
- GC 排除任何 `ACTIVE/RECOVERING` pin；pin 即使超过 `expires_at`，也只能在 owning lease 过期且 recovery/reconciliation 已将对应 attempt/reservation 收敛后转为 `RELEASABLE`，不能由 GC 自行判死。
- 新 owner 接管旧 pin 时先设 `RECOVERING`、验证 Blob 和 reservation，再提交 occurrence/结算或转 `RELEASABLE`；长时间 import/多制品 Step 的 heartbeat 必须续期全部 pin。
- GC 只考虑无 ArtifactOccurrence、无 `cache_blob_refs`、无 package file 引用、无非终态 writer/reservation 且所有 pin 均为 `RELEASABLE` 的 READY/STAGING Blob；CORRUPT/quarantine 使用独立保留策略，不能被普通 GC 静默删除证据。
- 删除采用两阶段 claim：在 digest 锁与 `BEGIN IMMEDIATE` 中重新确认 occurrence/cache/package 引用为零、不存在 ACTIVE/RECOVERING pin、writer/reservation 均为终态，CAS Blob 为 `DELETING` 并写唯一 `delete_claim_id`；同一延迟 FK 事务把每个 identity 从 LIVE 更新为 RETIRED、插入对应 `blob_pin_history`、核对迁移数量后删除 live pin 行，再提交。RETIRED identity 使相同 pin ID/writer token 永远不能重新插回 live 表。`PinExisting`、dedup publish、OpenVerified 和新引用事务必须拒绝/等待 DELETING，不能在 claim 后重新挂载。
- claim owner 随后用不跟随链接的固定路径操作，将 canonical Blob 原子移动到 artifact root 内固定 trash 并 flush 父目录，再在核对 claim ID 的事务中删除 Blob 行；最后清除 trash。若 rename/unlink 失败，保持 DELETING 供 reconciliation 重试，不能先删除数据库行。
- 启动 reconciliation 处理 DELETING：canonical 文件尚在则继续移动，已在 trash 则完成数据库删除，两个位置都不存在则在核对 claim/零引用后删除残留行。相同 digest 的新 writer 必须等旧 claim 完成并重新创建 STAGING，不能覆盖或复用 DELETING 字节。
- GC 不跟随符号链接，artifact root 本身必须是固定、已解析目录。

## 9. 缓存规范

### 通用规范化

- JSON 使用稳定字段排序、UTF-8、无无意义空白和明确 Schema version。
- 文本换行统一为 LF；不得对源代码做会改变语义的 trim。
- cache key 包含 component/version、normalized input digest 和所有影响输出的配置。

### LLM

包含 provider、model、prompt digest、Schema version、sampling、tool configuration。缓存 value 保存原 usage/provenance。

### Compile

包含规范化 `SourceBundleManifest` digest、按安全路径排序的全部 `(path, Blob digest)`、EntryPoint、language、compiler/toolchain manifest digest、flags、platform/arch 和 builder image digest；不能只用单个 source Blob 代表多文件构建。

### Run

默认关闭。仅角色白名单且通过 Determinism Gate 后启用；包含 input、executable、declared seed、runtime image、platform/arch 和 Runner profile digest。Resource Gate 不读取普通 run cache 的性能数据。

### Similarity

- Evidence cache：query digest、service identity、protocol、effective options、model/index version。
- Decision cache：evidence digest、policy version。

无服务版本时 Evidence cache 必须短 TTL。

## 10. 测试

- 在事务各步骤注入崩溃，验证 event/snapshot/预算不会半提交。
- 并发两个 writer，验证 run_version CAS。
- 两个 CLI 同时 generate/resume，验证只有 lease owner 能调度；lease 被接管后旧 owner 全部提交被 fencing 拒绝。
- lease 反复获取/释放时 epoch 永不复用；持有历史 owner/epoch 的提交在任意后续周期均失败。
- 活跃 owner 存在时，第二个 CLI 只能写 CANCEL control request；owner 正常确认和 owner 失联后接管两条路径都能清理容器并得到唯一 `CANCELLED` 事件。
- 让 CANCEL 插入与 attempt success、READY、FAILED 事务在所有关键交错点竞争，断言提交顺序唯一决定结果，永不出现 accepted CANCEL 后的 READY/FAILED。
- review CLI 在暂停态用短 lease 创建决定，不能与 resume 同时应用；重复请求保持幂等。
- 重放相同 attempt，验证幂等。
- 写 Blob 后事务失败，验证 GC 回收。
- 长时间 import/多制品 Step 与 GC 并发时 ACTIVE pin 防止删除；崩溃后新 owner 收敛 pin，只有 RELEASABLE 才能回收。
- 同 digest 同 size 的磁盘篡改会被完整 hash 检出、隔离并使关联 cache 失效；Sandbox/cache/Package 均不能读取损坏字节。
- 并发发布同 digest 只有一个不可覆盖原子发布成功，其他 writer 完整验 hash 后 dedup。
- GC claim 与 PinExisting/dedup writer 并发时只有“先创建引用”或“先进入 DELETING”之一提交；DELETING 期间不能读取/复用，canonical→trash→DB row→trash cleanup 各崩溃点均可由 reconciliation 收敛。
- 跨 call 绑定 artifact reservation、同 token 并发双开、同 token 不同 digest 重复 finalize 均被数据库约束/CAS 拒绝；同 token 同 digest finalize 可幂等重放且只结算一次。
- CACHE_PIN 同事务拥有 AttemptCall/reservation/FINALIZED token/pin 完整父链，不扣外部调用次数，也不能产生孤立 reservation 或被 dispatch。
- 同 digest 多 occurrence，验证 provenance 不互相覆盖。
- `mvp` 性能结果不能被 `release` 任务复用。
- 在题包 rename 后、READY 事务前注入崩溃，恢复只能认领完全匹配且重新通过 Package Gate 的目录。
