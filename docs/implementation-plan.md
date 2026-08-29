# MVP 实施计划

## 1. 实施原则

- 按纵向切片提交，每个切片都可运行和测试。
- 先实现 Fake adapter，再接真实付费/外部服务。
- 机器可验证 Schema、迁移和 fixture 与代码同提交。
- 不提前实现 Web、分布式队列、SPJ 生成和本地向量模型。

## 2. Slice 0：纵向技术探针

### 交付

- `go.mod`、`cmd/cpgen` 最小入口。
- domain outcome 类型。
- DockerSandbox 最小 Compile/Run/Probe 与仅测试可构造的 `Slice0ProbeHarness`；正式 CLI 不暴露测试 dispatch capability。
- 固定 `cpgen-builder/cpgen-runtime/cpgen-transfer` Dockerfile、`docker-direct-v2` 执行协议和 toolchain manifest。
- `docker-direct-v2` 可行性探针：direct PID 1、精确 memory/no-swap、Engine volume import、quota tmpfs keeper 跨 target stop、只读 export、none+attach 日志，以及基于 test-scoped durable control record 的 detached watchdog 预告计划/deadline Stop/Kill；不在 Slice 0 假装完成 SQLite janitor/TAKEOVER。
- testlib role adapter。
- 固定 A+B 题目 fixture。
- 最小 BlobStore、Package builder 和 `PackageStructuralGate`，使用合成 PrePackage report（无 SQLite run 状态）。

### 完成条件

- `go test ./...` 通过。
- Docker 可用时完成 compile → validate → differential → package。
- Slice 0 只产生 probe package，不产生正式 READY run 或 PackageVerificationReceipt。
- Docker 不可用得到分类后的 blocked probe，而不是 panic。
- 超过题目内存限制、但低于旧版“限制 + supervisor headroom”的程序稳定得到 MLE；Runner/Engine 进程不计入目标 cgroup。
- fork 后超时能清理整个容器；目标程序不可访问网络、Docker socket、宿主宽泛 mount、环境秘密或宿主执行记录。
- Docker Desktop/WSL2 与 Linux Engine 分别记录 capability/measurement profile；无法满足 release cgroup v2 指标时明确返回 blocked probe outcome。
- ADR-0003 test vectors 全覆盖。
- ADR-0004/0005 的 direct PID 1、精确 memory/no-swap、跨停止输出、watchdog、日志磁盘上限和外部 Runner 边界测试通过。
- 每个 import/keeper/target/export `ContainerCreate` 都有独立 ProbeDispatchLedger claim 和 sandbox-run 计量；logical CallTrace 可完整重建。

## 3. Slice 1：可恢复骨架

### 交付

- SQLite migrations 和 Repository。
- run/step/attempt/event/snapshot 状态机。
- execution lease/fencing、持久化取消 control request。
- Blob/PendingArtifact/ArtifactOccurrence、pin/GC 和 `OpenVerified`。
- CallOperation/AttemptCall 唯一 dispatch claim、成功/typed PortFailure 共用的 MeteredOutcome/CallTrace terminal projection、一次性 writer token/组合 FK、CACHE_PIN/PROBE_CACHE_PIN session、budget settlement 与 active-wall lease 计量。
- 持久化 SandboxExecution/resource lifecycle、三阶段 recovery intent 和启动 janitor。
- cleanup-only watchdog TAKEOVER；旧 operation 未完整持久化则停止、ABANDONED并以新 logical operation 重跑。
- `generate/run/review` CLI 骨架。
- Fake typed workflow 和 crash recovery。

### 完成条件

- event/snapshot/budget 同事务测试通过。
- 并行预算不会超卖。
- cache 过期/GC 与 `PinExisting` 并发时先 pin 后校验，物理调用只能由一个 AttemptCall claim 发出。
- BLOCKED resume 的 probing mode 受正常预算、active wall、CANCEL 和崩溃恢复约束。
- `BLOCKED/NEEDS_REVIEW` 恢复、waiver 和取消通过。
- 双 CLI lease 竞争、旧 owner fencing 和失联取消接管通过。
- 故障注入无半提交 current snapshot。
- 恢复期间 Docker/OpenVerified 不持 SQLite 写锁；active probe 以延迟 FK 原子换父或退出，READY 只能引用同 run VERIFIED package occurrence。
- GC claim 先把 RELEASABLE live pin 迁入无 Blob FK 的 history 再删 Blob；pin/occurrence 必须绑定原 producer lease epoch。

## 4. Slice 2：创意、题面与查重

### 交付

- LLM port、Fake adapter、一个真实 provider adapter。
- prompt 版本目录和结构化输出 Schema。
- provider 物理尝试计量、严格 JSON 解码、单次 schema repair 和 LLM provenance。
- digest 固定的 PricingPolicy、LLM data-class privacy policy 与请求前门禁。
- GenerationRequestV1、IdeaBatch/IdeaCandidate/IdeaSelection、mutation lineage 和 Statement revision typed Steps。
- Similarity HTTP adapter、yuantiji_v2 protocol。
- Similarity Evidence/Decision 两级 cache 和 Policy v1。
- 变异与预算回路。

