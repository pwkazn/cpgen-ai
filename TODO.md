# CP Problem Generator AI 开发 TODO

> 当前阶段：设计完成，代码尚未开始。  
> 开发主线：Go 模块化单体 + SQLite + Docker `docker-direct-v2`。  
> 详细约束以 [ARCHITECTURE.md](./ARCHITECTURE.md)、[实施计划](./docs/implementation-plan.md) 和对应 ADR 为准。

## 使用规则

- `[ ]` 未开始，`[~]` 进行中，`[x]` 已完成，`[!]` 被阻塞。
- 每个阶段必须先满足退出条件，才能进入下一阶段。
- 每个 PR 至少关联一个阶段目标、一个设计章节和对应测试。
- 新增状态、Schema、数据库字段或 Docker 权限时，必须同步版本、迁移、测试和文档。
- 真实 LLM、Similarity 和 Docker 服务只通过显式 smoke test 使用；CI 默认使用 Fake 实现。

## 总体里程碑

| 里程碑 | 目标 | 状态 |
|---|---|---|
| M0 | Go 工程和测试骨架可运行 | TODO |
| M1 | 可恢复的 SQLite 工作流核心 | TODO |
| M2 | 创意、题面、LLM 和查重闭环 | TODO |
| M3 | Docker 沙箱和 Judge 可验证 | TODO |
| M4 | Generator、Validator、差分和资源门禁闭环 | TODO |
| M5 | 题包导出和端到端 READY | TODO |
| M6 | Wrong Answer、Hack、SPJ 和人工审核 | TODO |

## Slice 0：工程启动与纵向技术探针

目标：建立可以持续运行的 Go 工程，并验证 Docker 安全边界，而不是先堆 Agent 逻辑。

### 工程骨架

- [ ] 初始化 `go.mod`、`cmd/cpgen` 和 `internal/` 分层目录。
- [ ] 配置 `go test ./...`、静态检查、格式化和基础 CI。
- [ ] 定义统一 `schema_version`、`Digest`、ID、时间和错误类型。
- [ ] 实现 `context.Context` 取消、阶段超时和测试用 fake clock。
- [ ] 建立 Fake LLM、Fake Similarity、Fake Sandbox 和内存 ArtifactSink。

### 领域与 Judge 基础类型

- [ ] 实现 `CompileOutcome`、`ProcessOutcome`、`ValidatorOutcome`、`CheckerOutcome`。
- [ ] 实现 `MeteredOutcome`、`CallTrace`、`PortFailure` 和 `FailureRouteClass`。
- [ ] 完成 ADR-0003 的固定 test vectors：CE、RE、TLE、MLE、OLE、VALID/INVALID、AC/WA/PE/CHECKER_ERROR。
- [ ] 所有 enum 反序列化拒绝未知值。

### Docker 探针

- [ ] 固定 `cpgen-builder`、`cpgen-runtime`、`cpgen-transfer` 镜像 digest 和 toolchain manifest。
- [ ] 实现显式本机 `unix://`/`npipe://` endpoint 校验，拒绝 TCP、SSH、remote context 和 ambient Docker context。
- [ ] 验证 direct PID 1、非 root、CapDrop=ALL、只读 rootfs、network none、no-new-privileges。
- [ ] 验证 `Memory=limit`、`MemorySwap=Memory`、`memory.swap.max=0`、PIDs 限制。
- [ ] 验证 Engine volume 导入、quota tmpfs output volume、keeper 跨 target stop 和只读 export。
- [ ] 验证 `LogConfig=none + Attach`、输出上限和 OLE 停止流程。
- [ ] 验证 detached watchdog：预告计划、pre-create ACK、迟到 Create、deadline Stop/Kill 和 control-record ACL。
- [ ] 验证 watchdog 异常退出/EOF 时 owner 禁止新 Start 并触发 cleanup。
- [ ] 验证 release cgroup v2 的 path、nonce、owner epoch、inode/device 识别和 `populated=0` 回收。

### Slice 0 退出条件

- [ ] Docker 可用时完成最小 compile → validate → differential → package 探针。
- [ ] Docker 不可用或能力不足时返回分类后的 `BLOCKED`，不 panic、不走 host process fallback。
- [ ] 网络、Docker socket、宿主宽泛 mount、环境秘密和宿主执行记录对目标程序不可见。
- [ ] Slice 0 不产生正式 `READY` 或 `PackageVerificationReceipt`。

## Slice 1：可恢复的工作流核心

目标：先解决状态、租约、计量、制品和崩溃恢复，再接入真实模型。

### SQLite 与状态机

- [ ] 编写顺序 migrations，启用 foreign keys、WAL 和 busy timeout。
- [ ] 实现 `runs`、`steps`、`attempts`、事件日志和 immutable request snapshot。
- [ ] 实现 `CREATED/RUNNING/BLOCKED/NEEDS_REVIEW/READY/FAILED/CANCELLED` 状态机。
- [ ] 实现 `NORMAL/PROBING/QUIESCING` execution mode 和 CANCEL control request。
- [ ] 实现 execution lease、单调 fencing epoch、旧 owner 拒绝和双 CLI 竞争。

