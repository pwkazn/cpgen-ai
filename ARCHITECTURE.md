# CP Problem Generator AI 架构

状态：当前

## 1. 产品边界
样例发布遵循版本化的[执行样例策略](docs/design/executed-samples.md)：不可变 Statement 草稿保留样例输入，Judge 独立核验后由单独的最终题面绑定执行答案。
当前 MVP 使用 workflow v3：v1 是历史固定流程，v2 增加有界内容重生成，v3 在同一预算与重试边界内加入独立执行样例定稿。报告 schema 为 `cpgen.solution-verification/v2` 与 `cpgen.judge-verification/v2`，题包 manifest 为 `cpgen.package/v3`；迁移 000028 只接纳新的版本身份，不改写旧 run。

CPGen 把结构化的竞赛编程请求转化为可审计的题包。生成模型负责提议候选；确定性代码、编译、执行、评测、查重策略与题包门禁决定这些候选是否可接受。

当前 MVP 范围（用户修订，2026-09-09）：有效的查重 ACCEPT 继续经过题解、数据、Docker/评测、质量与题包；未通过验收的业务结果进入人工评审。自动 Idea 变异与业务修复推迟到可用的正向循环之后再重新设计。既有的传输恢复、有界 JSON 格式修复，以及确定性题包/质量门禁仍然必需。[当前交付计划](docs/superpowers/plans/2026-09-09-mvp-generation-loop.md)取代了旧文档中以变异为先的顺序。

阶段 1 是一个模块化的本地应用：

- 一台主机和一个私有项目工作区；
- 每个 run 一个前台执行器；
- 确定性的每 run 进程锁；
- 一条编译进 Go 二进制的固定流水线；
- 用 SQLite 保存当前投影与审计记录；
- 私有的内容寻址制品存储；
- 由分离的看门狗守护的直接 Docker 执行；
- 没有工作流托管服务、守护进程、任务队列、远程 worker，也没有任意运行时图。

CLI 模式仍由前台 CLI 进程执行。`cpgen serve` 模式由一个前台 Go 服务进程承载多个任务 goroutine，并直接调用 application；服务管理器只控制并发容量、run 登记、独立 context 与关闭等待，不实现队列或恢复调度。不同 run 可以并发执行，同一个 run 永远不会有两个会修改状态的执行器。两种进程形态都通过 per-run OS 锁协调。

## 2. 架构原则

1. 结构化、带版本的领域模型是唯一事实来源。
2. 带类型的阶段输入与输出在编译期检查。
3. RunView 不可变，阶段代码只接收最小化的计量端口。
4. SQLite 写事务打开期间绝不发生外部 I/O。
5. 每个外部效应都有稳定的逻辑身份、物理 CallTrace，以及保守的预算结算。
6. Blob 不可变；occurrence 把字节绑定到 run、修订、角色与生产者证据。
7. 不可信程序只通过 Docker runner 与看门狗安全边界运行。
8. 人工评审是不可变的决策记录，而不是临时改状态。
9. READY 意味着同一个 run 原子地引用一个经过验证的题包 occurrence 和最终质量报告。
10. 本地协调器保持产品专用且小巧。

## 3. 系统上下文

用户通过 cpgen CLI 或 loopback-only 本地 Web 工作台交互。读取命令与 Web 读取使用本地存储装配，不涉及阶段执行器、provider 传输或 Docker 预检。Web 对已验证题包的下载复用应用读取边界。题包导出先获取共享的 run 锁，再获取共享的制品锁，用该 run 冻结的设置重建已提交的证明，并校验归档。新 run 会在其冻结配置中保留一份经过验证的 toolchain lock 快照。没有快照的旧 run 仍然需要原始的本地 lock 文件及其匹配摘要。

CLI 执行命令加载配置，打开并迁移 SQLite，获取 run 专属的执行锁与共享的制品使用锁，在需要时对账未完成的沙箱资源，执行一条命令，然后退出。`serve` 在启动时验证配置并打开本地存储，随后通过每任务 goroutine 直接调用 application；Web 不启动执行 CLI 子进程。取消服务接入后，已有任务收到独立 context 并在资源清理后汇合；重启不会自动恢复持久化的 RUNNING 任务。显式的制品维护获取全局排他制品锁，不获取任何每 run 锁，也绝不执行 run 阶段。它目前是一个 application API，而不是对外暴露的 CLI 子命令。

外部依赖仅限于显式配置的模型 provider、查重 provider，以及本地 Docker Engine。adapter 把 provider 结果归一化为带类型的领域结果。普通测试使用确定性的 Fake adapter；真实服务冒烟测试是可选的。

## 4. 组件

### 4.1 CLI