### 完成条件

- 默认 CI 全 Fake、确定性通过。
- 显式 smoke 可验证真实 provider/Similarity Schema。
- 服务不可用进入 BLOCKED，Schema 漂移失败关闭。
- Policy 变化不重复请求 Evidence。
- prompt/schema/adapter 版本变化正确使 LLM cache miss，结构修复不可绕过预算。
- 无匹配定价/远端隐私授权时请求数为零；金额预留与结算使用整数固定向量。
- manual/random 请求、effective seed、候选数量/排序/选择理由及变异父链都有确定性 Schema/fixture；LLM 与 Similarity provenance 使用同一 CallTrace。

## 5. Slice 3：解法与 Docker 安全执行

### 交付

- Solution/Brute Steps 及结构化代码 Bundle。
- 完整 CompileRequest/RunRequest 白名单。
- release profile cgroup v2 精确计量、容器级 kill/reap 和输出限制。
- Compile Gate 与 `SampleExecutionSmokeCheck`：BUILTIN checker 使用可信 A+B fixture 自检；reference/brute 只在声明 sample input 上要求 EXITED(0)/输出有界，不比较 expected output、不调用 Validator，失败走 Statement+Solution 联合诊断且不生成正式 Sample Gate evidence。
- run-scoped Docker capability probe、doctor 静态检查和 blocked/resume。

### 完成条件

- CE/RE/TLE/MLE/OLE/INFRA_ERROR fixture 全通过。
- secret/network/mount 不可见。
- 输出安全提升攻击测试通过。
- `step_deadline/run_budget_deadline/program_hard_deadline/watchdog_safety_deadline` 互不混淆；CLI 被强杀后 watchdog 收敛 target，重启 janitor/recovery 可继续。

## 6. Slice 4：数据与差分验证

### 交付

- TestPlan、Go/C++ generator、testlib validator。
- Validator 编译后执行绑定同一 revision 的正式 Sample Gate。
- 固定 seed 派生、逐测试原子提升、Coverage evidence 和 answer self-check。
- Validator 正负例门禁。
- Small-input Differential Gate 和反例制品。
- 正式测试/答案生成、Coverage 和 Resource Gate。
- revision/invalidation 全链实现。

### 完成条件

- 所有小输入先过 Validator，输出统一经 Checker。
- 标程变化自动失效旧答案/性能/差分证据。
- Resource Gate 不复用普通运行 timing cache。
- 固定 seeds 可重现。

## 7. Slice 5：打包与端到端验收

### 交付

- `cpgen.package/v1` Schema 和 reverse reader。
- 原子 Package builder、`MeteredPackageWriter`、Package Gate。
- Internal exporter、固定目标版本的 Polygon adapter/fixture。
- 经 MeteredArtifactSink 固定的 manifest/receipt/final Quality report，以及与 Evidence/Decision 交叉绑定的 package-safe Similarity report。
- 完整 CLI JSON Schema 和 E2E。

### 完成条件

- Package Gate 必须在 READY 之前。
- waiver/revision 失效端到端通过。
- 从 GenerationRequest 到 READY package 可重复运行。
- package rename 与 READY 事务之间崩溃可幂等恢复。
- `mvp` 与 `release` verification report 明确区分。
- Exporter 只能写静态计划内路径，`max_package_bytes`、manifest exact-byte 绑定和 PendingArtifact pin 故障注入通过。

## 8. 推荐首批代码顺序

1. `internal/domain/outcomes.go` 与 ADR-0003 tests。
2. `internal/port/sandbox.go` 请求/结果类型。
3. docker-direct-v2 Runner + pinned builder/runtime/transfer image + watchdog。
4. `internal/adapter/sandbox/docker`。
5. `internal/judge/role_adapter.go`。
6. Slice 0 fixture 与最小 package reader/writer。
7. 通过 Slice 0 后再引入 SQLite 状态机。

## 9. 每个 PR 的最低要求

- 关联具体 Slice、ADR/设计章节和验收条件。
- 更新或新增测试；不得只依赖真实外部服务。
- 新增持久化字段必须带 migration。
- 新增 enum/Schema 必须拒绝未知值并有版本。
- 新增 Docker 权限、mount、环境变量或编译参数必须安全审查。
- 文档与代码契约不一致时，先通过 ADR/文档变更再合并实现。

## 10. MVP 之后

- Phase 2：Wrong Answer/Hack Loop、生成 SPJ、主动审核点。
- Phase 3：HTTP/Web、远程 Worker、可移植 checkpoint、本地 Similarity Service。
- 只有出现真实吞吐瓶颈后才引入消息队列或拆分服务。
