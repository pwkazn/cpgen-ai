# 阶段 1 MVP 实施设计

Status: Accepted

Date: 2026-08-31

## 1. 目标与权威来源

阶段 1 构建一条从 GenerationRequest 到已验证题包的可审计本地 CPGen 路径。运行时契约是每个 run 一个前台执行器、每 run 一个进程锁、一条固定流水线，且无工作流托管服务。

权威来源是 ADR-0006、轻量本地工作流设计、ARCHITECTURE.md、当前 ADR 与各项详细设计。已完成的 Slice 0 证据仍然有效，且不被 Slice 1 重写。

## 2. 范围

2026-09-09 来自用户的范围修正：优先处理 Similarity ACCEPT → Solution → Data → Docker/Judge → Quality → Package，以及未被接受的业务结果 → 人工评审。自动变更与业务修复推迟到可用闭环之后重新设计。既有的变更账本/溯源描述仍是历史契约，而非 MVP 交付前提。遵循[当前执行计划](../plans/2026-09-09-mvp-generation-loop.md)。

MVP 包含：

- 严格的请求、配置与领域值；
- 类型化的 Idea、Statement、Similarity、Solution、Data、Judge、Quality 与 Package 阶段；
- 在单台宿主上的前台 CLI 执行；
- SQLite 当前投影与追加式审计事件；
- 用于预算、调用、制品、缓存、沙箱、评审与题包的 CPGen 账本；
- 带分离式看门狗的直接 Docker 目标执行；
- provider 中立的模型与查重适配器；
- 不可变 Blob 存储与以 run 为范围的 occurrence；
- 人工评审、有界阶段重试、人工恢复与取消；
- 确定性题包门禁与原子 READY 绑定。

不在范围内的是远程 worker、守护进程、任务队列、任意运行时图、跨宿主检查点、web/API 控制或无人值守定时器。

## 3. 组件边界

### CLI

校验本地配置与请求，调用应用服务，渲染稳定输出，并将类型化结果映射为退出码。

### 本地协调器

获取由操作系统支撑的 run 锁，选择当前已编译阶段，记录尝试与预留，在写事务之外执行外部工作，提交已验证结果，并在暂停或终态处退出。

### 类型化阶段

接收不可变的 RunView、复制的类型化输入与最小计量端口。它们绝不接收持久化、锁、裸 Docker、不受限的制品写入或可变的 run 状态。

### 适配器

SQLite、文件系统、模型、查重、Docker、时钟与 ID 适配器实现窄端口。确定性的 Fake 是默认的测试实现。

### Docker 安全边界

Runner 在 Docker create 之前持久化 SandboxExecution 及其完整资源计划。确定性身份与分离式看门狗确保 CLI 失效后目标停止。后续命令仅对账精确的持久化资源。

## 4. 数据流

1. 解析并严格校验 GenerationRequest 与 ApplicationConfig。
2. 规范化已脱敏的配置与请求摘要。
3. 原子地创建 run、已编译阶段投影与首个事件。
4. 获取 run 锁并选择当前阶段。
5. 创建或重放阶段尝试与稳定的逻辑幂等键。
6. 预留预算与效果记录。
7. 在数据库写事务之外调用类型化阶段与外部适配器。
8. 校验响应、评测证据与制品字节。
9. 在共享 SQLite 的范围内原子地结算 CallTrace、预算、occurrence 与阶段结果。
10. 继续固定的阶段序列，或在 BLOCKED、NEEDS_REVIEW、READY、FAILED 或 CANCELLED 处退出。
11. 在 Package 阶段，校验暂存树，并在 READY 之前原子地绑定同一 run 的已验证 occurrence 与最终质量报告。

## 5. 状态与重启

Run 状态为 CREATED、RUNNING、BLOCKED、NEEDS_REVIEW、READY、FAILED 与 CANCELLED。

阶段尝试是追加式审计。进程死亡后，操作系统释放锁。人工恢复从持久化账本对账当前阶段：