CLI 校验配置与请求，调用 application 服务，流式输出稳定的进度事件，渲染人类可读或 JSON 输出，并把带类型的错误映射为稳定的退出码。它绝不直接修改数据库行。

### 4.2 application 协调器

application 使用一条固定循环、一个 run 协调器和显式的业务阶段：

- Bootstrap/Application 构造存储、provider 与 Docker，并负责资源关闭。读取命令使用 BootstrapLocal，不带执行资源。
- LocalRunService 负责 run 锁、尝试、开始/结束、取消、评审与恢复。其 stageControl 辅助组件负责汇合的记账/取消轮询器。运行时状态不会被复制进独立的生活周期、终止或恢复对象。
- `fixedStages` 包含带类型的输入/结果分派和显式的恢复分支。业务执行器使用各自的 Reader；它们不包含上游执行器。每个 Reader 的方法与证明校验放在一起。
- 只读的模型与沙箱策略与传输保持分离。制品发布接收已存在的、已准入的 attempt 与 run 版本，然后在内部使用未改动的账本。

代码包边界遵循工作的归属：

- `internal/application` 负责业务阶段、run 协调与组装。阶段报告与其 validator 放在一起；草稿配置与阶段执行不会被拆进一次性使用的文件。
- `internal/execution` 负责计量式 LLM/查重调用、有界重试、回执、缓存复用，以及绑定 run 的调用账本。它不导入 application，也不推进工作流。其 store 契约读取当前 run 与 attempt，但不能创建或结束工作流阶段；缓存的写入在需要处单独提供。
- `internal/adapter/sandbox` 负责 Docker 会话、制品发布，以及已保留沙箱调用的结算。application 的准入与清理证明检查先于该结算。它依赖持久的调用协议，绝不依赖阶段执行器内部实现。
- `internal/artifact` 负责已准备的 writer 会话、有界校验读取与显式垃圾回收。调用方直接使用它；application 中不含兼容别名或转发构造函数。

`GenerationRunConfig` 只提供一次 owner 资源。组装在直接构造阶段结构体之前，先校验共享存储、准入、时钟、锁与冻结策略。历史构造函数辅助只存在于测试中；不可变的 `workflow.Definition` 保留已持久化的修订与阶段序列，且不升级旧 run。

已提交的 reader 继续检查 run/attempt 来源、发布状态与 Blob 摘要。题包完成使用独立的 `FinalizeVerifiedPackage` 事务；READY 不能先于已验证的 occurrence。RunView、BudgetSnapshot 与 CallTrace 始终是派生视图。[返工记录](docs/evidence/architecture-follow-up-2026-09-14.md)记录了被移除的抽象与校验。

它不实现通用调度、重放、定时器或分布式归属。

在 2026-09-13 的 ADR-0006 修订之下，`internal/application` 中的本地固定循环取代了 LangGraphGo 封装。SQLite 仍是权威来源：循环在推进之前必须检查阶段/证据提交，恢复则读取既有投影与已验证的 occurrence。自动图检查点和并行的 graph.json 存储被排除。LangChainGo 保留在 `internal/agent`；provider 库类型绝不进入 domain、port 或阶段代码。显式配置选择 MVP；默认 Fake 与历史 preview 修订保留其既有边界。

### 4.3 带类型的阶段

具体构造函数组装带版本的 Idea、题面、查重、题解、数据、评测、质量与题包阶段。Slice 1 提供了一条具有相同契约的确定性 Fake 流水线。

每个阶段接收一份拷贝的输入值、不可变的 RunView、规范时钟值，以及仅限其被授权的端口。它不能接收 repository、裸 Docker 客户端、可变 run 状态、不受限的制品 writer 或锁管理器。

### 4.4 持久化与制品存储

SQLite 保存短事务投影和 CPGen 专用的账本。私有制品存储通过声明、writer token、校验、pin 与 occurrence 协议写入不可变的 SHA-256 寻址 Blob。题包暂存使用私有目录与原子发布。

### 4.5 Docker runner 与看门狗

受信的主机 runner 在 Docker create 之前持久化 SandboxExecution 及其完整计划资源。确定性的名称、label、计划摘要、engine 身份与看门狗控制证据只授权恰好那些资源。

分离的看门狗在启动之前接收封装好的计划，并在超期或进程丢失时停止计划中的目标。之后的某条命令会运行一个范围很窄的 reconciler，它只能检查、停止、杀死、等待、移除并结算那些已持久化的资源。它不能继续阶段工作，也不能发布制品。

## 5. 领域契约

关键的不可变值包括：

