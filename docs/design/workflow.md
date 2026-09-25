# 固定工作流、revision 与预算

状态：ADR-0006 下为当前设计

## 1. 范围

本设计定义具体的类型化 CPGen 流水线、持久化阶段边界、阶段内重试、人工恢复、复核、取消与下游失效。协调器是面向单台主机的前台 CLI 组件。

前向 MVP 从已提交的 Similarity ACCEPT 继续，依次经过 Solution/Data/Docker/Judge/Quality/Package。2026-09-15 的范围在 `mvp.idea.statement.similarity.solution.data.judge.package.v2` 中增加有界的内容重生成；V1 与历史 checkpoint 保留其原有的停止行为。传输重试、JSON 格式修复、依赖恢复与显式人工复核保持独立。通用 idea 变更仍暂缓。

当前示例选择 `ExecutedSamplesRevision`（V3），在 V2 的重生成配额与固定阶段顺序上增加执行样例定稿；报告、题包及恢复兼容矩阵见[执行样例设计](executed-samples.md)。`GenerationRevision` 是 MVP V1 的持久化身份，不代表当前默认示例。

## 2. 身份与 revision

run 持久化：

- RunID 与已校验的请求身份；
- WorkflowRevision 与 SchemaVersion；
- 规范请求与配置摘要；
- 当前 run 版本、状态、阶段名称与阶段序号；
- 活跃时间核算与最终题包指针；
- 来自注入的规范时钟的时间戳。

每条阶段记录持久化其类型化输入摘要、可选输出摘要、attempt 计数、当前 attempt ID、状态、最后一次类型化错误、逻辑幂等键与已提交证据引用。

编译后的构造器是权威来源。持久化的名称与序号是兼容性选择器和审计字段，不是用户定义的图。不可变的 `workflow.Definition` 是固定阶段顺序、题包完成与 attempt 保留的唯一来源。`NewGenerationRunService` 使用带显式属主资源的扁平 `GenerationRunConfig`；历史 Go 构造器辅助函数只存在于测试中。资源/准入/冻结策略检查在装配时运行，而 run/attempt/证据检查仍留在执行边界。

## 3. 具体类型化流水线

生成构造器装配以下业务阶段：

1. Idea
2. Statement
3. Similarity
4. Solution
5. Data
6. Judge
7. Quality
8. Package

当前 `GenerationRevision` 装配完整的普通题流水线，包括独立的验证边界与决策边界。`revisions.go` 隔离未变的持久化 revision 字符串与历史停止点。所有 revision 都使用应用调度器；不存在单独的历史流水线执行器。默认的确定性 Fake 流水线及其能力配置位于 `internal/adapter/fake`。

`internal/application` 中的本地固定循环一次推进一个阶段边界。它在选择下一阶段之前校验身份、阶段顺序与已提交版本，在调用前检查取消，并在暂停或终态结果时返回。对 V2/V3，它还接受显式编译的内容重试路由；持久化边界拥有资格判定与持久化重试上限。调度器不执行独立的 checkpoint 写入。当 BeginStage 或核算已经提交时，失败的边界可以返回更新后的同阶段投影；外来或回退的投影会被拒绝。

进度由兼容的编译工作流 revision、阶段/输入/配置/schema 绑定与经验证的已存储输出重建。SQLite 仍是权威来源；不使用 graph.json 存储，也不使用未经检查的自动 checkpoint 回调。调度器不在 run 之间共享调用状态。`LocalRunService` 在 run 锁下直接管理 attempt、结果提交、复核与恢复；其 stageControl 辅助函数负责 join poller。`fixedStages` 适配类型化业务输入/结果，并通过显式 switch 选择恢复。其恢复 switch 执行阶段准入并验证清理证明，然后把保留的物理调用结算委托给 `internal/adapter/sandbox`。模型与 Similarity 的重试/收据/缓存协议位于 `internal/execution`；它们不导入 application，也不推进工作流阶段。不存在恢复注册表或独立的生命周期/终止对象。Bootstrap/Application 在存储之前拥有并关闭执行资源。移除图库之后，现有的阶段序列、错误、取消、并发 run 与子进程恢复测试仍是行为契约。

阶段由具体的 Go 输入与输出值参数化。它接收不可变的 RunView、一份复制的输入，以及一小组受计量的端口。它绝不接收持久化、进程锁、裸 Docker、不受限的 Blob 写入或可变的协调器状态。

