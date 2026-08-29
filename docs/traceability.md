# 需求与设计追踪矩阵

## MVP 功能

| 需求 | 主要设计 | 实施切片 | 核心验收 |
|---|---|---|---|
| F01 接收出题请求 | [配置](./design/configuration.md)、[Idea/Statement](./design/idea-statement.md)、[CLI](./design/cli.md) | Slice 1/2 | manual/random 规则、effective seed、未知字段拒绝、有效配置快照 |
| F02 创意生成与选择 | [Idea/Statement](./design/idea-statement.md)、[工作流](./design/workflow.md)、[LLM/Prompt](./design/llm.md)、ADR-0001 | Slice 2 | 多候选、确定性排序/选择理由、预算变异、父链/provenance |
| F03 结构化题面 | [Idea/Statement](./design/idea-statement.md)、[LLM/Prompt](./design/llm.md)、[工作流](./design/workflow.md) | Slice 2/4 | 严格 Schema、revision/字段校验；Validator 就绪后执行正式 Sample Gate |
| F04 语义查重 | [Similarity](./design/similarity.md) | Slice 2 | Evidence/Decision 分离、缓存版本、失败关闭 |
| F05 解法生成 | [LLM/Prompt](./design/llm.md)、[Judge](./design/judge.md)、[Sandbox](./design/sandbox.md) | Slice 3/4 | 结构化代码候选、标程/暴力编译、Slice 3 sample smoke、Slice 4 正式样例 verdict、复杂度证据 |
| F06 数据工具 | [数据流水线](./design/data-pipeline.md)、[Sandbox](./design/sandbox.md)、[Judge](./design/judge.md) | Slice 4 | seed 派生、确定性 Generator、Validator 正负例、原子测试提升 |
| F07 自动判题 | ADR-0003、[Judge](./design/judge.md)、[数据流水线](./design/data-pipeline.md) | Slice 0/3/4 | Compile/Process/Judge 分层、Differential/Resource Gates |
| F08 质量门禁 | [Judge](./design/judge.md)、[工作流](./design/workflow.md) | Slice 4–5 | 不可豁免门禁、current revision evidence、VERIFICATION 不修改被验内容 |
| F09 题目包导出 | [题包](./design/package.md)、[存储](./design/storage.md) | Slice 0/5 | 内容身份/验证 occurrence 分离、原子发布、反向读取、Package Gate 先于 READY |
| F10 缓存与恢复 | [存储](./design/storage.md)、ADR-0002 | Slice 1 | 原子事务、ABANDONED、幂等和分层缓存 |
| F11 异常审核 | ADR-0002、[CLI](./design/cli.md) | Slice 1/5 | revise/retry/waive/reject、waiver 自动失效 |

## 非功能要求

| 要求 | 设计位置 | 验收 |
|---|---|---|
| Go-first | ADR-0001、[实施计划](./implementation-plan.md) | 静态 typed Step，无 `map[string]any` Registry |
| Docker-only | [Sandbox](./design/sandbox.md)、ADR-0004/0005 | 无 host fallback；逐 ContainerCreate 计量；daemon 不可用为 BLOCKED |
| 安全隔离 | [Sandbox](./design/sandbox.md)、[测试](./design/testing.md) | 禁网、非 root、精确 target cgroup、跨停止配额输出、watchdog、日志/提升攻击测试 |
| 可追溯 | [存储](./design/storage.md)、[题包](./design/package.md) | CallOperation/AttemptCall/CallTrace、Blob/Occurrence 分离、manifest/provenance |
| 成本控制 | [工作流](./design/workflow.md) | 并发预留、结算、UNKNOWN 保守计费 |
| LLM 定价与隐私 | [LLM/Prompt](./design/llm.md)、[配置](./design/configuration.md) | 固定 PricingPolicy、data-class allowlist、未授权零请求 |
| 可恢复 | ADR-0002、[存储](./design/storage.md) | checkpoint、原 step resume、crash injection |
| 外部服务可替换 | [Similarity](./design/similarity.md)、端口架构 | base_url/protocol，业务层不依赖供应商字段 |
| OJ 可交付 | [题包](./design/package.md) | internal package v1、固定 Polygon contract fixture |

## 架构评审 P2 落点

| 评审项 | 已落文档 |
|---|---|
| Workflow 依赖和失效 | [workflow.md](./design/workflow.md#4-依赖与失效规则) |
| BLOCKED/checkpoint/ABANDONED | [workflow.md](./design/workflow.md#6-blocked-恢复)、[storage.md](./design/storage.md#7-崩溃恢复) |
| Metered StepServices | [workflow.md](./design/workflow.md#7-metered-stepservices) |
| Prompt/Schema/LLM provenance | [llm.md](./design/llm.md) |
| TestPlan/seed/数据提升 | [data-pipeline.md](./design/data-pipeline.md) |
| SQLite 单写者事务 | [storage.md](./design/storage.md#5-原子提交顺序) |
| testlib role adapter | [judge.md](./design/judge.md#3-role-adapter) |
| Docker direct-run/cgroup/提升 | [sandbox.md](./design/sandbox.md)、[ADR-0004](./adr/0004-docker-direct-execution.md)、[ADR-0005](./adr/0005-docker-execution-lifecycle.md) |
| 缓存协议 | [storage.md](./design/storage.md#9-缓存规范) |
| Similarity 两级缓存 | [similarity.md](./design/similarity.md#7-evidence-cache) |
| ReviewDecision/revision | [workflow.md](./design/workflow.md#8-reviewdecision-应用) |
| Package/Export | [package.md](./design/package.md) |
| Slice 0 夹具 | [testing.md](./design/testing.md#2-slice-0-固定夹具) |
| CLI/E2E | [cli.md](./design/cli.md)、[testing.md](./design/testing.md#9-packagecli-测试) |
