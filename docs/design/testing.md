# 测试与验收设计

## 1. 测试层

| 层 | 目标 | 外部依赖 |
|---|---|---|
| Unit | 领域状态、Policy、cache key、path、exit mapping | 无 |
| Contract | LLM/Similarity/Docker direct-run/Exporter Schema | Fake/fixture |
| Integration | SQLite、Blob、DockerSandbox、Judge | 本机 Docker |
| End-to-end | 固定题目到 READY package | Fake LLM + Fake Similarity + Docker |
| Smoke | 真实 LLM/Similarity 的最小兼容性 | 显式手动触发 |
| Adversarial | 恶意源码、路径、输出和 checker 行为 | 隔离 Docker/专用环境 |

默认 CI 不调用真实付费模型或公共查重服务。

## 2. Slice 0 固定夹具

固定题目建议为“输入两个整数，输出其和”，包含：

- reference.cpp：正确实现。
- brute.cpp：正确但独立实现。
- validator.cpp：testlib 范围验证。
- checker.cpp：固定 token/integer checker。
- generator：固定 seed 的边界和随机数据。

必须覆盖：

| 场景 | 期望 |
|---|---|
| 合法输入 | `VALID` |
| 非法输入 | `INVALID` |
| 正确输出 | `AC` |
| 错误输出 | `WA` |
| 多余/污染输出 | 固定 `WA` 或 `PE/DIRT` |
| validator 未知退出码 | `VALIDATOR_ERROR` |
| checker FAIL/未知退出码 | `CHECKER_ERROR` |
| solution exit non-zero | `RE` |
| solution signal | `RE`；有权威 runtime evidence 时附 signal，否则保留原始 exit code |
| `program_hard_deadline` | `TLE` |
| Docker 不可用 | 分类后的 blocked probe outcome；Slice 0 不创建 run |
| 内存超过题目限制但低于旧 headroom | `MLE` |
| fork 后超时 | 整个目标容器/cgroup 无遗留进程 |
| network/Docker socket/宿主 mount/环境秘密 | 目标程序不可访问 |
| 伪造执行记录 | 不影响宿主 Runner 生成的只读证据 |
| symlink/hardlink/FIFO 输出 | 提升拒绝 |
| 路径穿越/超大输出 | 提升拒绝或 `OLE` |

Slice 0 使用合成但 Schema 合法的 `VerifiedProblem` 与 `PrePackageQualityReport`，只运行可复用的 `PackageStructuralGate`（manifest/path/hash/reverse-read）。它不创建 SQLite run、PackageVerificationReceipt 或正式 `READY` 状态；完整 Package Gate/READY 事务在 Slice 5 验收。

Slice 0 通过测试专用 `Slice0ProbeHarness` 注入内存 dispatch ledger/artifact sink；正式 CLI 无法构造该 capability。它运行第 6 节无需 SQLite recovery 的 `docker-direct-v2` 能力 canary，并以测试 owner 子进程 + test-scoped durable control record 验证 watchdog 对完整预告计划和 deadline Stop/Kill 的独立性。持久化 SandboxExecution、startup janitor、连续 recovery 和 cleanup TAKEOVER 集成验收属于 Slice 1。任一核心隔离/配额/watchdog canary 失败时停止后续 Slice，不推迟到 Slice 3。

## 3. Workflow/状态测试

