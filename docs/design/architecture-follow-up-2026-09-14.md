# 架构问题复核与后续收敛方案

日期：2026-09-14。**本方案的第一版实现因增加过多抽象而被用户退回，以下内容仅作历史分析，不再作为当前实施要求。** 当前已改为合并协调状态、删除镜像接口与重复配置，见 [返工记录](../evidence/architecture-follow-up-2026-09-14.md)。完整工具链快照和专用 publication 账本仍是单独决策。

本次开始时，工作区已有大量未提交的代码和文档改动，包括 [2026-09-13 收敛计划](../superpowers/plans/2026-09-13-architecture-simplification.md) 及其 [实施记录](../evidence/architecture-simplification-baseline.md)。以下结论针对包含这些改动的工作区，不把它们视为本次新增，也不把旧代码的问题直接套到当前实现上。

**建议继续采用本地串行调度和 SQLite 持久化，下一轮重点完成能力边界与装配边界，暂不重写审计账本。** 已有重构方向成立，但“去掉执行器指针”和“换一个生产构造函数名”还不足以完成模块解耦。

## 五个问题的实施前状态

| 问题 | 探索时的代码证据 | 判断与后续方向 |
|---|---|---|
| 框架与自研工作流重叠 | `compiled_graph.go:42` 已是本地循环；`go.mod`、`go.sum` 已无 LangGraph，仍保留 LangChainGo | 主要问题已解决。保留现有提交、身份与后继校验；不再引入另一个调度引擎 |
| 执行器依赖链 | Package/Quality 已使用 Reader、Admission、Publisher；但 `PackageReader → QualityReader → DataReader → SolutionReader` 仍依赖具体类型，Solution/Data 构造器仍接收上游执行器 | 已解除执行对象的部分耦合，能力和构造边界仍需收敛 |
| 里程碑成为生产概念 | `NewMVPRunService` 已被生产 Bootstrap 调用；但 `MVPRunServiceConfig = Slice2RunServiceConfig`，实际装配仍从 Generation 内部取得存储、锁和时钟 | 入口名称改善，当前流程与兼容装配尚未真正独立 |
| 总协调器过大 | 已有不持有整个服务指针的 `stageControl`、`runRecoveryHandler`、`runReviewHandler`；但 `LocalRunService` 仍持有各执行器、Docker 配置、关闭函数，以及输入、结果、提交分派 | 生命周期拆分有实际效果，下一步应移交职责及其依赖，而不是继续搬方法到新文件 |
| 审计接线成本高 | 已有 `CallCoordinator`、`DraftExecution`、发布 session；但本地发布仍通过 `RunBoundLLMLedger`，Package 使用 sandbox 身份及 `CallSandboxCompile` | 先让阶段只看到所需能力；账本是否可减少，按故障边界逐项论证 |

主要代码入口：[调度](../../internal/application/compiled_graph.go)、[执行服务](../../internal/application/run_service.go)、[当前装配](../../internal/application/generation_run_service.go)、[历史适配](../../internal/application/legacy_executor_helpers_test.go)、[Package](../../internal/application/package_executor.go)、[读取链](../../internal/application/package_reader.go)。

以下第 1–5 节保留实施前的问题分析和设计约束，其中“当前”指探索时工作区。完成情况以实施记录为准。

## 1. 固定调度：保留现状，收敛定义来源

三种选择的取舍：

| 选择 | 收益 | 成本 | 本项目建议 |
|---|---|---|---|
| 保留当前本地循环 | 与固定串行流程匹配；复用既有 SQLite 原子提交和恢复 | 仍需维护少量转移校验 | 采用 |
| 把重试、checkpoint、恢复交给框架 | 将来动态分支、多执行者场景可能有收益 | 必须重新确定框架状态与业务账本的提交一致性；无法自动替代预算、Blob、Docker 证明 | 当前没有足够需求支持这次重写 |
| 另做通用阶段注册、事件总线或可配置 DAG | 扩展形式统一 | 引入当前固定流程不需要的新概念 | 不采用 |

当前循环仍需保留四项契约：SQLite 投影是进度依据；每次推进有已提交的新版本；成功只能进入该 revision 的精确后继；失败返回最后有效投影，不能触发隐式重试。`validateSnapshot`、`validateIdentity`、`validateTransition` 并非框架残留，应继续存在。

可以将 `compiledRunGraph` / `compiled_graph.go` 机械改名为 `runScheduler` / `run_scheduler.go`，但优先级低于实际边界改造。不要借改名改变数据库中的 revision、阶段名称、schema 或 digest 算法。

