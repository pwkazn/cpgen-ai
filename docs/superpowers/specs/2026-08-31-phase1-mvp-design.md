# Phase 1 MVP 实施设计

日期：2026-08-31
状态：按 Slice 0 → 5 顺序交付的方案已批准；本文待用户复核

## 1. 目标与事实来源

Phase 1 的目标是完成 `plan.md` 中“阶段一：核心闭环（MVP）”，而不是只完成 `Slice 1`。交付物是一个 Go CLI：它能把生成请求转换为非 SPJ 题目的可复验题包，并保存创意、题面、查重、解法、数据、Judge、质量门禁和导出过程的证据。

出现冲突时按以下顺序执行：

1. `plan.md` 的产品范围；
2. `ARCHITECTURE.md` 的冻结边界；
3. `docs/adr/` 的关键决策；
4. `docs/design/` 的模块契约；
5. 已发布 Schema、数据库迁移和代码。

本设计不重新定义这些文档，而是规定如何把它们按 Slice 落地、验证和提交。

## 2. 范围与非目标

Phase 1 包含：

- Slice 0：Docker 安全执行基座和纵向探针；
- Slice 1：SQLite 状态、预算、制品、恢复和清理；
- Slice 2：请求、创意、题面、LLM 和 Similarity；
- Slice 3：解法生成、DockerSandbox 和 Judge；
- Slice 4：Generator、Validator、差分、答案和资源门禁；
- Slice 5：内部题包、导出、完整 CLI 和端到端 READY。

Phase 1 不包含 Web/API、远程 Worker、分布式队列、本地向量模型、Wrong Answer/Hack Loop、生成式 SPJ 或主动人工审核点。这些属于 Phase 2/3。

## 3. 架构与组件边界

系统保持 Go 模块化单体：

- `internal/domain` 保存版本化值对象、状态、outcome、digest 和不可变业务规则；
- `internal/port` 定义 LLM、Similarity、Sandbox、Artifact 和持久化端口；
- `internal/workflow` 以静态类型化 Step 组合流程，唯一负责状态、预算和制品引用的提交顺序；
- `internal/adapter` 实现 Fake、SQLite/Blob、HTTP provider、Docker 和 exporter；
- `internal/judge` 只解释 compile/process/validator/checker 结果和质量门禁；
- `internal/cli` 提供机器可读、版本化的命令和 JSON 输出；
- `cmd/cpgen` 只做依赖装配、配置加载和退出码映射。

依赖只从 CLI/Workflow 指向端口和领域，再由装配层选择 adapter。领域与工作流不得依赖 Docker、SQLite、HTTP provider 或供应商字段。目标程序只能通过类型化 DockerSandbox 请求执行，不存在 host process fallback。

## 4. 数据流

主链路如下：

```text
GenerationRequest
  -> immutable request/config snapshot
  -> IdeaBatch -> feasibility -> IdeaSelection
  -> Statement revision -> Similarity Evidence/Decision
  -> Solution/Brute/Generator/Validator source bundles
  -> Docker compile/run outcomes -> Judge outcomes
  -> Sample/Differential/Coverage/Resource gates
  -> promoted tests + reference answers
  -> internal package -> reverse read -> Package Gate
  -> VERIFIED package occurrence -> READY
```

每个外部调用先创建 logical operation 和唯一 physical attempt claim，再执行 I/O，最后把 `MeteredOutcome`、`CallTrace`、预算结算和制品 occurrence 原子投影到 SQLite。Blob 以 SHA-256 寻址；run/step/attempt 只引用带 producer、revision 和 lease epoch 的 occurrence。

内容 revision 变化必须使依赖的查重、解法、数据、答案、门禁和题包证据失效。缓存只复用输入、版本、策略和 capability digest 全部匹配的结果；Resource Gate 不复用普通运行的 timing cache。

## 5. Slice 交付与提交边界

Phase 1 横跨多个独立子系统，因此不创建一个不可审查的巨型代码计划。每个 Slice 在开始实现前都根据本设计和对应详细设计生成自己的实施计划；该计划只覆盖当前 Slice，完成退出条件并提交 checkpoint 后，才为下一 Slice 生成计划。

### Slice 0：执行基座

完成现有 M0 代码，加入显式本机 Docker endpoint 校验、`docker-direct-v2`、固定镜像/toolchain manifest、watchdog、A+B fixture、最小 Blob/Package 探针。必须证明 direct PID 1、非 root、禁网、只读 rootfs、精确 memory/no-swap、PIDs、跨 stop 配额输出、OLE 停止和不可见 Docker socket/宿主秘密。Slice 0 只产生 probe package，不产生正式 READY 或 `PackageVerificationReceipt`。

### Slice 1：可恢复核心

加入顺序 migration、run/step/attempt/event/snapshot、状态机、execution lease/fencing、取消、预算、CallOperation/AttemptCall、Blob/pin/occurrence、SandboxExecution、recovery intent 和 janitor。故障注入必须证明无半提交 snapshot、预算超卖、旧 owner 写入或 pin 泄漏，且外部 I/O 时不持 SQLite 写锁。

### Slice 2：创意、题面与查重

加入版本化 request/prompt/schema、Fake 和一个真实 LLM provider adapter、定价与隐私门禁、Idea/Statement revision、Similarity HTTP adapter、Evidence/Decision 两级缓存及 mutation lineage。CI 使用确定性 Fake；真实服务 smoke test 必须显式启用。未授权远端数据、未知定价或 Schema 漂移必须在发请求前失败或进入分类后的 BLOCKED。