### 调用、预算和制品

- [ ] 实现 `CallOperation` / `AttemptCall` 的唯一 dispatch claim 和 terminal projection。
- [ ] 实现 `DISPATCHED`、`CACHE_HIT`、`NO_DISPATCH` 三类 `CallTrace` 校验。
- [ ] 实现 typed `PortFailure`、retry、UNKNOWN 和 reservation settlement。
- [ ] 实现 LLM、Similarity、Sandbox、Artifact bytes、wall time 预算账户。
- [ ] 实现 BlobStore、ArtifactDeclaration、writer token、pin、occurrence 和 `OpenVerified`。
- [ ] 实现 cache source、cache pin、capability observation ticket/floor 和 probe cache。
- [ ] 实现 mutation budget account、claim 和 record 的组合 FK provenance。

### 恢复与清理

- [ ] 实现持久化 `SandboxExecution`、resource plan、engine identity 和资源生命周期。
- [ ] 实现 recovery intent 三阶段协议和启动 janitor。
- [ ] 实现 cleanup-only watchdog `TAKEOVER`，禁止旧 operation 继续 Start/export/Create。
- [ ] 实现 active probe 的延迟 FK 原子切换或退出。
- [ ] 注入 owner 在授权、dispatch、写入、清理各边界崩溃，验证无半提交 snapshot、预算和 pin 泄漏。

### Slice 1 退出条件

- [ ] 同一 attempt 重复提交幂等，不同 digest 被拒绝。
- [ ] 并行分支预算不会超卖，取消优先级由 SQLite 提交顺序确定。
- [ ] `BLOCKED` resume、probe、lease 接管、cleanup handoff 和 `NEEDS_REVIEW` 流程通过集成测试。
- [ ] 恢复过程不在外部 I/O 时持 SQLite 写锁。

## Slice 2：创意、题面、LLM 与查重

目标：将请求稳定转换为可追溯的 `ProblemSpec`，并完成查重变异闭环。

### LLM 与请求

- [ ] 实现 LLM port、Fake adapter 和一个真实 provider adapter。
- [ ] 建立 PromptRef 版本目录、结构化 Schema 和单次 schema repair。
- [ ] 实现 pricing policy、整数成本预留/结算和 token provenance。
- [ ] 实现 data-class privacy policy；未授权的远端请求数必须为零。
- [ ] 实现 `GenerationRequestV1`、`GenerationRequestSnapshotV1` 和 effective config digest。

### Idea 与 Statement

- [ ] 实现 IdeaBatch、IdeaCandidate、确定性 feasibility 和 IdeaSelection。
- [ ] 实现 `stage_scope_digest`、mutation budget claim、mutation intent/record。
- [ ] 实现 Idea → Statement 的 digest 链和 revision invalidation。
- [ ] 验证 required/forbidden features、seed、候选数量、排序和选择理由可复现。

### Similarity

- [ ] 实现 `SimilarityProvider` 和 `yuantiji_v2` HTTP adapter。
- [ ] 实现请求/响应 Schema 校验、超时、有限重试、熔断和 fail-closed。
- [ ] 分离 Similarity Evidence、Decision、threshold policy 和 package-safe report。
- [ ] 实现 evidence cache、decision cache 和相似题触发的 Idea mutation 回路。
- [ ] 保存 Top-K 证据、provider、policy、model/index version 和 CallTrace。

### Slice 2 退出条件

- [ ] CI 全部使用 Fake 实现且结果确定性一致。
- [ ] provider/Similarity 不可用进入 `BLOCKED`，Schema 漂移不会假装通过。
- [ ] 相同 snapshot、seed、policy 和输入 digest 得到相同候选与选择结果。
- [ ] 相似度变异重新生成 IdeaBatch、feasibility、selection 和 Statement，不复用旧 selection。

## Slice 3：解法生成与 Docker Judge

目标：把标程、暴力程序和工具程序放入受控 Docker 环境，并得到可审计的程序结果。

- [ ] 实现 Solution/Brute typed Steps 和 SourceBundle manifest。
- [ ] 实现白名单 CompileRequest/RunRequest，禁止 shell command、任意 mount、环境和 Docker option。
- [ ] 实现 DockerSandbox 的 import/keeper/target/export 完整 ContainerPlan。
- [ ] 实现逐容器 AttemptCall、sandbox-run 预算和 sealed dispatch grant。
- [ ] 实现 `SampleExecutionSmokeCheck`：可信 A+B checker fixture + sample input 进程检查。
- [ ] 实现 Judge Harness role adapter 和 `FailureRouteClass` 路由。
- [ ] 实现 run-scoped Docker capability probe、doctor 静态检查和 blocked/resume。
- [ ] 实现 watchdog、日志、stdout/stderr、OOM、TLE、OLE、停止证明和 cleanup evidence。
- [ ] release profile 实现 cgroup v2 的 CPU、peak memory、OOM 和 `populated=0` 证据。