- 每个 AgentResult 到 StepState/RunState 的 table-driven test。
- 每个正常 AgentResult 都得到 `AttemptState=FINISHED` 和正确 result_kind；Attempt 生命周期不与 Step 成败混淆。
- `BLOCKED` resume 先进入 `RUNNING(mode=PROBING)`；Similarity/Docker 探测分别按规则计量，健康后恢复原 step，失败收敛后回到 BLOCKED，探测期间 active wall 与 CANCEL 均生效。
- 普通 Step 的 `StepServices` 无法取得 `MeteredDependencyProber`；只有 PROBING Orchestrator 可调用它，且 checkpoint kind/identity/policy 不匹配时在 dispatch 前失败。
- LLM/Similarity/Docker 每个真实 probe/retry 都有新的 AttemptCall 并按 policy 扣减对应预算；`doctor` 结果不能充当 run-scoped probe evidence 或推进 BLOCKED checkpoint。
- stale NORMAL scheduler 在 PROBING、普通 Step/cache pin 在 PROBING、任何新授权在 QUIESCING 均被 mode-capability/CAS 拒绝；只有 digest 匹配 blocked checkpoint 的 probe call 可授权。
- BLOCKED→PROBING 与新 DEPENDENCY_PROBE attempt/active_probe_attempt_id 原子提交；转移前后故障注入分别得到完整 BLOCKED 或可恢复的 PROBING+probe attempt，不存在无父 AttemptCall，旧 FINISHED blocked attempt 永不追加调用。
- `active_probe_attempt_id` 绑定其他 run、普通 STEP kind 或非 RUNNING probe 时，被 runs 同行 CHECK + `DEFERRABLE INITIALLY DEFERRED` 组合 FK 在提交时拒绝；测试直接运行真实 SQLite migration。
- PROBING AttemptCall 必须组合 FK 到同事务创建并由 trigger 校验的不可变 probe_authorization；伪造普通/旧 attempt、其他 checkpoint identity/policy 或缺少父记录均被拒绝。
- 崩溃后的 `RUNNING -> ABANDONED`。
- cancel、`step_deadline/run_budget_deadline` 和程序 TLE 区分。
- user cancel、并行 quiesce、revision invalidation 与 lease lost 的 context cause 分别映射到正确 Attempt/Step/Run 状态。
- `user_cancel/quiesce/revision_invalidated/lease_lost/step_deadline/run_budget_deadline` 分别与程序 TLE/OOM/正常退出竞态时都先返回对应 ExecutionInterrupted cause；只有 user_cancel 产生 Run CANCELLED，run budget 进入 NEEDS_REVIEW，只有程序硬 deadline 才产生 TLE。
- revision 变化触发正确下游失效。
- waiver 的 gate/policy/evidence/revision 任一变化即失效。
- Package Gate 失败时永不 READY。
- 两个 CLI 同时 resume 只有一个取得 lease；接管后旧 owner 的 attempt、budget、artifact 和 package 提交全部被 fencing 拒绝。
- lease 释放后保留单调 epoch，重复获取不发生 token ABA；模拟溢出时失败关闭。
- 并行分支 BLOCKED 时先 quiesce；进入 BLOCKED 后没有旧 sibling 的普通提交。
- 活跃 owner 与失联 owner 两种情况下，跨 CLI CANCEL 都只产生一次终态并完成旧容器清理。
- CANCEL 与普通 attempt、READY、FAILED 的事务交错测试证明：先提交的取消或终态唯一获胜，不存在 accepted CANCEL 后的 READY/FAILED。
- CANCEL 与 review/budget-patch 竞争时，取消先提交会拒绝新决定并使旧 PENDING 决定 STALE；review 先提交后仍可由随后取消终止 run。
- run 创建事务后立即崩溃时，`CREATED` 可由 resume 安全启动且不重复创建 run。
- verify import 在 CREATED 状态不能预留/写 Blob；`CREATED -> RUNNING` 与配置复核提交后才允许 MeteredArtifactSink。

状态机测试生成所有合法转移，并断言未列出的转移被拒绝。

### 3.1 Request / Idea / Statement 契约

- manual request 四类意图字段至少一项非空；random 可全空，但 forbidden/resource/budget 仍生效。未知/重复归一化 tag、required/forbidden 冲突、越界 brief/limit 均在 run 创建前拒绝。
- 缺省 seed 只在创建事务生成一次 effective seed；崩溃重放、resume、mutation 和 TestPlan 不再次读取系统随机源。
- run 行内不可变保存 GenerationRequestSnapshot 与去密 effective config；全局配置/endpoint 改变或 Blob GC 后 resume 仍使用创建时 snapshot，digest 不匹配时失败关闭。
- IdeaBatch 候选数使用版本化范围；candidate ID、确定性排序、feasibility reason、selection reason/evidence 和 mutation parent/ordinal 都由严格 Schema 覆盖。
- 相同 request/effective seed/prompt-policy fixture 得到相同候选排序与 selection；变更 policy/prompt/schema 正确改变 digest/cache key。
- mutation 在 dispatch 前必须写 Intent/预算 reservation，成功后才写 Record/settlement/output batch；无可行候选与 Similarity 触发使用不同 typed evidence。用旧 selection 选择新 candidate、失败 intent 伪造 output、缺 settlement/trigger digest 或 batch/request digest 不匹配均拒绝。
- StatementInput 的 request snapshot/batch/selection/selected ID 形成完整 digest 链；ProblemSpec revision 与 request 语言/资源/required-forbidden、selected idea 和 mutation lineage 双向绑定。Statement 修订后下游证据按依赖图失效，不能继续引用旧样例或约束。