后续将固定阶段序列、结束边界和恢复策略归入一处不可变流程定义。请求创建、调度与恢复使用同一份定义；不要继续用 `generation != nil` 推断流程种类。定义仍由代码显式选择，不接收用户自定义阶段序列。

## 2. 阶段依赖：从具体 Reader 进一步收敛到所需能力

现有 Reader 之间的业务证明依赖有理由保留：Package 确实要检查 Quality，Quality 确实要检查 Judge。目标不是抹平证明链，而是让使用方只依赖可验证的结果，不知道读取方的字段布局、执行能力或资源来源。

仍可定位到的耦合：

- `package_assembly_reader.go:16` 的 PackageReader 持有 `*QualityReader`；`quality_content_reader.go:9` 的 QualityReader 持有 `*DataReader` 和完整 `DockerSandboxConfig`。
- `package_executor.go:67` 和 QualityExecutor 匿名嵌入具体 Reader，方法和内部字段被提升；执行与读取的边界在类型上仍不够清楚。
- `NewSolutionExecutor` 接收 SimilarityExecutor 和 GenerationExecutor；`NewDataExecutor` 从 Solution 的 drafts、store、reader 取得依赖。构造单独的阶段仍需先造上游执行器。
- `RunEvidenceReadStore` 声明少量方法，但 `package_reader.go:19`、`quality_reader.go:17` 又断言额外接口。构造成功不代表具备全部读取能力。
- `CommittedPackageReader` 为读取构造 `StructuredLLMCalls`，依靠 `readOnlyLLM.GeneratePhysical` 返回错误阻止误发送；已有防护有效，但读取对象仍承载不需要的执行方法。

推荐先完成 Package/Quality 一个纵向切片，再推广到 Solution/Data：

1. Package 注入命名字段的证据读取能力、阶段准入能力、发布能力；不匿名嵌入 Reader。证据读取边界可以用 `Load(ctx, runID)` 一次返回本阶段所需的 QualityInput、QualityReport、SimilarityContent 及绑定信息。其实现复用现有的完整核验。
2. Quality 只声明 Judge 输入及报告的读取方法，Data 只声明已验证 Solution 的读取方法。使用方无需要求一个完整上游 Reader，更无需上游 Executor。
3. 为 reader 声明包含所有实际读取方法的命名接口，把“存储缺少方法”的错误提前到装配或编译时。不要把 RuntimeStore 或 RunLLMStore 全部塞入这些接口。
4. 从 `StructuredLLMCalls.ReadCommitted` 提取真正只读的证明核验对象；生产调用与离线读取共用身份、摘要和响应验证函数。只读构造不再需要实现 `GeneratePhysical` 的替身。
5. 将 sandbox 的计划重建输入与 Engine/Watchdog 分开。Reader 接收冻结工具链、引擎身份摘要、协议和限制等证明策略；运行器才持有可调用的 Engine 和生命周期资源。
6. 给 Solution/Data 提供能力配置构造器，在 Bootstrap 显式装配共享的存储、策略、准入和调用能力。不要仅删除原有“同一 StageAdmission”检查：新的装配仍须保证 store、run、attempt、冻结策略的一致绑定。

接口只用于真实的替换与权限边界，不要求给每个结构体配一份同形接口。`PackageInputs` 这类返回值可先使用应用层对象，避免修改已持久化 JSON。拿到 DTO 也不等于来源合法；真实 reader 必须保留错 run、错 attempt、未提交、错误摘要和损坏 Blob 的拒绝行为。

重复验证的性能目前没有测量数据。先统一一次读取的入口；若测量确认重复重建显著，再使用持有 run/artifact 读锁期间的局部缓存。缓存键至少绑定 run、已提交版本及证明摘要，不能用仅按 runID 缓存的长期对象代替来源校验。

## 3. 当前流程与历史兼容：分开运行入口，保留数据身份

当前 `NewMVPRunService` 并非继续调用 `NewSlice2RunService`，这一点已经改善。但它们仍调用 `newCompiledRunService(Slice2RunServiceConfig)`，后者从 `config.Generation.config` 取出基础设施，并根据 revision 增量装配后续阶段。因此只看 MVP 构造，仍需理解历史装配。

建议形成三个显式入口：当前普通题流程、历史 revision 适配、Fake 测试/演示流程。当前入口使用独立配置类型；共用的是基础资源装配函数和稳定能力，不再共用带开发切片名称的生产配置。

