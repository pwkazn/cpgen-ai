# ADR-0001：静态类型化工作流

- 状态：Accepted
- 日期：2026-08-29

## 背景

不同 Agent 的输入输出类型不同。Go 中的 `Step[IdeaInput, IdeaOutput]` 与 `Step[ProblemSpec, SolutionBundle]` 不是同一个接口实例，无法同时放进一个类型安全的运行时 Registry。用 `map[string]any` 会把错误推迟到运行时，也会诱导 Step 修改共享 `RunContext`。

## 决策

工作流在 Go 代码中静态组装，泛型只用于节点之间的编译期连接：

```go
type Step[I any, O any] interface {
	Name() StepName
	Run(context.Context, RunView, StepServices, I) (AgentResult[O], error)
}
```

- `RunView` 是不可变快照，只返回值或集合副本。
- `StepServices` 是按节点授权、带预算计量的端口代理。
- Step 通过 `AgentResult` 返回候选输出、`PendingArtifact` 和证据，不直接写数据库、ArtifactOccurrence 或任务状态。Adapter 只能通过 StepServices 注入的 run-scoped `MeteredArtifactSink` 幂等写入不可变 Blob，不能持有可绕过预算的裸写接口；occurrence 只能由 Orchestrator 提交。
- Orchestrator 是单个 run 的唯一提交者。
- Phase 2 的 Hack/SPJ 节点由静态 workflow builder 按配置启用，不通过字符串动态加载任意节点。
- LLM、Similarity、Sandbox、Storage 和 Exporter 仍使用运行时适配器；可插拔的是基础设施端口，不是无类型工作流。

示意组合：

```go
wf := NewMVPWorkflow(
	ideaStep,
	statementStep,
	similarityStep,
	Parallel(solutionStep, bruteStep),
	dataStep,
	judgeStep,
	packageStep,
)
```

## 后果

优点：

- 编译期发现相邻节点 Schema 不兼容。
- 并发 Step 不能共享可变任务状态。
- workflow revision 可以绑定到代码版本和构造器配置。

代价：

- 新增工作流节点需要编译发布新版本。
- 不支持第三方在运行时注入任意 Agent 插件；MVP 不需要该能力。

## 禁止做法

- `map[string]any` 保存异构 Step。
- 将可写 Repository、预算对象或 `*RunContext` 交给 Agent。
- 让 Step 在返回结果之前自行提交 ArtifactOccurrence 或状态事件。