## 4. 预算并发测试

- 两个并行 Step 争夺最后一次 LLM call，只能一个 reservation 成功。
- CANCEL 与 reserve/dispatch authorization 在 `BEGIN IMMEDIATE` 中竞争：取消先提交时物理请求数/ContainerCreate 数为零；authorization 先提交时最多存在该一次在途调用并被取消/结算。
- AUTHORIZED 后在 dispatch claim 前、claim 后但 HTTP send/ContainerCreate 前发生 CANCEL/fence/context 失效时，确认未发出的 call 只能到 ABORTED_NO_DISPATCH 并释放全部 token/reservation；边界不确定必须 UNKNOWN，非 CACHE_PIN 不能使用 COMPLETED_NO_DISPATCH。
- raw response/stdout/stderr/声明文件的 reservation/writer authorization 与物理调用同事务预留；CANCEL 后 adapter 只能收敛这些既有 writer，尝试新增输出 reservation 被拒绝。
- 同一逻辑调用连续 429/5xx 重试时，每个真实请求都受独立 call reservation 限制，额度耗尽后不再发送下一次。
- Similarity 物理请求受独立 `max_similarity_calls` 账户限制，health/search/retry 的计费分类有固定测试。
- 预估费用高于余额时调用不会发出。
- effective model/usage category 缺定价时请求数为零；PricingPolicy digest、整数向上取整、溢出和 cached-input 上界有固定向量。
- 实际 usage 小于预留时正确释放差额。
- 429/明确未发送与连接后状态未知使用不同退款策略。
- 崩溃后 UNKNOWN reservation 保守结算。
- `(run_id, dimension, idempotency_key)` 重放返回同一 reservation；同 key 不同调用摘要失败且不会重复预留/结算。
- 一个 AttemptCall 同时绑定 call-count、cost/token 和多个 artifact reservations；并发/崩溃只有一次 AUTHORIZED→DISPATCHING claim，任一预算行都不能单独触发重发。
- artifact declaration 先固定 role/path/media type 和同一 AttemptCall 的 ARTIFACT_BYTES reservation；writer token 与 occurrence 分别通过组合 FK 绑定 declaration。直接绑定 `LLM_CALLS`、错误 subkey，或把普通输出 token 重标为 PACKAGE_MANIFEST/RECEIPT/FINAL_REPORT 必须在真实 SQLite 提交时失败。
- pin/occurrence 的组合 FK 必须拒绝与 token `final_digest/final_size` 不同的 Blob；occurrence 还必须拒绝非 FINALIZED token。并发双开只有一次 PREPARED→OPEN，重复 finalize 仅允许同 digest/size 幂等重放，不同内容失败。
- Artifact 写入中途超过总字节预算立即停止。
- LLM/Similarity/Sandbox adapter 尝试绕过 MeteredArtifactSink 或写未声明 role/media type 时被契约测试拒绝。
- 编译期/架构测试确认低层 Blob put 不导出到 Sink 包外；维护接口不能写 run artifact/occurrence。
- dedup hit、新 Blob、写入失败和 attempt CAS 失败都只结算一次 artifact reservation；Sink 与 attempt 事务不会双重扣费/退款。
- cache entry 与当前 attempt 并发过期/GC 时，PinExisting 在 `OpenVerified` 前提交的 ACTIVE pin 保护 Blob 直到 occurrence/失败结算；若校验失败则不产生 occurrence。
- cache hit 创建完整的 CACHE_PIN AttemptCall/cache_pin_use/declaration/reservation/FINALIZED token/pin 父链；cache key、原 producer call 与 capability observation 由组合 FK 绑定，不能只改 value_json 伪造来源。外部调用账户不扣减且该 call 永不能 dispatch。
- PROBING 只允许 PROBE_CACHE_PIN；同 dependency identity + capability policy 的 observation sequence 必须达到 checkpoint `min_observation_seq`，且 status=HEALTHY。旧序列、其他 identity/policy、普通 CACHE_PIN 或错 probe parent 均在提交前被拒绝；不同 policy 的失败不会污染当前 policy 的序列，回拨 wall clock 不改变结果。
- 两个 run 并发授权同 dependency observation 时 ticket sequence 原子递增无重复；构造“健康 H 先观察后暂停提交、失败 F 提交 BLOCKED、H 迟到完成”，H 的授权 ticket 必须低于 floor，不能解除 BLOCKED。BLOCKED floor 使用事务当时 counter+1，而非结果落库时间。
- 两个并行调用的重叠运行时间只计入一次 run active wall time；BLOCKED/NEEDS_REVIEW 区间以及 lease expiry 到新 lease acquisition 的停机空窗不累计，恢复 lease 取得后继续累计。
- heartbeat 后任意时点崩溃，恢复对最后 heartbeat 到旧 lease expiry 做有界保守补计；长时间停机不被错误计入，反复崩溃也不能绕过 max_wall_time。

