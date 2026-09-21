# CP Problem Generator 文档索引

[2026-09-13 架构简化计划](superpowers/plans/2026-09-13-architecture-simplification.md)跟踪读/导出与执行服务的分离、显式的阶段依赖、本地固定调度，以及历史兼容的整合。其实现与验证记录区分已完成变更与尚未完成的环境检查。本地调度器取代了早先的 LangGraphGo 封装，同时保留持久化的修订与转换契约。

[2026-09-14 架构后续](design/architecture-follow-up-2026-09-14.md)保留了首个提案，其实施因引入过多抽象而被否决。[返工记录](evidence/architecture-follow-up-2026-09-14.md)涵盖简化后的协调器、直接读取器、被移除的配置层以及当前验证。工具链快照后续沿用现有的生效配置；单独的发布账本仍是一项独立的设计决策。

## 文档层级

1. ARCHITECTURE.md 定义当前系统边界。
2. docs/adr 记录已接受的决策及其取代关系。
3. docs/design 包含组件与契约细节。
4. docs/implementation-plan.md 定义切片交付顺序。
5. docs/traceability.md 将需求映射到设计与证据。
6. docs/evidence 记录已完成的验证。
7. docs/superpowers/specs 包含已批准的实施设计。
8. docs/superpowers/plans 包含可执行的工程计划。

当文档冲突时，最新的已接受 ADR 及其指定的权威设计优先。ADR-0006 定义了 Slice 1（lightweight local workflow，轻量本地工作流）。

用户在 2026-09-09 修订了 MVP 优先级：[先完成可用的生成闭环](superpowers/plans/2026-09-09-mvp-generation-loop.md)。查重 ACCEPT 继续进入 Solution/Data/Docker/Judge/Quality/Package；未被接受的业务结果进入人工评审。变异推迟以重新设计。本范围修订优先于旧计划与组件设计中的变异优先顺序。

[Solution 切片](evidence/mvp-solution-foundation.md)、[Data 执行](evidence/mvp-data-foundation.md)、[Judge 答案与差分检查](evidence/mvp-judge-foundation.md)以及[采用题包格式 v2 的 Quality](evidence/mvp-quality-package-foundation.md)现在汇入[题包组装、原子 READY 与 CLI 导出](evidence/mvp-package-commit-foundation.md)。完整 MVP 配置通过了针对普通 C++ 问题的真实 Docker 与独立 CLI 验收，包括题包事务内的进程退出以及从导出 ZIP 的全新执行。随后的 [APINode 实时提供方测试](evidence/apinode-live-mvp-2026-09-10.md)通过了经 Docker、导出与独立重验证的真实模型生成。查重仍为本地 fixture；实际原创性尚未检查。

新的 Solution/MVP run 会连同其生效配置一起持久化经过验证的规范工具链锁，因此即使配置的锁路径被移除，CLI 恢复与离线导出仍可使用该绑定快照。没有快照的历史 run 继续验证并读取原始锁路径；缺失的旧版锁仍为封闭失败。

[执行前沙箱恢复修复](evidence/sandbox-unsent-recovery-2026-09-15.md)允许在首个 `solution_verify` create 于执行记录存在之前失败后进行人工恢复，采用原子性的不发送检查与新的验证尝试。原始调用与记账保持完整；已发送/未知的工作以及现有执行记录仍保留回执恢复。

历史性的 2026-09-08 [库集成检查点](evidence/slice2-library-integration.md)记录了此前的进程内库修订与提供方边界测试。下文较晚的检查点记录预览 CLI 与持久化图集成；2026-09-13 的 ADR-0006 修订取代了图库实现选择。

随后的[提供方配置与持久化派发检查点](evidence/slice2-durable-llm-dispatch.md)记录了 LLM-02 与 LLM-03a，并由独立子代理验收。[私有结果重放检查点](evidence/slice2-private-llm-replay.md)、[有界 JSON 修复检查点](evidence/slice2-bounded-json-repair.md)与[私有缓存检查点](evidence/slice2-private-llm-cache.md)在完整门禁下完成 LLM-03b 至 LLM-05。[编译后的应用图](evidence/slice2-compiled-graph.md)在完整门禁下完成 WF-01。[开发日志](development-log.md)记录当前工作顺序。

[严格内容草稿](evidence/slice2-content-drafts.md)、[活动时间 LLM 账本桥接](evidence/slice2-active-llm-ledger.md)、[类型化已提交阶段输入恢复](evidence/slice2-committed-generation-inputs.md)与[持久化内容执行器](evidence/slice2-generation-executor.md)完成了 LLM-06a、WF-04a、WF-03a 与 WF-02a。[持久化查重证据检查点](evidence/slice2-durable-similarity.md)涵盖物理派发、私有重放与已提交读取。[类型化查重执行](evidence/slice2-typed-similarity.md)将经过验证的 Statement 链绑定到按尝试划分的证据与独立检查点；完整门禁通过。这些组件现在驱动下文已接受的预览服务。