## 4. 状态模型

run 状态为 CREATED、RUNNING、BLOCKED、NEEDS_REVIEW、READY、FAILED 与 CANCELLED。

阶段状态为 PENDING、RUNNING、SUCCEEDED、BLOCKED、NEEDS_REVIEW、FAILED 与 CANCELLED。

阶段 attempt 状态为 RUNNING、SUCCEEDED、BLOCKED、NEEDS_REVIEW、FAILED、CANCELLED 与 INTERRUPTED。

只有当同一 run 的经验证题包 occurrence 与最终质量报告被原子绑定后，才能提交 READY。

## 5. 协调器协议

一条有状态的执行命令：

1. 获取确定性的按 run 进程锁；
2. 打开并迁移 SQLite；
3. 对该 run 未完成的精确沙箱资源进行对账；
4. 校验请求、配置、工作流 revision 与当前投影；
5. 在短事务中启动或回放当前阶段 attempt；
6. 预留所需预算与效果记录；
7. 在所有 SQLite 写事务之外调用阶段；
8. 验证返回的证据与制品；
9. 在短事务中结算账本、完成 attempt、更新阶段与 run 投影，并追加有序事件；
10. 推进到下一个已编译阶段，或在暂停或终态时退出。

每次转移都检查预期的 run 版本。回放已提交的转移会返回其存储结果，绝不重复事件或核算。

## 6. 重试

V2 每次 run 最多自动重生成内容 **两次**，该配额在阶段与进程重启之间共享。示例配置选择 V2；现有冻结的 V1 run 保持为 V1。这一初始策略从原始类型化输入重生成；尚不把编译器诊断或先前草稿回灌到 prompt 中。

| 失败 | 重生成起点 |
| --- | --- |
| 没有可行 idea；草稿/输入绑定被拒绝 | 当前草稿阶段 |
| 在配置的格式配额用尽后仍发生经验证的 JSON 格式拒绝 | 当前草稿阶段 |
| Solution 或 brute 编译失败 | Solution |
| Solution 样例失败（样例本身可能出错） | Statement，然后是 Similarity 及之后所有阶段 |
| Generator/validator 编译、执行、校验或可复现性失败 | Data |
| Judge 的 reference/brute 失败或差分不一致 | Solution，然后是 Data 及之后所有阶段 |

Similarity 决策、固定 checker 的 Quality 失败、provider HTTP 拒绝、未知发送边界、不支持的诊断与预算耗尽都不授权内容重生成。它们保留现有的复核/阻塞/错误行为。

`FinishContentRetry` 在同一个 SQLite 事务中完成失败的 attempt、插入不可变的 `content_retries` 证据，并使目标的整个下游后缀失效。它保留 attempt 序号、更早的制品、请求/配置绑定与所有预算账户。回放不能再次消耗配额。重生成的草稿 attempt 绕过缓存查找；已经派发的调用保留正常的持久化回放。READY 之前，每个下游验证都必须重新通过。两次配额用尽后，同一事务在普通且不可豁免的 NEEDS_REVIEW 门禁处结束。待处理的取消会阻止重生成。

重试是当前阶段及其带版本策略内的有界循环。每次物理 attempt 都有新的序号与持久化调用记录。逻辑操作身份与幂等键在多次 attempt 之间保持稳定。

重试在成功、阻塞条件、人工复核、永久失败、用户取消或阶段预算耗尽时停止。外部发送边界未知的请求绝不会以新键重新发送。适配器在支持时对账原始身份；否则保守地结算预留，并返回类型化的暂停或失败。

退避仅在前台命令运行期间生效。不存在后台计时器。未来的重试时间作为阻塞 checkpoint 的一部分持久化，并需要人工恢复。

## 7. 阻塞与人工恢复

BLOCKED checkpoint 包含：

- 阶段名称与类型化输入摘要；
- 依赖身份与策略摘要；
- 规范错误证据；
- retry-after 时间戳；
- 相关预算与 revision 绑定。

人工恢复重新获取 run 锁，并为同一阶段创建新的 attempt。其首个被授权的依赖操作使用普通计量端口与当前策略，重新校验 checkpoint 中的确切依赖。缓存的历史健康状态仅为诊断信息，本身不能确立恢复。

如果依赖仍不可用，新的 attempt 以 BLOCKED 结束并更新证据。如果依赖健康，同一 attempt 可以继续普通阶段工作。不引入单独的 run 模式或特殊调度器路径。

