# CP Problem Generator 文档索引

## 文档层级

出现冲突时按下列顺序处理：

1. `plan.md`：产品目标和阶段范围。
2. `ARCHITECTURE.md`：已冻结的系统边界、质量原则和核心状态语义。
3. `docs/adr/`：不可静默修改的关键技术决策。
4. `docs/design/`：模块接口、持久化协议和验收设计。
5. 已发布 Schema、数据库迁移和代码：实现期的机器可执行事实来源。

实现发现架构级冲突时，先新增 ADR，再修改架构和详细设计；不得只修改代码绕过文档约束。

## ADR

- [ADR-0001：静态类型化工作流](./adr/0001-static-typed-workflow.md)
- [ADR-0002：任务状态、审核与恢复](./adr/0002-run-state-review.md)
- [ADR-0003：编译、进程和判题结果分层](./adr/0003-judge-outcomes.md)
- [ADR-0004：目标程序直接运行于独立 Docker cgroup](./adr/0004-docker-direct-execution.md)
- [ADR-0005：Docker 执行生命周期与跨停止制品传输](./adr/0005-docker-execution-lifecycle.md)

## 详细设计

- [工作流、revision 与预算](./design/workflow.md)
- [LLM、Prompt 与结构化输出](./design/llm.md)
- [Idea 与 Statement 契约](./design/idea-statement.md)
- [SQLite、制品与缓存](./design/storage.md)
- [DockerSandbox](./design/sandbox.md)
- [Judge Harness 与 testlib](./design/judge.md)
- [TestPlan、Generator 与数据流水线](./design/data-pipeline.md)
- [Similarity Service 与决策策略](./design/similarity.md)
- [配置模型](./design/configuration.md)
- [内部题包与导出](./design/package.md)
- [CLI 契约](./design/cli.md)
- [测试与验收](./design/testing.md)
- [实施计划](./implementation-plan.md)
- [开发 TODO 与阶段目标](../TODO.md)

## 统一术语

| 术语 | 含义 |
|---|---|
| Agent / Step | 调用 LLM 或确定性工具完成一个类型化工作流节点 |
| Orchestrator | 唯一能够提交任务状态、预算和制品引用的应用层组件 |
| Sandbox | 只负责 Docker 编译与进程执行，不产生 WA/PE 等业务判决 |
| Judge Harness | 解释 solution/validator/checker 结果并执行质量门禁 |
| Validator | 判断测试输入是否符合题目输入规范 |
| Checker / SPJ | 判断程序输出是否可接受；MVP 默认使用 exact/token checker |
| Blob | 按 SHA-256 去重的不可变字节内容 |
| ArtifactOccurrence | Blob 在某个 run/step/attempt 中产生或被复用的可审计关系 |
| CallOperation / AttemptCall / CallTrace | 一次逻辑端口调用、其一个或多个物理尝试，以及对二者/缓存来源的公共追踪投影 |
| RunRelation | `GENERATED/DERIVED/VERIFICATION`；决定内容是否可在当前 run 修订 |
| InputOrigin | `NONE/SAMPLE/GENERATED_TEST/IMPORTED_TEST/TRUSTED_FIXTURE`；Compile 用 `NONE`，可信 checker 自检用 `TRUSTED_FIXTURE`，参与 Validator/Checker 失败路由 |
| FailureRouteClass | `SANDBOX_INFRA/TRUSTED_TOOL_INFRASTRUCTURE/VERIFICATION_CONTENT/GENERATED_REPAIR`；只决定回退路径，不覆盖三层 Judge Outcome |
| SandboxExecution | 可恢复的 Docker logical operation 生命周期；`CLEANUP_PENDING` 时 run 保持 QUIESCING |
| `BLOCKED` | 外部运行条件暂时不满足，可在条件恢复后继续 |
| `NEEDS_REVIEW` | 需要人对内容、证据、预算或 waiver 作决定 |

## 当前基线

- 核心语言：Go。
- 不可信程序：统一在 Docker Linux 容器执行。
- MVP：非 SPJ 题、CLI、SQLite、本地制品存储、原题姬兼容 Similarity Service。
- 架构冻结状态：当前已知 P0/P1/P2/P3 评审项均已闭环；后续架构级变化必须新增 ADR。
