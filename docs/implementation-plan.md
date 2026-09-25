# MVP 实施计划

状态：ADR-0006 下为当前

当前交付：普通 C++ MVP 的 LOOP-01/SOL-01、DATA-01、JUDGE-01、PKG-01 已完成，初始证据见[题包闭环](evidence/mvp-package-commit-foundation.md)。示例配置已选择 V3，加入[执行样例定稿](design/executed-samples.md)，沿用 V2 每个 run 最多两次的有界内容重生成；冻结的 V1/V2 保持原行为。[V3 稳定性复验](evidence/ready-stability-2026-09-22.md)记录三个真实模型任务通过，第三轮曾因输出超限触发一次 Data 重生成。[V2 CLI 验收](evidence/v2-cli-live-acceptance-2026-09-15.md)保留为未触发重生成的历史正常通路证据。下文保留各切片的交付顺序与历史边界，不能将旧预览的未实现阶段视为当前 MVP 待办。真实查重、SPJ、自动变异、Go 实际闭环与通用不可信包导入仍为后续范围。

## 1. 交付契约

阶段 1 采用每个 run 一个前台执行器、每 run 进程锁、固定流水线，且不设工作流托管服务。SQLite 存储当前 run 与阶段投影以及 CPGen 领域账本。所有外部 I/O 都在写事务之外进行。

每个切片都必须保留较早切片的已完成证据，使用类型化契约与确定性测试，通过仓库门禁，并以可评审的检查点收尾。

## 2. Slice 0：执行基础 —— 已完成

已交付：

- Go 1.24 模块与严格的领域取值；
- Judge 结果基础与确定性优先级；
- 使用显式 argv 的直接 Docker 目标执行；
- 能力检查与类型化的不兼容宿主结果；
- 预算与 CallTrace 证据；
- 确定性的 Docker 计划、资源身份与标签；
- 分离式看门狗、超期、停止、杀死、等待与控制通道 EOF 行为；
- Windows 与 Docker Desktop 探测证据。

完成证据记录在 docs/evidence/slice0-verification.md。Slice 1 既不改变已完成的代码，也不改变该证据。

## 3. Slice 1 轻量本地工作流

目标：交付一个与产品边界相称、可恢复的单宿主前台核心。

### 任务 1：架构契约

- 新增 ADR-0006 并接受轻量设计；
- 修订 ADR-0001、ADR-0002、ADR-0004 与 ADR-0005；
- 对齐架构、详细设计、阶段 1 范围、可追溯性、README 与 TODO；
- 新增并通过可执行的架构一致性检查。

### 任务 2：生命周期取值与进程锁

- 定义严格的 RunState、StageState、StageAttemptState、ReviewDecisionKind 与 ReviewDecisionState；
- 实现由经过验证的 RunID 派生的跨平台操作系统锁；
- 证明同 run 互斥、不同 run 并发，以及进程死亡后的释放。

### 任务 3：SQLite 投影

- 为 runs、stage_records、stage_attempts、run_events、control_requests 与 review_decisions 新增有序迁移；
- 实现期望版本的状态转移，以及投影加事件的原子提交；
- 新增仅用于计量的活动时间记账时间戳；
- 证明评审与取消约束。

### 任务 4：调用与预算账本

- 持久化逻辑操作、物理调用记录、预留、结算与 CallTrace；
- 以事务方式强制所有已配置限制；
- 保持稳定的逻辑幂等性与保守的未知边界处理；
- 证明并发预留不会超支。

### 任务 5：Blob CAS 与 occurrence

- 实现私有的内容寻址 Blob 发布、经验证的读取、声明、写入器令牌、pin 与 occurrence；
- 将每个 occurrence 绑定到 run、阶段尝试、角色、修订与来源证据；
- 证明抗路径穿越、损坏检测、去重、崩溃安全与原子 occurrence 挂载。

### 任务 6：缓存、变异与维护

- 持久化缓存来源调用与制品引用；
- 保留变异与来源记账；
- 将垃圾回收设为排他制品锁下的显式命令；
- 证明缓存命中会创建完整的当前 run 来源信息，且不会与普通工作流使用产生竞态。

### 任务 7：Docker 身份持久化与对账

- 在 Docker create 之前持久化 SandboxExecution 与完整资源计划；
- 授权精确的 RunID、AttemptID、SandboxExecutionID、逻辑操作、范围、计划与引擎身份；
- 保留确定性标签与分离式看门狗；
- 实现一个范围狭窄的对账器，仅限精确的 inspect、stop、kill、wait、remove 与结算；
- 证明绝不触碰无关资源。