历史适配集中负责：原始 request 解码、固定阶段序列、checkpoint 终点、同 attempt 恢复或中断重建策略、旧 Go 构造入口转发。业务阶段不需要知道“自己是第几次开发切片”；保留必要的 schema/策略适用性检查即可。

| 持久化 revision | 后续兼容策略 |
|---|---|
| `slice1.fake.v1` | 保留 prepare/exercise/checkpoint 语义与 Fake 入口 |
| `slice2.idea.statement.similarity.v1` | 保留代码支持的旧序列；不因此新增当前 CLI 配置选项 |
| `slice2.idea.statement.similarity.checkpoint.v1` | 保留 similarity 后的非豁免 preview checkpoint |
| `slice3.idea.statement.similarity.solution.checkpoint.v1` | 保留 solution 验证后的历史 checkpoint |
| `mvp.idea.statement.similarity.solution.data.judge.package.v1` | 当前完整普通题流程；只有 package 原子提交可以产生 READY |

这里区分“二进制中有定义”“当前配置允许新建”“能够读取”“满足配置及资源前提时可恢复”。不能从调度器支持旧序列推断 CLI 可以无条件恢复任意历史 run。当前恢复会检查 revision 和配置 digest，这一拒绝边界必须保留。

旧 Go 符号是否继续保留，按实际调用者决定；`internal` 包中的构造器无需仅为想象中的外部用户永久保留。数据库 revision 则是已有数据契约，不能随 Go 名称一起替换。不要把旧 run 自动升级为当前流程。

## 4. 总协调器：沿阶段事务边界移交职责

现有 `stageControl` 确实拥有计时、取消 poller 和 join；恢复、复核组件也没有回指整个服务，这部分应保留。剩余问题主要在 `executeStage`、`runStage`、`finishExecution` 和终态清理之间。

建议的职责归属如下，先在 application 内完成，不以拆出新 package 为验收条件：

| 组件 | 拥有什么 | 不需要持有什么 |
|---|---|---|
| Bootstrap/Application | SQLite、锁管理器、Docker 工厂及 Close 所有权；校验完整装配 | 业务阶段推进状态 |
| RunService | Generate/Resume/Cancel 用例、run 锁与 artifact 锁的顺序、调用固定调度和恢复 | 每个业务 Executor 字段、Docker 配置细节 |
| 固定阶段分派 | 各阶段的输入读取、typed 执行适配和结果绑定 | 后台计时、资源关闭、通用注册能力 |
| 阶段生命周期 | Begin/Resume attempt、RunView、现有 stageControl、结果提交；使用窄的清理能力 | 全部历史请求解释器、具体 Docker Engine |
| 恢复/终态清理 | 已有调用和发布身份的结算、精确 sandbox 清理、恢复计时 | 新业务工作规划和新发送授权 |
| 已有 Review handler | 读取并应用绑定当前版本/证据的持久化复核决定 | 模型与 Docker 执行器 |

实施时把现有 `stageExecution` 作为收敛起点。阶段 adapter 负责把自己的 typed 结果转换为提交材料，生命周期组件处理共同的开始、控制与完成协议。可以保留显式 switch；不要为了删除 switch 引入反射或动态插件。

普通完成与 Package 完成保留不同的事务命令。`finishExecution` 当前使用 `FinalizeVerifiedPackage` 同时绑定质量摘要、package occurrence 和 READY；不能抽成“先发布包、再 SetReady”的两步通用接口。缓存索引仍属于提交后的可选操作，不得让缓存失败撤销或掩盖已提交阶段。

取消也不能被简化成写一个 CANCELLED：先持久化请求，执行所有者停止工作并 join poller，结算调用/计时并取得清理证明，最后提交终态。若以后拆出不需要 Docker 的取消请求写入入口，应另行明确它返回“请求已记录”还是“已取消”，不能静默改变现有 CLI 语义。

验收要看新增一个普通业务阶段是否只需修改阶段实现、证据读取、固定流程定义和 typed 适配，而无需更改取消、计时、恢复、预算与 writer 状态机。顶层仍可保留少量协调代码，不以字段数或文件数决定完成与否。

## 5. 审计：区分持久化事实、原子投影和读取视图

这些名词处在不同层次，不能按“看起来重复”合并。尤其 logical operation 并非必然对应一张额外表：当前 `CallRecord` 中就有 `LogicalOperationID`，分别表达业务槽位和持久化记录身份。