## 5. SQLite/Blob 故障注入

在完成事务的每个写点注入进程终止，验证：

- event 与 snapshot 不半提交。
- attempt/result 重放幂等。
- CAS 阻止旧 revision 覆盖。
- sibling 仅推进 run_version 时相同 input digest 可幂等重交；input 真正失效时走 stale-result 结算且不接入 current snapshot。
- Blob 存在但 occurrence 不存在时最终被 GC。
- GC 两阶段 DELETING claim 与 PinExisting/dedup 并发线性化；canonical→trash→DB row→trash cleanup 各故障点重启后可收敛，DELETING 字节不能被读取或重新引用。
- 同 Blob 多 occurrence 的 provenance 都存在。
- DB 损坏/磁盘满返回分类故障，不静默丢证据。
- CANCEL/终态/attempt 守卫通过 `BEGIN IMMEDIATE` 串行化；busy 重试耗尽返回明确冲突，不发生先读后写竞态。
- READY pointer 跨 run、指向 IMPORTED occurrence、status shadow 不一致或 receipt/final report 为空时，真实 SQLite 延迟组合 FK/CHECK 在提交时拒绝；manifest/receipt/final artifact 来自其他 run 或 role 不匹配也被组合 FK/role shadow 拒绝。
- pin/occurrence 伪造其他 run/attempt/producer lease epoch 时组合 FK 拒绝；recovery 只能改 custodian epoch，不能改 producer provenance。
- GC claim 在同一延迟 FK 事务把永久 blob_pin_identity 从 LIVE 改 RETIRED、迁入无 Blob FK 的 history 后再删除 live pin/claim Blob；验证同 pin ID/writer token 无法重新插回 live，history 保留审计。
- recovery claim/reconcile/commit 三阶段故障注入：Docker、文件哈希和 OpenVerified 期间不持写事务，另一个 run 可正常提交；fencing/version 改变只让 intent STALE。
- 连续两个 recovery owner 分别在 CLAIMED/RECONCILING 崩溃；新 epoch 必须先把旧 active intent 原子置 STALE，再成功创建唯一新 intent。
- active DEPENDENCY_PROBE 恢复只能在一个延迟 FK 事务中换到新 RUNNING parent 或原子退出 PROBING，绝不存在 pointer 指向 ABANDONED attempt 的已提交状态。
- SandboxExecution 在每个资源/phase 边界可恢复；target 未确认 STOPPED 时 run 保持 QUIESCING，不能提交 CANCELLED/BLOCKED/READY。
- 已知 container ID 在 recovery 时没有历史 die event，但 Wait/Inspect stopped 或满足 create-settlement 条件的 exact NotFound 仍可证明安全停止；缺 die event 只让 verdict evidence 不完整，不得永久卡住 QUIESCING。

## 6. DockerSandbox 安全测试

Slice 0 覆盖不依赖 SQLite recovery 的协议能力，Slice 1 起用真实 migration/AttemptCall/SandboxExecution 重跑完整集成；各项检查 Docker Inspect、执行记录、计量及无残留资源：