### 任务 8：固定的类型化 Fake 流水线

- 组装具体的类型化构造器；
- 实现本地协调器与不可变 RunView；
- 在 SQLite 写事务之外运行外部工作；
- 实现有界的阶段重试、人工恢复、当前阶段重启、评审应用与取消；
- 在题包验证之前保持 READY 不可用。

### 任务 9：本地 CLI 与配置

- 新增严格的本地运行时、存储、锁、记账、提供方与沙箱配置；
- 实现 generate、run list/show/events/resume/cancel 与 review 命令；
- 保持稳定的退出码与 JSON 信封；
- 定义同 run 锁冲突立即失败与进程重启的语义。

### 任务 10：崩溃与 Docker 持久化证明

- 在每个持久化阶段边界注入进程死亡；
- 证明不存在重复的不可逆效应、预算超支、事件重复或制品损坏；
- 测试目标运行期间的 kill、导出前的 stop，以及清理期间的死亡；
- 确认后续对账绝不继续未完成的旧导出。

### 任务 11：边界审计与检查点

- 在支持的环境中运行完整测试、vet、竞态测试、架构检查、Linux 交叉构建与需要 Docker 的门禁；
- 扫描生产源码中的被否决通用运行时机制；
- 更新可追溯性与证据；
- 记录 Slice 1 检查点，且不改变 Slice 0 历史。

### Slice 1 完成标准

- 一个执行器可以在本地创建、暂停、恢复、评审、取消并检查一个 run；
- 竞争的同 run 进程无法执行阶段；
- 进程死亡会释放操作系统锁，重启仅对账当前阶段；
- SQLite 投影、事件、预算、调用、制品、缓存与沙箱资源在崩溃边界保持一致；
- 取消会等待不可信目标已停止的证明；
- Fake 流水线确定性地到达每条暂停与失败路径；
- 所有完整仓库门禁通过。

## 4. Slice 2：Idea、Statement、模型与查重

**历史优先级，2026-09-09（普通题闭环已完成）：** 完成可用的正向生成闭环。有效的查重 ACCEPT 继续进入 Solution；未被接受的业务结果进入人工评审。按照[当前可执行计划](superpowers/plans/2026-09-09-mvp-generation-loop.md)实现 LOOP-01/SOL-01 → DATA-01 → JUDGE-01 → PKG-01。自动变异及其保留来源/授权前置条件推迟以重新设计，不作为 MVP 交付的门禁。现有的 Quality 与 PackageGate 检查仍然必需。

交付类型化的 GenerationRequest、Idea、Statement、模型适配器、提示词注册表、严格结构化输出、查重适配器、证据缓存、策略决策、隐私规则与评审路由。

完成要求确定性的 Fake E2E 覆盖、可选的提供方冒烟测试、阻塞后按当前策略的依赖检查、完整的预算与来源记录，以及不发生题包不安全的内容泄漏。

最初的[架构简化计划](superpowers/plans/2026-09-13-architecture-simplification.md)之后是[最初的 R1–R4 提案](design/architecture-follow-up-2026-09-14.md)，该提案在用户否决后经过返工；当前变更与验证见其[返工记录](evidence/architecture-follow-up-2026-09-14.md)。普通题闭环已实现；下文更宽泛的历史切片里程碑不重新开启该交付。

### 历史库集成检查点（2026-09-08）

这些记录最初的集成。2026-09-13 的 ADR 修订用经过校验的本地循环取代 LangGraphGo，并保留 LangChainGo；当前依赖要求见 go.mod。

1. INT-01/INT-02：修订 ADR-0006 与当前设计；在 Go 1.25.0、最低/当前 CI、导入边界与上游契约探测下固定 LangChainGo v0.1.14 与 LangGraphGo v0.8.5。保留历史 Slice 0/1 证据。
2. LLM-01：通过现有的 MeteredLLM 契约移植提供方适配器，将规范请求与类型化结果同 HTTP 适配器比较，并保留本地严格 schema 与端点策略。
3. LLM-02 至 LLM-06：完成提供方配置、持久化物理派发与重放、一次有界 JSON 修复以及缓存/隐私；通过应用工厂接通真实阶段，并附本地 HTTP 生命周期证据。在声称外部服务验收之前，单独记录可选的对外冒烟测试。
4. WF-01 至 WF-05：在 internal/application 中组装固定的串行图节点，通过现有的 SQLite/Blob 协议提交，保留恢复/评审/取消与修订兼容性，并止步于已实现的切片边界。SQLite 保持权威；不引入 graph.json 或库检查点持久化。
5. 用 Slice 3–5 与完整的八阶段业务图完成 WF-06/WF-07。任何未实现的阶段或库终态结果都不能替代 Judge、Quality 或 PackageGate。

