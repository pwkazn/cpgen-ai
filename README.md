# CP Problem Generator AI

一个使用 Go 编排的算法竞赛自动出题系统。它从用户需求或随机种子出发，生成题面、题解、标程、暴力程序、Validator、Generator 和测试数据，并通过 DockerSandbox、差分测试、Similarity Service 和质量门禁生成可追溯的 OJ 题目包。

## 当前状态

- 产品计划：已形成 MVP 范围。
- 系统架构：v1.0 已冻结，最新审查的 P0/P1/P2/P3 均已闭环。
- 详细设计：已覆盖工作流、LLM/Prompt、状态恢复、预算、存储、DockerSandbox、Judge、TestPlan/数据流水线、Similarity、配置、题包、CLI 和测试。
- 实现：Slice 0 已完成；显式本机 Docker 执行基座、固定镜像/toolchain、detached watchdog、A+B 纵向探针和结构包探针均已通过验证。逐项证据见 [Slice 0 验证记录](./docs/evidence/slice0-verification.md)。

## 核心技术决策

- 核心应用使用 Go，工作流静态类型化组合。
- 生成的 C++/Go 程序统一在一次性 Docker Linux 容器中编译运行。
- Sandbox 只报告编译和进程结果，Judge Harness 按角色解释 testlib 退出码。
- SQLite 保存任务、事件、预算和审计关系；Blob 按 SHA-256 内容寻址。
- Similarity Service 通过 `base_url + protocol` 配置，检索证据与门禁决策分离。
- 内部题包是事实来源，Polygon 是导出适配器。

## MVP 流程

```text
请求 -> 创意 -> 题面 -> 查重 -> 标程/暴力
     -> Generator/Validator -> 差分 -> 正式数据/答案
     -> 资源门禁 -> 内部包/导出 -> Package Gate -> READY
```

基础设施暂时不可用进入 `BLOCKED`；内容证据或预算需要人工决定进入 `NEEDS_REVIEW`。

## 文档

- [项目计划](./plan.md)
- [冻结架构](./ARCHITECTURE.md)
- [详细文档索引](./docs/README.md)
- [需求追踪矩阵](./docs/traceability.md)
- [MVP 实施计划](./docs/implementation-plan.md)
- [开发 TODO 与阶段目标](./TODO.md)

## 下一步

进入 [Slice 1](./docs/implementation-plan.md#3-slice-1可恢复骨架)：实现 SQLite 状态机、租约/围栏、预算/制品计量和崩溃恢复协议。