1. **direct target**：目标程序是非 root PID 1，无 init/supervisor，CapDrop=ALL、no-new-privileges、network=none、restart=no、AutoRemove=false。
2. **输入传输**：源码/程序/输入只从 `OpenVerified` 流入 Engine volume；没有 host bind、archive、artifact root、workspace 或 daemon volume path 暴露。
3. **跨停止输出**：target 停止/删除后 keeper 仍持有 quota tmpfs volume，编译产物和声明输出可由只读 export helper 完整提升；PendingArtifact 持久化前不得删卷。
4. **硬配额**：local-driver tmpfs 的 size/uid/noexec 选项实际生效，越界写被限制；不支持时 profile BLOCKED，不能回退普通无界 writable volume。
5. **独立 watchdog**：Slice 0 验证 control record ACL、完整 plan/name/labels 预告、`resource_plan_digest`/`engine_identity_digest` 绑定、event subscription、pre-create ACK 和 deadline Stop/Kill；在 Create 返回前、Create 后写 ledger 前及 deadline 后迟到 Create 注入强杀，watchdog 均发现并停止，且在 UNKNOWN create 未收敛时不退出。额外强杀 watchdog 进程/关闭 control channel，验证 owner 立即 quiesce、禁止新 Start 并触发独立 cleanup；Slice 1 再在程序运行、target 已停未 export、cleanup 三点强杀 CLI，验证 startup janitor 与 cleanup-only TAKEOVER；旧 operation 不续跑 export，全部容器停止后 ABANDONED/重跑，watchdog 直到 CLEANED 才退出。
6. **有界日志**：target/helper `LogConfig=none` 且 Start 前 Attach；无限 stdout/stderr 读到 limit+1 得 OLE 并停止 target，Docker daemon 数据目录不持续增长。若使用 bounded-local 兼容档，单文件/文件数上限必须以磁盘 canary 验证。
7. **mvp 内存**：Inspect/容器内证据同时证明 `Memory=请求值`、`MemorySwap=Memory`、`memory.max=请求值`、`memory.swap.max=0`；PID 1 OOM 和子进程 OOM（含 PID 1 存活/退出 0）都由 target OOM event/State 捕获为 MLE，CPU/RSS 缺失仍可形成合法 mvp 记录。
8. **release 指标**：只在 allowlisted rootful Linux；target 使用预建父 cgroup，持久化受限相对路径/creation nonce/owner epoch/inode-device identity，分别在 mkdir 前后强杀 owner 并由 watchdog/janitor 按 cgroup 路径收敛；瞬时退出后仍能读层级 cpu.stat/memory.peak/memory.events，helper 不在其中，`populated=0` 后才删除；缺 capability 时 release BLOCKED。
9. **收敛语义**：user cancel、`step_deadline/run_budget_deadline`、lease/revision 失效均使用 background cleanup context 执行 TERM→KILL→Wait/Inspect；Wait、明确 stopped Inspect 或满足 create-settlement 条件的 exact NotFound 均可证明安全停止，die event 缺失只影响 verdict。Engine 暂时失联产生 CleanupPending/QUIESCING，绝不伪造 INFRA verdict 或提前终态。
10. **时间边界**：create、attach/watchdog-arm、engine-start、`program_hard_deadline`、stop/kill/wait、export/evidence、cleanup/`watchdog_safety_deadline` 分别注入超时；Start 未知不是 TLE，只有明确 Start 后 `program_hard_deadline` 是 TLE。
11. **完整配置反查**：Start 前 Inspect 只允许 `/program,/src,/input` RO volume、`/work` tmpfs、可选 `/result` quota volume和显式 `/dev/shm`，且所有 volume `NoCopy=true`；拒绝镜像 Config.Volumes/额外 writable mount/镜像路径预填充。环境、WorkingDir、TMPDIR/HOME/GOCACHE/GOPROXY、ulimit、Pids/Shm/CPU/memory 均等于 profile。
12. **逐容器授权**：同一 logical operation 的 import/keeper/target/export 各有独立 AttemptCall、sandbox-run reservation 和 sealed grant；缺少、多余、重复、错序 grant，或 CallTrace 集合/ResultAttemptCallID 不匹配时失败关闭。明确未 Create 的预留 grant 只能 ABORTED_NO_DISPATCH。
13. **release 可重复性**：Resource Gate 串行、固定 cpuset/CPU quota；存在旧 epoch cpgen 容器、父 cgroup归属不明或测量窗口污染时 timing evidence 作废。
14. **Engine 身份**：显式 endpoint、daemon/instance marker、runtime/cgroup/security 与镜像 digest 任一变化都使 capability cache 失效；ambient DOCKER_HOST/context 被拒绝，Runner/watchdog 身份不一致时 Create/TAKEOVER 前失败关闭；MVP 对 `tcp://`、`ssh://`、remote context 配置明确拒绝。
15. **计划与 identity 完整性**：替换/删减 resource plan、重复 deterministic name、engine identity 或 plan digest 不一致、Docker/volume label 冲突、cgroup 路径/nonce/owner epoch 不匹配，均在 Create/mkdir/cleanup 前失败关闭，不得删除不属于本 operation 的资源。