各切片状态由下文链接的验证证据记录。[提供方配置与持久化派发检查点](evidence/slice2-durable-llm-dispatch.md)新增 LLM-02 与 LLM-03a。LLM-03b 实现私有响应发布、经验证的重放与崩溃恢复；[重放证据](evidence/slice2-private-llm-replay.md)记录完整门禁。LLM-04 新增一次已配置的格式修复，带有持久化的脱敏诊断与独立记账；[修复证据](evidence/slice2-bounded-json-repair.md)记录其完整门禁。

LLM-05 完成[私有的同 run 缓存来源](evidence/slice2-private-llm-cache.md)，WF-01 提供[编译后的应用图](evidence/slice2-compiled-graph.md)。LLM-06a 新增[严格内容草稿](evidence/slice2-content-drafts.md)，其身份与冻结资源字段在本地派生。组装过程暴露了长时间模型调用与缓存完成重放中的心跳/版本冲突；WF-04a 通过[按尝试绑定的账本与原始命令回执](evidence/slice2-active-llm-ledger.md)解决。

WF-03a 完成[类型化已提交输入恢复](evidence/slice2-committed-generation-inputs.md)：当前阶段/尝试的来源信息与经验证的私有字节无需提供方请求即可重建语义 Idea/Statement 链，包括在评审失效与缓存复用之后。已接受的实时预览现在组合了按尝试执行、原子回执挂载、显式工作流选择器以及恢复/控制生命周期。

组合状态与剩余边界：

1. WF-02a 的类型化执行器验收已完成，包括在剩余调用预算为零时的缓存复用以及跨活动时间版本的重放。
2. SIM-01 与 SIM-02 通过完整门禁，涵盖持久化查重派发、私有证据/重放、已提交读取以及针对已验证 Statement 链的类型化组合。语义查重输入在下次尝试之前绑定问题/快照、决策与提供方策略、结果上限、精确重试设置与成本上限。传输格式的逻辑身份加入 run 与 attempt；未知的下一次尝试不出现在前一阶段的输出绑定中。HTTP 适配器从请求派生其 Idempotency-Key，因此精确的同尝试重放保持身份不变，而全新的 BLOCKED 尝试会获得新的身份。未来的证据缓存复用仍保持独立。
3. 显式预览选择器与冻结的内容/提供方配置已在生产 Bootstrap 中实现。省略选择器的 Fake 快照被保留；仅提供方配置本身不会选择实时执行。完整的常规 tests/vet、生产与补充生命周期竞态验证、Linux 构建以及架构/格式/补丁检查通过。
4. SIM-02 新增 `slice2.idea.statement.similarity.checkpoint.v1` 与第四个 `slice2_checkpoint` 阶段；现有的三阶段修订不变。成功收集会将私有 occurrence 与策略决策分开返回，因此查重证据在检查点路由之前提交。SIM-03 必须通过真实生命周期应用已提交的 Accept/Review/Reject/Blocked 决策。现有的控制结果 occurrence 限制保持完整。检查点不能产生 READY，也不能豁免未实现的后续门禁。
5. 真实预览服务在调用前、已封存与已完成的提供方边界之间保留精确的 RUNNING 尝试。类型化终态对账在取消或预算耗尽释放之前恢复现有回执。同尝试恢复与终态清理具有不同的授权路径。十五个真实的进程崩溃场景与六个失败/丢失的提交返回在精确尝试、occurrence 与 HTTP 计数下通过常规与竞态验证。
6. BLOCKED 依赖工作现在在记账内部启动。权威的前驱检查点在调用打开之前经受重启，并作为恢复证明绕过历史生成缓存；成功的生成建立当前可用性，并在同一计量操作中提供结果。调用方提供的取消版本保持严格，而记账心跳会在其下一个有界 tick 重试瞬时版本冲突。

集成测试必须分别在提供方完成、私有回执发布与阶段提交之后停止执行；恢复必须在每个边界保持请求/seed/配置身份、HTTP 计数与精确的下一阶段。取消与活动时间预算耗尽必须在最终 run 控制状态转移之前结算同一批持久化调用。