### Slice 3 退出条件

- [ ] CE、RE、TLE、MLE、OLE、INFRA_ERROR fixture 全部通过。
- [ ] Sample smoke 不调用 Validator、不比较 expected output、不产生正式 Gate evidence。
- [ ] Checker 原始 `CHECKER_ERROR` 不被覆盖，可信工具故障通过 `FailureRouteClass` 进入基础设施路径。
- [ ] CLI 被强杀后 watchdog/janitor 能在安全期限内收敛 target 和 helper。

## Slice 4：数据、差分和资源门禁

目标：生成可验证、可复现、能区分错误解的正式测试数据。

- [ ] 实现 TestPlan、size profile、coverage tags 和固定 seed 派生。
- [ ] 实现 Go/C++ generator、testlib validator 编译和安全执行。
- [ ] Validator 绑定同一 ProblemSpec revision，正例/负例门禁可区分工具错误。
- [ ] 实现正式 Sample Gate、Small-input Differential Gate 和反例制品。
- [ ] 实现正式测试逐点原子提升、输入 Blob provenance 和 Validator evidence。
- [ ] 实现标程生成 `.ans`、answer self-check、Checker 版本绑定。
- [ ] 实现 Coverage Gate、Resource Gate 和 release timing profile。
- [ ] 实现 revision/invalidation：题面、标程、暴力、generator、validator、checker 变化自动失效下游证据。
- [ ] 固定 seeds 重跑结果一致；普通 run cache 不得污染 Resource Gate。

### Slice 4 退出条件

- [ ] 所有正式输入通过 Validator，非法定向输入被拒绝。
- [ ] 小数据枚举与固定种子随机差分通过，反例可重放。
- [ ] 最大测试满足时间、内存、输出限制并留安全裕量。
- [ ] 所有 `.ans` 均由已记录 digest 的标程生成。

## Slice 5：题包、导出与端到端验收

目标：将已通过质量门禁的内部事实来源导出为可复验的 OJ 题包。

- [ ] 定义 `cpgen.package/v1` manifest、路径、哈希、大小和资源限制 Schema。
- [ ] 实现 atomic staging、durable flush、rename、reverse reader 和崩溃恢复。
- [ ] 实现 Package Gate：文件完整、hash 一致、答案存在、report/provenance 可反向读取。
- [ ] 实现 Internal exporter 和固定版本的 Polygon adapter/fixture。
- [ ] 实现 package-safe Similarity report 与原 Evidence/Decision occurrence 交叉核对。
- [ ] 实现 `generate`、`run`、`resume`、`review`、`verify` CLI JSON 输出。
- [ ] 完成 2～3 个固定简单题的完整 E2E 回归和包哈希校验。
- [ ] 验证 package rename、READY 事务、导入验证和并发重验的崩溃恢复。

### Slice 5 退出条件

- [ ] Package Gate 在 READY 之前不可跳过。
- [ ] `READY` 只能引用同一 run 的 VERIFIED package occurrence。
- [ ] `mvp` 与 `release` verification profile 在报告中明确区分。
- [ ] 同一输入、snapshot、镜像和工具链可重复生成相同题包哈希。

## Phase 2：质量增强

以下功能不阻塞 MVP：

- [ ] Wrong Answer Mutator：溢出、边界遗漏、错误贪心、状态缺失和复杂度退化。
- [ ] Hack Loop：错误解运行、反例搜索、最小化、回归集持久化。
- [ ] SPJ Agent：checker 生成、正例/合法异解/非法解/攻击用例门禁。
- [ ] 创意、题面和最终打包前的人工审核点。

## Phase 3：扩展能力

- [ ] HTTP/Web API 和更友好的 CLI/UI 交互。
- [ ] 可移植 checkpoint 和远程 Worker。
- [ ] 本地向量检索服务与相似度索引更新流程。
- [ ] 只有在单机吞吐成为实际瓶颈后，才评估消息队列、拆分 Worker 或 Kubernetes。

## 当前优先级

按以下顺序开始编码：

1. `internal/domain` 的 outcome、ID、Schema 和 ADR-0003 tests。
2. `internal/port` 的 Sandbox、Judge、Artifact 和 MeteredOutcome 契约。
3. Slice 0 的 Docker direct-run 探针与固定 A+B fixture。
4. Slice 0 最小 package reader/writer 和 fake workflow。
5. 通过 Slice 0 后开始 Slice 1 SQLite 状态机和恢复协议。

## 通用验收命令

```text
go test ./...
go vet ./...
docker version
docker info
docker build --pull=false --iidfile ...
```

真实服务 smoke test 必须显式触发，并单独记录 provider、endpoint class、镜像 digest、工具链版本、预算消耗和 provenance；不得成为默认 CI 依赖。