[阶段间隙恢复修正](evidence/slice2-stage-gap-recovery.md)处理某个阶段已提交、其后继阶段尚未开始时的取消与陈旧前驱身份问题。

[提供方对账组件](evidence/slice2-provider-reconciliation.md)新增仅回执的终态清理，并阻止在提供方结算前释放阶段；完整门禁通过。

[显式实时预览](evidence/slice2-live-preview.md)连接冻结配置、生产 Bootstrap、类型化图提交、同尝试进程恢复以及提供方取消/预算清理。常规 tests/vet、生产与补充竞态验证、Linux 构建以及架构/格式/补丁检查均通过。默认仍为 Fake，预览终止于不可豁免的评审。业务路由与后续质量/题包门禁仍未完成。

[已提交查重路由计划](evidence/slice2-similarity-route-plan.md)完成了只读的决策/配额检查，并修正了遗漏的逻辑/题包预算限制。完整门禁通过。其感知变异的路由与旧的[业务路由计划](superpowers/plans/2026-09-09-slice2-business-routing.md)作为推迟的研究保留；前向 MVP 直接使用已提交的决策，不需要变异配额或授权。

[变异核心/意图契约](evidence/slice2-mutation-contracts.md)、[制品与结果账本加固](evidence/slice2-artifact-mutation-records.md)、[持久化候选收集](evidence/slice2-idea-candidates.md)、[原子变异完成与结果恢复](evidence/slice2-atomic-mutation-stage.md)与[独立的变异提供方契约](evidence/slice2-mutation-prompt.md)保留其已完成的门禁。进一步的变异开发已暂停；这些检查点不对 Solution 或题包施加前置条件。

[应用 fixture 准备变更](evidence/application-fixture-preparation.md)通过完整门禁，在保持数据库隔离的同时减少重复的测试搭建工作。[初始类型化批量发布器](evidence/slice2-idea-batch-output.md)同样通过完整门禁，包括真实的进程恢复与评审清理。它不改变预览图。

## ADR

| ADR | 决策 |
|---|---|
| 0001 | 静态类型化 CPGen 流水线与 activity 契约 |
| 0002 | 七种 run 状态、评审、重试、取消与本地重启 |
| 0003 | Judge 结果与确定性优先级 |
| 0004 | 在隔离的 Docker cgroup 中直接执行目标 |
| 0005 | Docker 执行生命周期、看门狗与跨停止传输 |
| 0006 | 轻量本地工作流边界 |

## 详细设计

| 文件 | 范围 |
|---|---|
| workflow.md | 编译后的阶段、重试、恢复、评审、取消 |
| storage.md | SQLite 投影与 CPGen 领域账本 |
| testing.md | 确定性、子进程、崩溃与 Docker 验收 |
| cli.md | 命令、退出码、重启行为 |
| configuration.md | 本地运行时、存储、提供方与沙箱取值 |
| llm.md | 提示词、严格结构化输出、记账、隐私 |
| similarity.md | 适配器、证据、缓存与决策策略 |
| sandbox.md | 直接 Docker 协议、看门狗、精确对账 |
| judge.md | checker 与判定契约 |
| package.md | 内部题包、门禁、验证、导出 |
| data-pipeline.md | 数据生成与差分验证 |
| idea-statement.md | 请求、idea 与题面模型 |

## 当前基线

Slice 0 已完成，其证据仍为权威来源。Slice 1 为每个 run 使用一个前台本地 CLI 执行器、由操作系统支撑的 run 锁、编译后的类型化阶段、短 SQLite 事务、CPGen 专用账本，以及保留的 Docker 看门狗。

已接受的设计是 docs/superpowers/specs/2026-08-31-lightweight-local-workflow-design.md。实施计划是 docs/superpowers/plans/2026-08-31-slice1-lightweight-local-workflow.md。

## 词汇表

- Run：一个不可变请求及其持久化的当前投影。
- 阶段：一次具名的编译后 CPGen 变换。
- 阶段尝试：当前阶段的一次物理执行。
- RunView：对某个阶段可见的不可变取值与只读剩余预算。
- CallTrace：物理外部调用的证据。
- Blob：不可变的按摘要寻址的字节。
- ArtifactOccurrence：Blob 在 run 作用域内的来源信息。
- SandboxExecution：持久化的 Docker 逻辑操作与完整资源计划。
- ReviewDecision：由后续恢复应用的人工不可变操作。
- PackageOccurrence：run 与已暂存或已验证题包之间的关系。