| 概念 | 必须保留的信息与故障用途 | 所有者/建议 |
|---|---|---|
| stage attempt | 当前阶段哪次尝试被授权、输入绑定、是否已提交；服务重启可继续同一次 attempt，不能等同于进程启动次数 | RuntimeStore 原子开始/完成，生命周期及恢复组件使用 |
| logical operation / CallRecord | 一次业务操作的请求、策略、幂等范围、结果选择或缓存来源；一次 attempt 可有多个操作 | 调用能力内部创建；业务声明稳定 operation key，不手工组装全部 ID |
| physical call | 某次被计划的物理尝试，以及授权、发送、成功、失败或未知边界；记录存在不代表已经发生 HTTP 请求 | CallCoordinator/适配器推进，receipt/reconciler 恢复；已知可重试失败与未知发送分开 |
| reservation | 副作用前已授权的预算上界和后续结算/释放；不能从成功结果推断尚未结算的授权 | 账本事务管理；不因本地串行而删除，进程崩溃仍留下未完成授权 |
| writer token | 发布者、声明、封存状态、Blob 绑定和临时 pin；跨 SQLite 与文件系统的发布窗口 | artifact session 管理；业务只返回 pending publication |
| receipt | 当前私有 receipt 保存响应/结果及其请求与物理身份绑定，支持“结果已封存但账本尚未完成”的回放；并非天然等同于供应商签发的收据 | provider/publication 适配器创建，恢复和只读核验消费；摘要不能代替内容 |
| Blob / digest | Blob 是内容；digest 用于比较内容、策略与请求身份 | digest 可由原始字节重算，但持久化的期望 digest 是不可变绑定，不能一并删掉 |
| occurrence | 某 run/attempt 以什么角色、路径、来源提交引用了某 Blob；相同内容可以有多个合法使用关系 | 阶段提交创建；不能仅按 Blob digest 推导授权来源 |
| pin / cleanup proof | 临时内容的存活保护及外部资源已清理的事实 | 发布/GC 与 sandbox 生命周期拥有；不从“阶段完成”反推资源已清理 |
| run projection / budget account | 当前实现使用版本化投影执行 CAS 和预算准入，是原子事务中的运行权威 | 即使部分聚合理论上可重算，本轮仍保留；不能假设现有 events 足以重建全部状态 |
| RunView / BudgetSnapshot / CallTrace | 面向读取或阶段执行的组合结果、剩余额度与调用轨迹 | 从已提交记录构造，不新增独立可变账本；已嵌入历史 receipt 的序列化内容保持兼容 |
| cache index | 已提交结果的可选查找加速 | 保留失败不影响阶段完成的语义；索引可重建与来源证明可删除是两回事 |

一个已存在的崩溃用例说明为什么不能只存一个 `operation.status`：

1. 模型请求已经发生，响应尚未封存就退出：本地没有可信结果，恢复按未知边界处理，不重新发送。
2. 响应已封存，物理调用尚未完成：从原 receipt 结算并回放，不产生第二次请求。
3. 结果已完成但阶段未提交：继续原 attempt 的结果验证与 occurrence 提交。
4. 阶段已提交：从持久化后继继续，不能因缓存发布或进程退出重复前一阶段。

这几种窗口必须可以区分。现有 `TestLLMReplayProcessCrashBoundaries` 覆盖 before-seal、sealed、finalized、sent、completed、finished，并检查恢复后 HTTP 次数仍为 1。可靠性目标包含“无可信结果时停下”，不承诺跨任意外部服务的全局 exactly-once。

**最有价值的减法是缩小业务可见协议。** 建议在现有实现上分别收敛受控模型调用、受控 sandbox 执行、阶段产物发布三种能力。它们内部继续共用已经验证的账本协议；阶段只提供业务请求、稳定操作键、声明和结果，不自行创建 reservation、physical ID、writer token 或恢复分支。

一个具体试点是 Package 发布：当前 `packagePublicationIdentity` 构造 `SandboxAuthorizationIdentity` 并设置 `CallSandboxCompile`；`SandboxArtifactSink` 又通过 `RunBoundLLMLedger` 记录 `PhysicalLocalArtifactWrite`。这证明命名和公开能力混合了不同用途，不证明账本字段可以立即删除。