- RunID、StageName、AttemptID、LogicalOperationID、CallID、SandboxExecutionID；
- WorkflowRevision、SchemaVersion、ConfigDigest、InputDigest、OutputDigest；
- RunView 与带类型的阶段输入/输出值；
- BudgetAccount、预留、MeteredOutcome 与 CallTrace；
- BlobRef、ArtifactDeclaration、WriterToken、ArtifactOccurrence 与 provenance；
- SandboxPlan、资源身份、看门狗证据、ProcessOutcome 与评测裁决；
- ReviewDecision、waiver 绑定、题包 occurrence、校验回执与质量报告。

所有标识符在被用于路径、label 或查询之前都会经过校验。规范编码与摘要都带版本。

## 6. 固定工作流

权威的阶段顺序与尝试恢复策略编译在 `internal/workflow/definition.go` 中。Definition 的访问器会拷贝阶段切片；调用方无法改动该顺序。持久化的阶段名、序号、工作流修订与 schema 版本共同选出一个兼容的二进制定义；数据库行不定义图的边。

封闭的 run 状态集合为：

- CREATED
- RUNNING
- BLOCKED
- NEEDS_REVIEW
- READY
- FAILED
- CANCELLED

阶段使用 PENDING、RUNNING、SUCCEEDED、BLOCKED、NEEDS_REVIEW、FAILED 或 CANCELLED。其只追加的 attempt 以 SUCCEEDED、BLOCKED、NEEDS_REVIEW、FAILED、CANCELLED 或 INTERRUPTED 结束。

MVP 只能通过经过验证的题包事务到达 READY。历史 preview 与 checkpoint 修订不会产生 READY。

## 7. 每 run 加锁与事务协议

锁路径由经过校验的 RunID 派生，位于私有运行时目录之下。CLI 进程退出时（包括异常终止）由操作系统释放该锁。只读的 show 与 events 命令不需要它。当另一个进程持有锁时，cancel 可以插入一条幂等的控制请求。

SQLite 仍然使用期望 run 版本比较。对于一个阶段尝试：

1. 获取 run 锁；
2. 打开并迁移 SQLite；
3. 对账该 run 未完成的沙箱工作；
4. 加载不可变的请求、配置、投影与已编译修订；
5. 在一个短事务中创建或重放阶段尝试与预留；
6. 在写事务之外执行网络、Docker、哈希、文件系统与已验证的 Blob 读取；
7. 校验返回的证据；
8. 在一个短事务中结算预算与效应、附加 occurrence、结束尝试、更新投影并追加事件；
9. 继续下一个已编译阶段，或退出。

写事务内部不发生哈希、fsync、rename、provider 调用、Docker 调用、看门狗 IPC 或阻塞等待。

## 8. 持久化模型

工作流投影表为：

- runs
- stage_records
- stage_attempts
- run_events
- control_requests
- review_decisions

CPGen 领域账本是独立的：

- budget_accounts 与 call_records；
- 制品声明、writer token、Blob、pin 与 occurrence；
- 带源调用与制品引用的缓存条目；
- 变异声明与来源；
- sandbox_executions 与 sandbox_resources；
- 题包、package_occurrences、校验回执与质量报告。

run 事件提供只追加审计，但不会被重放以重建控制流。关系约束防止跨 run 的来源、预算、制品与题包污染。

## 9. 重试、暂停、评审与取消

重试在活动的领域阶段内有界进行。每次物理尝试都有新的序号和调用记录；逻辑幂等键保持稳定。未知的发送边界必须尽可能对账原始的 provider 或 Docker 身份。否则系统保守计费，并返回带类型的暂停或失败。

BLOCKED 保存阶段输入摘要、依赖身份、策略摘要、错误证据与 retry-after 时间。手动恢复会为同一阶段启动一次全新的尝试。该尝试在开展正常工作之前，先通过其常规的计量端口重新校验依赖。历史健康数据仅用于诊断。

评审命令创建一条 PENDING 决策。手动恢复在一个短事务中校验并应用匹配的 REVISE、RETRY、WAIVE 或 REJECT 决策。

取消会插入一条幂等的控制请求。活动的命令轮询它，取消根 context，停止新的授权，并结算在途效应。只有在每个不可信目标都被证明已停止之后，才会提交 CANCELLED。

## 10. 重启与崩溃恢复

进程可能在 run 投影与当前 attempt 仍显示 RUNNING 时退出。下一次手动恢复会获取已被释放的操作系统锁，并遵循阶段特定规则：

- 未授权任何外部效应时重跑；
- 重放或对账同一个稳定的 provider 身份；
- 保守结算未知边界；
- 在附加或释放 writer token 之前，校验任何已发布的 Blob；
- 使用已持久化的 SandboxExecution 与精确的资源身份来清理 Docker 工作；
- 若结果与账本已提交，则正常开始下一阶段。

恢复由作用于领域账本之上的幂等命名操作组成。它绝不扫描无关的 run，也不发明通用的工作流动作。