- 在未授权不可逆效果时重跑；
- 查询或重放同一 provider 幂等身份；
- 保守地结算未知发送边界；
- 校验已发布的 Blob 字节与写入器令牌；
- 停止并清理精确的 Docker 资源；
- 如果已完成的结果已经提交，则正常推进。

BLOCKED 恢复会为同一阶段创建全新尝试，并首先通过其常规计量端口与当前策略重新校验检查点依赖。仅凭历史健康数据不能恢复工作。

取消是一条幂等控制请求。在每个不可信目标被证明已停止之前，CANCELLED 不能提交。

## 6. 持久化

投影表为 runs、stage_records、stage_attempts、run_events、control_requests 与 review_decisions。

领域账本包括：

- budget_accounts、逻辑操作、调用记录与预留；
- 声明、写入器令牌、Blob、pin 与 occurrence；
- 缓存条目、来源调用、Blob 引用与当前 run 使用；
- 变更与溯源记录；
- 沙箱执行、资源与证据；
- 题包、题包 occurrence、校验回执与质量报告。

每个改变状态的操作都检查期望版本并使用短事务。事件是审计，而不是控制流重放来源。

## 7. Slice 交付

### Slice 0：执行基础 —— 已完成

已交付严格的领域值、评测基础、直接 Docker 执行、宿主能力结果、CallTrace 与预算证据、确定性资源身份、分离式看门狗与校验证据。

### Slice 1：轻量本地核心

交付架构对账、生命周期值、跨平台 run 锁、SQLite 投影、调用与预算账本、Blob 与 occurrence 存储、缓存与显式维护、Docker 身份持久化与窄范围对账、固定类型化 Fake 流水线、CLI 命令与崩溃测试。

### Slice 2：idea、statement、model、similarity

交付请求与内容契约、prompt 注册表、严格结构化输出、provider 适配器、查重证据与策略、缓存、隐私与评审路由。

### Slice 3：solution 与评测

交付题解生成、编译/运行集成、checker 与 SPJ 支持、目标测量与持久化的评测证据。

### Slice 4：数据与质量

交付数据生成、校验、期望输出、差分检查、变更证据、资源限制与最终质量报告。

### Slice 5：题包与 E2E

交付规范的内部题包、结构与语义门禁、校验回执、原子 READY 绑定、导出/导入校验与完整 E2E 证据。

## 8. 错误处理

类型化错误对无效输入、依赖不可用、宿主不兼容、可重试传输、未知发送状态、预算耗尽、需要评审、取消、永久阶段失败与内部损坏进行分类。

重试在前台阶段内有界。退避不会创建无人值守定时器。未知边界保留原始身份或保守地暂停。取消之后，制品与 Docker 清理保持幂等。

## 9. 测试

必需的证明包括：

- 转移表与已编译的类型化边界；
- 双进程同 run 互斥与进程死亡释放；
- 原子投影加事件与期望版本冲突；
- 在每个持久阶段边界处崩溃；
- 稳定的逻辑身份与物理 CallTrace；
- 预算并发与保守结算；
- Blob 安全、损坏、去重与 GC 互斥；
- 缓存来源与当前 run 溯源；
- 真实 Docker kill、看门狗 EOF 与精确资源对账；
- 评审与取消生命周期；
- 题包门禁完整性与同一 run 的 READY；
- 完整的 Go 测试、vet、竞态测试、2026-09-08 ADR-0006 修正案下的 Go 1.25.0 兼容性、Linux 交叉构建与架构一致性。

## 10. 验收

只有当干净工作区能够通过已编译流水线生成或确定性地模拟一个请求、经受注入的进程死亡、在无重复效果的情况下恢复当前阶段、安全停止不可信目标、产出已验证题包，并从持久化证据复现结果时，阶段 1 才算完成。

## 契约短语（canonical contract phrases）

架构检查脚本以这些英文短语作为契约锚点：

- one foreground executor per run
- per-run process lock
- fixed pipeline
- no workflow-hosting service