通用对抗 fixture 还包括：fork bomb/PIDs、CPU loop/TLE、内存增长/MLE、无限输出/OLE、子进程逃逸、`/proc`/网络/socket/秘密探测、shell/参数注入、symlink/hardlink/device/FIFO/socket/TOCTOU 输出、伪造执行记录，以及 scan→Create/Start 之间的旧 owner fencing 竞态。任何权威 Engine/cgroup 证据缺失或矛盾均为 `INFRA_ERROR(runtime_evidence)`。

高风险安全测试仅在专用 Docker VM/CI Runner 执行，不在含私人数据的宿主机运行。

## 7. Judge 测试

- ADR-0003 的每个 testlib code 都有 fixture。
- testlib digest/compile macro 变化导致 manifest/cache miss。
- Validator 正负例共同防止 always-accept/always-reject。
- Differential 输入先经 Validator，输出通过 Checker 而非字符串比较。
- 内置 checker 故障走基础设施路由；生成 SPJ 故障回退 SPJ Agent。
- Validator/Checker 执行得到 Sandbox `INFRA_ERROR` 时不生成业务 Outcome、不触发 Data/SPJ revision；暂时性故障进入 `BLOCKED`，不可恢复故障进入 `FAILED`。
- Resource Gate 不读取普通运行 cache 的 timing。
- Slice 3 smoke 先用可信 A+B fixture 自检 BUILTIN checker，再在声明 sample input 上只要求 reference/brute EXITED(0)/输出有界；不比较 expected output、不调用 Validator。smoke 失败走 Statement+Solution 联合诊断且不产生 Gate evidence。正式 Sample Gate 仅在 current Validator/Reference/Checker 都绑定同一 ProblemSpec revision 后运行。
- 以 RunRelation × InputOrigin × ToolRole × ToolOrigin × OutcomeCategory 做矩阵测试：Compile 使用 `InputOrigin=NONE`，可信 A+B checker fixture 使用 `TRUSTED_FIXTURE`；Sandbox INFRA_ERROR 和 `FailureRouteClass=TRUSTED_TOOL_INFRASTRUCTURE`（保留原始 `CheckerOutcome=CHECKER_ERROR`）优先走基础设施；VERIFICATION 的任意内容失败只生成 blocker/FAILED；GENERATED/DERIVED 的 GENERATED/IMPORTED Validator/Checker CE、signal、TLE/MLE/OLE、未知码和语义 QA 分别回 Data/SPJ 产生新 revision，不覆盖 imported Blob。

## 8. Similarity 测试