## 8. 进程重启

异常退出可能使 run、阶段与当前 attempt 仍记录为 RUNNING。操作系统会释放 run 锁。人工恢复时，协调器首先对账该 attempt：

- 如果没有授权任何不可逆效果，将其标记为 INTERRUPTED 并重跑；
- 如果 provider 支持稳定幂等键，则查询或回放该身份；
- 如果派发状态未知且无法查询，保守结算并暂停或失败；
- 如果 Blob 字节已发布，验证它们并附加或释放 writer token；
- 如果 Docker 工作已开始，在重跑之前结算持久化的 SandboxExecution；
- 如果已完成的结果与账本已经提交，则从权威来源投影推进。

只考虑当前阶段。恢复绝不选择任意图节点，也不改动无关的 run。

## 9. 复核

ReviewDecision 的种类为 REVISE、RETRY、WAIVE 与 REJECT。状态为 PENDING、APPLIED、REJECTED 与 STALE。

复核命令插入一条不可变的 PENDING 决策，携带 run 版本、工作流 revision、阶段输入、证据、策略、请求的修改与豁免范围。人工恢复恰好校验一个决策：

- REVISE 创建新 revision、使下游输出失效，并从最早受影响的已编译阶段重新开始；
- RETRY 为被复核的阶段创建新的 attempt；
- WAIVE 记录有界策略证据，并仅在策略允许处继续；
- REJECT 以复核证据将 run 结束为 FAILED。

过期或有歧义的决策绝不应用。

## 10. 取消

即使另一个进程持有 run 锁，取消命令也会插入一条幂等控制请求。前台协调器轮询该表并取消其根 context。

观察到取消之后：

- 不得授权新的调用、制品 writer 或沙箱启动；
- 已授权的操作会被结算；
- 每个不可信目标都被停止并证明已停止；
- 记录清理证据与预算结果；
- 待处理的复核决策变为过期；
- run 在一个短事务中提交为 CANCELLED。

如果没有执行器处于活跃状态，取消可以获取 run 锁，并在提交前执行同样的精确资源对账。

## 11. 下游失效

每个阶段都声明构成其输入摘要的输入组件与先前阶段输出。revision 变化会从编译后的定义计算最早受影响的阶段。该阶段及之后阶段的当前输出变为非当前，但不可变的先前 attempt、事件、Blob、occurrence、调用轨迹与复核决策仍可审计。

未变化的摘要只有在 schema、工作流 revision 兼容性、策略、来源与预算规则允许时，才可以复用经验证的已提交阶段输出。

## 12. 预算

协调器向每个阶段暴露包含只读剩余额度的 RunView。计量端口拥有模型调用、similarity 调用、Docker run、制品字节、token、成本与活跃时间的预留和结算。

阶段发布接收现有的已准入 attempt 与版本，以及制品声明和字节；发布适配器拥有物理调用、预留与 writer 身份。已提交证明的读取器使用只读响应策略与沙箱规划输入，不涉及传输或生命周期资源。这些能力边界不伴随任何数据库 revision、schema、规范编码或审计身份的变化。

阶段代码不能修改计数器。并行安全的账户更新使用数据库约束与预期账户版本。缓存结果仍会创建逻辑调用证据，并保留源调用与制品来源。

新增一个普通业务阶段需要其类型化实现、已提交证据读取器、新的兼容工作流定义，以及固定的执行/提交/恢复适配器。现有持久化定义必须保留其序列。计时器、取消、预算、writer 与通用终态清理状态机不增加业务阶段分支；只有新的效果协议才需要单独复核的适配器改动。

## 13. 验收

测试必须证明：

- 只有编译后的阶段顺序可以执行；
- RunView 与输入不能通过别名被修改；
- run 锁排除同一 run 的第二个执行器；
- 每个持久化边界都能在不产生重复效果的情况下重启；
- 重试保持逻辑身份并获得新的物理记录；
- 阻塞恢复执行当前策略的依赖检查；
- 复核与取消遵循上述规则；
- 外部 I/O 绝不与 SQLite 写事务重叠；
- 在同一 run 的题包验证之前不可能进入 READY。

## 契约短语（canonical contract phrases）

架构检查脚本以这些英文短语作为契约锚点：

- local fixed loop
- internal/application
- SQLite remains authoritative