先引入面向阶段的 publication scope/工厂，在内部适配现有身份与调用类型，保持 canonical JSON、摘要和恢复身份不变。随后再评估是否需要真正独立的 publication 账本。后者涉及预算预留、声明外键、崩溃恢复、GC 和旧记录读取，应单列迁移设计；没有这份证明前，不承诺通过“合并几个表”获得安全简化。

## 离线导出的剩余缺口

读取命令已由 `BootstrapLocal` 避开 Docker 与模型客户端，但 `NewFrozenReadPolicy` 仍需打开原 frozen config 中的工具链 lock 路径并比较摘要。因此“没有 Docker 也可导出”与“原工具链文件丢失仍可导出”是两个不同验收目标，后者尚未完成。

可选后续方案：新 run 在冻结配置时持久化完整工具链 lock 的不可变快照，记录 run 与 snapshot Blob 的绑定；快照必须在需要它的执行阶段之前完成可恢复发布。采用原有 `toolchain.Lock.Digest()` 校验工具链身份，另用 Blob digest 校验快照字节，不能假设两者相同。导出用快照重建计划，不读取宿主原路径，也不重新执行 Docker。

历史 run 只在找到与已冻结工具链摘要匹配的 lock 后补录，并通过显式绑定及事务保证不替换原配置；缺失时继续明确拒绝。快照应纳入 run 的 GC 存活关系。此项需要单独的数据模型/迁移设计，不混在无迁移的接口整理中。

## 建议实施顺序与可检查结果

| 步骤 | 范围与交付 | 完成依据 |
|---|---|---|
| R1 | Package/Quality 的命名能力接口、reader 所需存储接口、只读响应与 sandbox 策略；推广 Solution/Data 配置构造 | 单独构造 Package/Quality 不需要上游 Executor、provider transport 或 Engine；真实证据拒绝用例继续通过 |
| R2 | 独立当前流程配置和装配；集中兼容解释；单一固定流程定义与恢复策略 | MVP 主装配不调用 Slice 构造或从 Generation 抽基础资源；旧 revision 不发生隐式升级 |
| R3 | 从 LocalRunService 移交阶段生命周期、typed 分派、提交适配、资源关闭所有权 | 阶段变化不需要改计时/取消；READY 仍原子提交；join/清理/Close 顺序保持 |
| R4 | 用 Package 试点阶段发布能力；收敛普通调用的身份构造与诊断视图 | 阶段只声明发布意图，不接触低层物理调用/预留/writer 协议；原账本身份与计数一致 |
| 单独决策 | frozen lock snapshot；如确有收益，再讨论 publication 专用持久化模型 | 分别提供迁移、历史读取、崩溃恢复与 GC 证明，不绑定前四步完成 |

R1 可从一个纵向切片开始，不要求先重写全体 reader。R2/R3 如出现“必须先搬完所有阶段才能编译”，应缩小迁移单元，用内部适配保持旧实现可运行。每步独立提交，结构调整先不改数据库与摘要契约，便于回退代码；涉及持久化变更的步骤须另行约定回退限制。

关键验证复用现有测试：调度检查暂停、终态和最后有效投影；调用检查 HTTP 次数、原物理身份和预算；reader 检查错 run/attempt、缺失与篡改产物；发布检查封存与提交窗口；生命周期检查取消和精确清理；Package 检查事务中断恢复、READY 绑定及独立导出复验。不能只比较最后一个状态字符串。

实施后按变更范围运行定向测试，再执行项目常规 `go test ./...`、`go vet ./...`、`git diff --check` 和架构检查。生命周期、锁或调度变化增加相应 race 与进程测试；Docker 相关变化需要真实容器验收，fixture 与真实供应商结果分别记录。

## 探索阶段验证与范围

探索阶段仅新增这份方案与文档索引链接，未修改运行时代码。核对了当前实现、已有收敛计划和历史验证记录，并执行以下无缓存定向测试：

```powershell
go test ./internal/application ./internal/cli -run 'Test(CompiledGraph|RunGraph|BootstrapLocal|FrozenReadPolicy|LocalCommands|CancelPoller|CallCoordinator|DispatchCoordinator|LLMReplayProcessCrashBoundaries)' -count=1 -timeout 5m
```

结果：application PASS（3.516s），cli PASS（0.451s）。本次未重跑全量、race、真实 Docker 或真实供应商验收；2026-09-13 文档中的这些结果属于既有记录。测试通过支撑已有机制的当前行为，不代表上述后续改造已经实现。

文档检查：`git diff --check` 通过；架构检查通过（27 份规范文档）；本方案的 7 个本地链接均可解析。