生命周期验收还覆盖阶段成功提交之后、下一次尝试开始之前的间隙。该投影为 RUNNING，当前阶段为 PENDING，且没有活动尝试。WF-04b 新增范围狭窄的终态转移与权威的尝试查找，包括活动服务仍缓存其前驱身份的情形。失败优先的回归与完整门禁通过。终结仍拒绝真正处于 RUNNING 的尝试，并保留较早的已提交历史。

对于提供方恢复，将普通的同尝试延续与终态清理区分开。取消与活动预算耗尽可以重放/结算原始的 DISPATCHING、SENT 或已完成回执，但不能规划新的传输调用、格式修复或缓存查找。未开始的 OPEN/PREPARED 调用需要一条显式的不发送结算路径，且在存在待处理取消时仍然有效。不要让阶段取消在其提供方父项仍处于非终态时释放唯一的已封存响应。

WF-03a 恢复精确的已提交请求与生效 seed，验证当前成功阶段的 occurrence 来源，重建类型化输出并比较其存储的语义 digest。[实时预览组合](evidence/slice2-live-preview.md)通过现有生命周期连接这些读取器、原子提交、冻结配置、新依赖准入与回执清理。其显式检查点对每个保留的查重决策仍为不可豁免的预览。LOOP-01 引入一个兼容的正向修订，直接使用已提交证据：ACCEPT 进入 Solution，其他业务结果进入评审。此前的[变异路由计划](superpowers/plans/2026-09-09-slice2-business-routing.md)是冻结的研究，不是下一工作序列。[开发日志](development-log.md)记录已完成的会话与后来的优先级修订。预览验收不能替代完整 MVP 门禁。

## 5. Slice 3：题解与 Docker Judge

通过 Slice 0 的 Docker 边界交付题解生成、编译/运行集成、参考题解验证、checker/SPJ 支持以及持久化的 Judge 证据。

完成要求权威的目标测量、精确的沙箱身份、看门狗安全、确定性的 Judge 优先级，以及完整的制品与 CallTrace。首个纵向切片生成 Reference/Brute 与说明，在 Docker 中编译并检查样例。内容/正确性失败保留证据并停下来等待评审/失败；自动题解修复推迟。

## 6. Slice 4：数据与质量门禁

交付可复现的测试数据生成、validator、期望输出、oracle 与差分测试、资源限制证据以及最终质量报告。高级变异/对抗测试在首个可用闭环之后进行，不推迟普通的样例、边界、差分或资源检查。

完成要求确定性的失败 seed、策略要求时的最小化证据、严格预算、可复现报告，以及针对模糊质量失败的评审路由。

## 7. Slice 5：题包与端到端验收

交付内部题包暂存、规范 manifest、结构与语义门禁、验证回执、原子题包 occurrence、READY 绑定、导出、导入验证以及完整的 E2E fixture。

完成要求确定性的题包字节、路径安全、崩溃安全的发布、同 run 已验证 occurrence 约束、干净工作区 E2E 与验收证据。

## 8. 每次变更的最低门禁

每次变更先运行聚焦测试，然后：

~~~powershell
go test ./...
go vet ./...
git diff --check
~~~

边界或并发变更还要运行 `go test -race -timeout 30m ./...`。显式的套件超时用于容纳插桩后的 SQLite 迁移与崩溃/重放覆盖；2026-09-09 扩展后的应用扫描在 1199.150 秒通过，过于接近此前 20 分钟的包限制。操作级的 HTTP、锁与辅助超时仍各自独立受限。Docker 变更在兼容环境中运行真实引擎安全测试。文档变更运行 scripts/check-slice1-architecture.ps1。

## 9. 推迟的工作

根据 2026-09-09 的用户修订，自动 Idea 变异、自动业务题解修复以及旧的 BR-01b/BR-02–BR-05 变异来源/授权链均被推迟。已完成的契约与迁移证据仍为历史资产，不是正向路径的必需依赖。只有在存在可用的产包闭环之后，才重新考虑更简单的变异设计。

远程执行、多宿主协调、常开定时器、运维式工作流搜索、任意插件图以及 web/API 控制仍不在 MVP 范围内。任何此类扩展都需要单独的架构决策与迁移计划。

## 契约短语（canonical contract phrases）

架构检查脚本以这些英文短语作为契约锚点：

- one foreground executor per run
- per-run process lock
- fixed pipeline
- no workflow-hosting service