- 请求/响应 Schema、大小、timeout、TLS/redirect、认证脱敏。
- 429/5xx 重试与熔断。
- effective rewrite/rerank 降级进入 Evidence。
- Evidence/Decision 两级 cache 独立失效。
- 无 index version 的短 TTL。
- 服务不可用为 `BLOCKED`，不是 PASS。
- health TTL 到期后必须重验证；重验证不可达时，当前 revision 已提交 Evidence 可继续使用，新 Similarity step 不得靠过期 cache 前进。
- public endpoint 未显式授权时网络请求数为零，任务进入可审核状态。
- package-safe similarity report 无法表示 third-party body/original/t0/t1/raw metadata；未知字段、凭据 URL 和超长标题失败关闭。
- generation/derived Package Gate 将 package-safe report 与实际 Evidence/Decision occurrence 确定性交叉核对；任一 digest、policy、reason 或 hit 投影篡改均拒绝。
- 外部 verify 不把 origin similarity PASS 当作 current Evidence；必须生成新的包外 Evidence/Decision 绑定，服务不可达时不得 READY。
- LLM/Similarity/Sandbox 的成功、全部 retry 失败、probe、cache hit 和熔断/预算预拒绝均生成同一 MeteredOutcome/CallTrace Schema；DISPATCHED 的 ResultAttemptCallID 必须属于当前 logical operation，CACHE_HIT 的 source 与当前 pin 不得互相冒充，NO_DISPATCH 不得伪造 physical/result/cache 字段。CallOperation terminal projection 与返回 trace 必须逐字段一致。
- 预算/熔断/策略预拒绝先持久化 `CallOperation(OPEN)`，再终结为 `FAILED + NO_DISPATCH`；测试确认没有 AttemptCall/reservation 仍可恢复，且重试/缓存不能把该 operation 伪装为 physical dispatch。

## 9. Package/CLI 测试

- manifest 路径、hash、size、额外文件和 Unicode/大小写冲突。
- verify 导入时并发替换/链接交换 observed path，后续 reverse-read 与代码执行仍只使用同句柄复制出的已校验 Blob。
- 外部 verify 的最终 Package Gate 只读 Blob 重建的私有 staging；导入后篡改/删除 observed path 不改变验证结果或 package ID。
- staging 原子发布及崩溃残留清理。
- flush/rename/parent-flush/READY 各边界故障注入；跨 volume staging 配置拒绝，release durability capability 缺失时 BLOCKED。
- CLI 每个状态前置条件和 exit code。
- Docker cleanup 超出本地等待时 CLI 返回 10，run 保持 RUNNING/QUIESCING 且 watchdog 已武装；resume 先收敛后才能调度普通 Step。
- cleanup handoff 只有在 QUIESCING、全部未停止执行为 CLEANUP_PENDING、watchdog ACK 有效且无新 dispatch capability 时才能原子写 OwnerYieldedForCleanup/释放 lease；NORMAL/PROBING 或记录不全时拒绝。
- `--json` 没有日志污染或秘密。
- ReviewDecision 全流程及审计输出。
- 题包 rename 后、READY 事务前注入崩溃，可重验并认领相同 package ID；冲突目录不覆盖。
- 同一 package ID 被两个 verification run 重验时只有一个 package identity、两个独立 occurrence/receipt/QualityReport；原目录和原证据不变。
- 外部导入在完整验证前 occurrence 只能是 IMPORTED 且不能被 READY 引用；失败验证不会伪造 receipt，成功事务原子转 VERIFIED。
- READY 事务必须在同一 run 内把 occurrence 填入 receipt/final report、转 VERIFIED，并同时更新 READY pointer/status shadow；跨 run 或半完成 occurrence 被数据库约束拒绝。
- 用 release profile 重验 mvp 包时，receipt 绑定新的外部 PrePackage evidence/profile/environment，包内原始报告不被当作当前验证证据。
- Exporter 只能写分配给目标前缀的 MeteredPackageWriter，不能取得 staging/BlobStore 路径、创建未声明路径或绕过单文件/总包字节限制。
- staging 中 `manifest.json` 的字节必须与 manifest PendingArtifact 经 `OpenVerified` 读取的字节完全相同；manifest/receipt/final report 的 pin 在 READY 事务前崩溃时由 recovery 收敛。

## 10. Slice 验收

- Slice 0：Docker/Judge/最小 PackageStructuralGate 纵向链通过，不产生正式 READY run。
- Slice 1：重启恢复、预算、review CLI 和原子提交通过。
- Slice 2：Fake/真实显式 smoke 的 LLM + Similarity 契约通过。
- Slice 3：生成解法并通过 Compile Gate/SampleExecutionSmokeCheck；Docker blocked/resume 通过。
- Slice 4：Validator、正式 Sample Gate、Differential、正式答案和 Resource Gate 通过。
- Slice 5：内部/Polygon fixture、waiver、Package Gate 和完整 E2E 通过。