对于首个 `solution_verify` create 在任何 SandboxExecution 存在之前就失败的情况，手动恢复可以中断旧尝试并开始一次普通的新校验尝试。SQLite 在中断事务中检查这一点：所有调用都是终态的沙箱/本地发布调用，每个 Docker 物理调用都是 `ABORTED_NO_DISPATCH` 且没有 dispatch 开始时间戳，并且预留都已结算。已有的执行记录（包括 CLEANED）、已发送或未知的调用，以及未结算的证据仍走原本的恢复路径。原始调用身份、证据与已消耗的预算保持完好；新尝试使用未改动的已提交题解输入，以及正常的准入、资源授权与 READY 门禁。参见[恢复证据](docs/evidence/sandbox-unsent-recovery-2026-09-15.md)。

## 11. 预算与来源

预算维度包括模型调用、token 与成本、查重调用、沙箱运行、制品字节数、阶段尝试次数，以及可选的活跃墙钟时间。预留先于不可逆工作；最终结算单调且可审计。

活跃时间记账可能持久化 active_elapsed_ns、active_started_at 与 last_accounting_heartbeat_at。该心跳仅用于计量。崩溃之后，计费被保守地限制在最后一次记账时间戳加上一个配置间隔与 run 截止时间之内。

每一次物理外部调用都记录 provider、请求摘要、策略、时序、结果分类、幂等身份与 CallTrace。缓存命中保留源调用与制品来源，并按其对逻辑效应计费。

## 12. 制品、缓存与题包

制品在写入之前先声明。字节在强制校验哈希与大小限制的同时流式写入私有临时文件，然后原子地提升为其规范的 Blob 身份。在需要高信任的边界上，已验证的读取会在使用前重新计算哈希。

occurrence 把已定稿的 writer token 与 Blob 关联到产生它的 run、阶段尝试、角色、修订与源调用。缓存命中会创建新的 run 作用域来源，而不是返回一个裸 Blob 引用。

垃圾回收是一条显式的维护命令。有状态的工作流命令持有共享制品锁；回收则取排他形式，重新检查数据库引用，并通过私有 trash 移动字节。

题包创建只暂存已声明的文件。结构与语义门禁在发布之前运行。READY 与经过验证的题包 occurrence 在同一个事务中提交。

## 13. 安全

- 私有的运行时、数据库、锁、制品与暂存路径；
- 严格的路径归一化，锁路径不可由用户选择；
- 配置快照、日志、制品、Docker label 或题包中不含机密；
- provider 允许列表、HTTPS、默认禁用重定向、响应大小上限与超时；
- Docker profile 采用非 root 身份、丢弃 capabilities、只读根文件系统、显式挂载、pids/memory/CPU 限制，且不暴露 socket；
- 清理之前进行精确的资源身份检查；
- 确定性错误，不泄漏敏感内容。

## 14. 测试与发布门禁

Slice 1 证明：

- 所有 run 状态转换与固定阶段转换；
- 编译期的类型边界与拷贝的 RunView 值；
- 双进程锁互斥与进程死亡后的自动释放；
- 期望版本冲突，以及投影与事件的原子提交；
- 每个持久化阶段边界上的重启；
- 有界重试、稳定身份、未知边界处理、预算、评审与取消；
- Blob 路径穿越、损坏、去重与崩溃行为；
- 看门狗死亡、超期与精确资源对账；
- SQLite 写事务期间不发生外部 I/O。

发布门禁包括完整的 Go 测试、vet、race 测试、Go 1.25.0 兼容性、Linux 交叉构建、在可用时需要的 Docker 安全测试，以及架构一致性脚本。go.mod 固定 LangChainGo v0.1.14；固定调度不依赖任何图库。CI 会测试最低 Go 版本与当前稳定工具链。

## 15. 交付切片

- Slice 0：已完成的执行探针、直接 Docker runner、看门狗、评测基础与证据。
- Slice 1 lightweight local workflow：本地生命周期值、每 run 锁、SQLite 投影、领域账本、Blob 存储、Fake 流水线、CLI 与崩溃测试。
- Slice 2：请求、idea、题面、模型集成与查重。
- Slice 3：题解生成与 Docker 评测集成。
- Slice 4：数据生成、差分校验与质量门禁。
- Slice 5：题包组装、导出、校验与端到端验收。

本架构刻意保持 Slice 0 安全证据完好，同时让 Slice 1 与单主机前台产品相称。

## 契约短语（canonical contract phrases）

架构检查脚本以这些英文短语作为契约锚点，实现与评审均以它们为准。

- one foreground executor per run
- per-run process lock
- fixed pipeline
- no workflow-hosting service
- local fixed loop
- LangChainGo
- SQLite remains authoritative
- Go 1.25.0