### Slice 3：解法与 Docker Judge

加入 Solution/Brute typed Steps、SourceBundle、完整 Compile/Run 白名单、逐容器计量、run-scoped capability probe、sample execution smoke 和 release cgroup v2 证据。CE/RE/TLE/MLE/OLE/INFRA_ERROR 必须可区分，可信 checker 故障不能被误报为选手答案错误。

### Slice 4：数据与质量门禁

加入 TestPlan、固定 seed 派生、Generator/Validator、正式 Sample Gate、小数据差分、测试逐点原子提升、答案 self-check、Coverage Gate 和 Resource Gate。所有正式输入必须通过同 revision Validator；所有答案必须由记录 digest 的标程生成；反例可重放；下游证据随 revision 正确失效。

### Slice 5：题包与 E2E

加入 `cpgen.package/v1`、原子 staging/flush/rename、reverse reader、Package Gate、Internal exporter、固定 Polygon fixture、package-safe Similarity report、完整 `generate/run/resume/review/verify` CLI JSON Schema，以及 2～3 个固定题目的端到端回归。READY 只能引用同 run 的 VERIFIED package occurrence；Package Gate 不能跳过。

每个 Slice 满足 `docs/implementation-plan.md` 和 `TODO.md` 的全部退出条件后，运行该 Slice 的新测试与全量回归，再创建一个明确的阶段提交。提交建议使用：

- `phase1(slice-0): complete execution probe`
- `phase1(slice-1): complete recoverable workflow core`
- `phase1(slice-2): complete idea statement and similarity flow`
- `phase1(slice-3): complete solution sandbox and judge`
- `phase1(slice-4): complete data and quality gates`
- `phase1(slice-5): complete package and end-to-end workflow`

未通过退出条件不得提交为“Slice 完成”，也不得开始下一 Slice。当前工作树中的 M0 代码属于 Slice 0，不能伪装成额外已完成阶段。

## 6. 错误处理与恢复

- Docker、LLM 或 Similarity 暂时不可用时进入 typed `BLOCKED`，保留 checkpoint，恢复后从原 Step 继续；
- 内容冲突、相似性证据模糊或预算需要人决定时进入 `NEEDS_REVIEW`；
- schema/version/enum 未知值、完整性破坏和不可恢复的业务错误 fail closed；
- `PortFailure`、compile/process outcome 与 Judge verdict 分层保存，路由不能覆盖原始事实；
- 取消通过持久化 control request 和 SQLite 提交顺序决定优先级；
- 崩溃恢复使用 fencing epoch、recovery intent、watchdog 和 janitor，旧 owner 不能继续 dispatch、export、Create 或写入；
- Docker 能力不足时返回分类证据，不 panic、不降低隔离要求、不走宿主执行。

## 7. 测试与验证策略

每个行为先写失败测试，再写最小实现。测试分层为：

- 领域、Schema、digest、状态转换和门禁的 table-driven unit tests；
- Fake adapter 与本机 `httptest` 的端口契约测试；
- SQLite 原子性、并发、迁移、lease、预算和 crash-injection 集成测试；
- Docker capability、安全边界、资源结果和 watchdog smoke/integration tests；
- 固定 seed、固定 fixture 和固定 package hash 的确定性 E2E；
- CLI golden/JSON Schema、reverse reader 和崩溃点恢复测试。

每个 Slice 提交前至少执行：

```text
gofmt 检查
go test ./...
go vet ./...
go test -race ./...
该 Slice 对应的显式 integration/smoke tests
```

真实付费或外部服务不属于默认 CI 依赖；其 adapter 通过本机协议 fixture 验证，显式 smoke 记录 provider、endpoint class、版本、预算和 provenance。Docker 在当前主机可用，但运行时仍必须通过显式本机 endpoint 与 capability doctor，不读取 ambient remote context。

## 8. 完成审查

Slice 5 阶段提交完成后，调用一次独立 subagent，对 Phase 1 的全部提交和当前工作树进行整体 review。Review 必须逐项对照 `plan.md`、`ARCHITECTURE.md`、ADR、`docs/implementation-plan.md`、`TODO.md` 和测试证据，重点检查安全边界、恢复原子性、预算、revision/invalidation、Judge 分层、Package Gate 与 CLI E2E。

主 agent 修复 review 发现的问题并创建明确的修复提交；按相同范围重新运行全量验证。只有所有 Phase 1 要求都有直接证据、工作树无未解释变更、最终验证通过时，才可把目标标记为完成。

## 9. 当前环境约束

- 当前分支为 `main`，相对 `origin/main` ahead 1、behind 1；不在 Phase 1 实施中执行破坏性 reset/rebase；
- 当前工作树已有未提交的 M0/Slice 0 代码和文档状态更新，实施时必须保留并审查这些改动；
- 当前 Docker Desktop 提供本机 npipe、Linux Engine、cgroup v2、memory/swap/PID 限制；约 2 GB daemon memory 要纳入镜像构建和资源 fixture 设计；
- 外部 LLM/Similarity 凭据不是默认测试前提；缺失时应由真实 smoke 明确报告，而不能把 Fake 结果冒充真实调用。
